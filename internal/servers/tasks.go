package servers

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Scheduled tasks: things a server does by itself on a schedule, like
// Crafty's schedules. A task runs console commands, restarts, starts, stops
// or backs the server up, every so often, at set times on chosen days, on a
// cron schedule, or a while after another task ran (a chain: warn, then
// restart). Restarts and stops can warn players first, and any task can be
// skipped while someone is playing.
//
// Tasks are kept in the server's details. When a task is next due is worked
// out from when it last ran, so a panel restart doesn't run one twice; runs
// missed while the panel was off are skipped, not made up.

// Task is one scheduled task.
type Task struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	// Action: "command", "restart", "start", "stop" or "backup".
	Action string `json:"action"`
	// Command: console commands, one per line (Action "command").
	Command  string       `json:"command,omitempty"`
	Schedule TaskSchedule `json:"schedule"`
	// Warn: minutes of warning players get before a restart or stop (0: none).
	Warn int `json:"warn,omitempty"`
	// SkipIfPlaying: don't run while someone is on the server.
	SkipIfPlaying bool      `json:"skipIfPlaying,omitempty"`
	LastRun       time.Time `json:"lastRun,omitzero"`
	LastResult    string    `json:"lastResult,omitempty"`
}

// TaskSchedule is when a task runs.
type TaskSchedule struct {
	// Kind: "every" (Every minutes), "daily" (At on Days), "cron" (Cron) or
	// "after" (Delay minutes after task After runs).
	Kind  string `json:"kind"`
	Every int    `json:"every,omitempty"`
	At    string `json:"at,omitempty"`   // "15:04"
	Days  []int  `json:"days,omitempty"` // 0 Sunday … 6 Saturday; none: every day
	Cron  string `json:"cron,omitempty"`
	After string `json:"after,omitempty"`
	Delay int    `json:"delay,omitempty"`
}

// TaskView is a task as the page shows it.
type TaskView struct {
	Task
	Next    *time.Time `json:"next,omitempty"`
	Running bool       `json:"running,omitempty"`
}

const (
	maxTasks    = 50
	taskTick    = 15 * time.Second
	maxTaskWarn = 30
)

var (
	reTaskClock = regexp.MustCompile(`^([01]?\d|2[0-3]):([0-5]\d)$`)
	taskAction  = map[string]bool{"command": true, "restart": true, "start": true, "stop": true, "backup": true}
)

// taskState is what the scheduler keeps in memory.
type taskState struct {
	mu      sync.Mutex
	next    map[string]time.Time // server/task -> when it's due
	running map[string]bool
	started time.Time
	wake    chan struct{}
}

func newTaskState() *taskState {
	return &taskState{next: map[string]time.Time{}, running: map[string]bool{}, started: time.Now(), wake: make(chan struct{}, 1)}
}

func taskKey(serverID, taskID string) string { return serverID + "/" + taskID }

// ---- schedules ----

// check says what's wrong with a schedule, if anything. tasks are the
// server's other tasks (for chains).
func (sc TaskSchedule) check(self string, tasks []Task) error {
	switch sc.Kind {
	case "every":
		if sc.Every < 1 || sc.Every > 7*24*60 {
			return errors.New("choose how often: between 1 minute and 1 week")
		}
	case "daily":
		if !reTaskClock.MatchString(sc.At) {
			return errors.New("choose a time of day")
		}
		for _, d := range sc.Days {
			if d < 0 || d > 6 {
				return errors.New("unknown day")
			}
		}
	case "cron":
		if _, err := parseCron(sc.Cron); err != nil {
			return err
		}
	case "after":
		if sc.Delay < 0 || sc.Delay > 24*60 {
			return errors.New("the wait after the other task must be between 0 minutes and a day")
		}
		byID := map[string]Task{}
		for _, t := range tasks {
			byID[t.ID] = t
		}
		// Follow the chain back: it must reach a task with its own schedule.
		seen := map[string]bool{self: true}
		for id := sc.After; ; {
			t, ok := byID[id]
			if !ok {
				return errors.New("choose which task this one follows")
			}
			if seen[id] {
				return errors.New("tasks can't follow each other in a circle")
			}
			seen[id] = true
			if t.Schedule.Kind != "after" {
				break
			}
			id = t.Schedule.After
		}
	default:
		return errors.New("choose when the task runs")
	}
	return nil
}

// nextAfter is the first time after t the schedule is due (zero for
// "after", which runs when its task does).
func (sc TaskSchedule) nextAfter(t time.Time) time.Time {
	switch sc.Kind {
	case "every":
		return t.Add(time.Duration(sc.Every) * time.Minute)
	case "daily":
		m := reTaskClock.FindStringSubmatch(sc.At)
		if m == nil {
			return time.Time{}
		}
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
		for i := 0; i < 9; i++ {
			d := day.AddDate(0, 0, i)
			at := time.Date(d.Year(), d.Month(), d.Day(), h, mi, 0, 0, t.Location())
			if at.After(t) && dayChosen(sc.Days, at.Weekday()) {
				return at
			}
		}
	case "cron":
		if c, err := parseCron(sc.Cron); err == nil {
			return c.next(t)
		}
	}
	return time.Time{}
}

func dayChosen(days []int, d time.Weekday) bool {
	if len(days) == 0 {
		return true
	}
	for _, x := range days {
		if x == int(d) {
			return true
		}
	}
	return false
}

// cronSpec is a five-field cron schedule: minute hour day month weekday.
type cronSpec struct {
	min, hour, dom, month, dow uint64 // bit sets
	domAny, dowAny             bool
}

func parseCron(s string) (cronSpec, error) {
	f := strings.Fields(s)
	if len(f) != 5 {
		return cronSpec{}, errors.New("a cron schedule has five parts: minute hour day month weekday, e.g. 0 4 * * *")
	}
	var c cronSpec
	var err error
	bounds := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
	sets := []*uint64{&c.min, &c.hour, &c.dom, &c.month, &c.dow}
	names := []string{"minute", "hour", "day", "month", "weekday"}
	for i, part := range f {
		if *sets[i], err = cronField(part, bounds[i][0], bounds[i][1]); err != nil {
			return cronSpec{}, fmt.Errorf("the %s part %q: %w", names[i], part, err)
		}
	}
	if c.dow&(1<<7) != 0 { // 7 is Sunday too
		c.dow |= 1
	}
	c.domAny, c.dowAny = f[2] == "*", f[4] == "*"
	return c, nil
}

func cronField(s string, lo, hi int) (uint64, error) {
	var set uint64
	for _, item := range strings.Split(s, ",") {
		rng, stepStr, hasStep := strings.Cut(item, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 {
				return 0, errors.New("bad step")
			}
			step = n
		}
		a, b := lo, hi
		if rng != "*" {
			x, y, isRange := strings.Cut(rng, "-")
			var err error
			if a, err = strconv.Atoi(x); err != nil {
				return 0, errors.New("not a number")
			}
			b = a
			if isRange {
				if b, err = strconv.Atoi(y); err != nil {
					return 0, errors.New("not a number")
				}
			} else if hasStep {
				b = hi
			}
		}
		if a < lo || b > hi || a > b {
			return 0, fmt.Errorf("must be between %d and %d", lo, hi)
		}
		for v := a; v <= b; v += step {
			set |= 1 << uint(v)
		}
	}
	return set, nil
}

// next is the first minute after t the schedule matches.
func (c cronSpec) next(t time.Time) time.Time {
	t = t.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if c.month&(1<<uint(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
			continue
		}
		if !c.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
			continue
		}
		if c.hour&(1<<uint(t.Hour())) == 0 {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, t.Location())
			continue
		}
		if c.min&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

// dayMatches: like cron, when both the day and the weekday are limited,
// either one will do.
func (c cronSpec) dayMatches(t time.Time) bool {
	dom := c.dom&(1<<uint(t.Day())) != 0
	dow := c.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case c.domAny && c.dowAny:
		return true
	case c.domAny:
		return dow
	case c.dowAny:
		return dom
	}
	return dom || dow
}

// ---- the server's tasks ----

// Tasks returns the server's tasks with when each runs next.
func (s *Server) Tasks() []TaskView {
	s.mu.Lock()
	tasks := append([]Task(nil), s.meta.Tasks...)
	s.mu.Unlock()
	ts := s.m.tasks
	out := make([]TaskView, 0, len(tasks))
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for _, t := range tasks {
		v := TaskView{Task: t, Running: ts.running[taskKey(s.ID(), t.ID)]}
		if n, ok := ts.next[taskKey(s.ID(), t.ID)]; ok && t.Enabled && !n.IsZero() {
			v.Next = &n
		} else if t.Enabled && t.Schedule.Kind != "after" {
			n := t.Schedule.nextAfter(laterOf(t.LastRun, time.Now()))
			if !n.IsZero() {
				v.Next = &n
			}
		}
		out = append(out, v)
	}
	return out
}

func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func newTaskID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SaveTask adds a task (an empty ID) or changes one, and returns it.
func (s *Server) SaveTask(t Task) (Task, error) {
	t.Name = strings.TrimSpace(t.Name)
	t.Command = strings.TrimSpace(strings.ReplaceAll(t.Command, "\r", ""))
	if t.Name == "" || len(t.Name) > 60 {
		return Task{}, errors.New("give the task a name (up to 60 characters)")
	}
	if !taskAction[t.Action] {
		return Task{}, errors.New("choose what the task does")
	}
	if t.Action == "command" {
		lines := strings.Split(t.Command, "\n")
		if t.Command == "" || len(lines) > 20 || len(t.Command) > 4000 {
			return Task{}, errors.New("type the commands to run, one per line (up to 20)")
		}
	} else {
		t.Command = ""
	}
	if t.Warn < 0 || t.Warn > maxTaskWarn {
		return Task{}, fmt.Errorf("the warning can be up to %d minutes", maxTaskWarn)
	}
	if t.Action != "restart" && t.Action != "stop" {
		t.Warn = 0
	}
	sc := t.Schedule
	if sc.Kind != "every" {
		sc.Every = 0
	}
	if sc.Kind != "daily" {
		sc.At, sc.Days = "", nil
	}
	if sc.Kind != "cron" {
		sc.Cron = ""
	}
	sc.Cron = strings.Join(strings.Fields(sc.Cron), " ")
	if sc.Kind != "after" {
		sc.After, sc.Delay = "", 0
	}
	t.Schedule = sc

	s.mu.Lock()
	found := -1
	for i, x := range s.meta.Tasks {
		if t.ID != "" && x.ID == t.ID {
			found = i
		}
	}
	switch {
	case t.ID != "" && found < 0:
		s.mu.Unlock()
		return Task{}, errors.New("that task is gone; reload the page")
	case t.ID == "" && len(s.meta.Tasks) >= maxTasks:
		s.mu.Unlock()
		return Task{}, fmt.Errorf("a server can have up to %d tasks", maxTasks)
	case t.ID == "":
		t.ID = newTaskID()
	}
	if err := sc.check(t.ID, s.meta.Tasks); err != nil {
		s.mu.Unlock()
		return Task{}, err
	}
	if found >= 0 {
		t.LastRun, t.LastResult = s.meta.Tasks[found].LastRun, s.meta.Tasks[found].LastResult
		s.meta.Tasks[found] = t
	} else {
		s.meta.Tasks = append(s.meta.Tasks, t)
	}
	s.mu.Unlock()
	s.m.tasks.forget(s.ID(), t.ID)
	return t, s.saveMeta()
}

// DeleteTask removes a task. Tasks that followed it stop running until
// they're given another.
func (s *Server) DeleteTask(id string) error {
	s.mu.Lock()
	kept := s.meta.Tasks[:0:0]
	found := false
	for _, t := range s.meta.Tasks {
		if t.ID == id {
			found = true
			continue
		}
		if t.Schedule.Kind == "after" && t.Schedule.After == id {
			t.Enabled = false
		}
		kept = append(kept, t)
	}
	s.meta.Tasks = kept
	s.mu.Unlock()
	if !found {
		return errors.New("no such task")
	}
	s.m.tasks.forget(s.ID(), id)
	return s.saveMeta()
}

// RunTaskNow runs a task straight away (and the tasks that follow it).
func (s *Server) RunTaskNow(id string) error {
	t, ok := s.task(id)
	if !ok {
		return errors.New("no such task")
	}
	if !s.m.tasks.begin(s.ID(), id) {
		return errors.New("that task is running already")
	}
	go s.runTask(t, "now")
	return nil
}

func (s *Server) task(id string) (Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.meta.Tasks {
		if t.ID == id {
			return t, true
		}
	}
	return Task{}, false
}

func (ts *taskState) forget(serverID, taskID string) {
	ts.mu.Lock()
	delete(ts.next, taskKey(serverID, taskID))
	ts.mu.Unlock()
}

// begin marks a task running; false if it already is.
func (ts *taskState) begin(serverID, taskID string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	k := taskKey(serverID, taskID)
	if ts.running[k] {
		return false
	}
	ts.running[k] = true
	return true
}

func (ts *taskState) end(serverID, taskID string) {
	ts.mu.Lock()
	delete(ts.running, taskKey(serverID, taskID))
	ts.mu.Unlock()
}

// ---- running ----

// RunTasks runs scheduled tasks until the panel shuts down.
func (m *Manager) RunTasks() {
	go func() {
		t := time.NewTicker(taskTick)
		defer t.Stop()
		for {
			m.taskPass(time.Now())
			select {
			case <-m.ctx.Done():
				return
			case <-t.C:
			case <-m.tasks.wake:
			}
		}
	}()
}

// taskPass starts the tasks that are due at now.
func (m *Manager) taskPass(now time.Time) {
	ts := m.tasks
	for _, s := range m.all() {
		s.mu.Lock()
		tasks := append([]Task(nil), s.meta.Tasks...)
		gone := s.deleted
		s.mu.Unlock()
		if gone {
			continue
		}
		for _, t := range tasks {
			k := taskKey(s.ID(), t.ID)
			ts.mu.Lock()
			next, known := ts.next[k]
			if !known && t.Schedule.Kind != "after" {
				// From when it last ran, or from when the panel started:
				// runs missed while the panel was off are skipped.
				next = t.Schedule.nextAfter(laterOf(t.LastRun, ts.started.Add(-time.Minute)))
				if next.Before(ts.started) {
					next = t.Schedule.nextAfter(now)
				}
				ts.next[k] = next
			}
			due := t.Enabled && !next.IsZero() && !now.Before(next) && !ts.running[k]
			if due {
				ts.running[k] = true
				if t.Schedule.Kind == "after" {
					delete(ts.next, k)
				} else {
					ts.next[k] = t.Schedule.nextAfter(now)
				}
			}
			ts.mu.Unlock()
			if due && m.ctx.Err() == nil {
				go s.runTask(t, "schedule")
			}
		}
	}
}

// runTask does a task and records what happened. The caller has marked it
// running.
func (s *Server) runTask(t Task, why string) {
	defer s.m.tasks.end(s.ID(), t.ID)
	result := s.doTask(t)
	now := time.Now()
	s.mu.Lock()
	var followers []Task
	for i := range s.meta.Tasks {
		x := &s.meta.Tasks[i]
		if x.ID == t.ID {
			x.LastRun, x.LastResult = now, result
		}
		if x.Enabled && x.Schedule.Kind == "after" && x.Schedule.After == t.ID {
			followers = append(followers, *x)
		}
	}
	s.mu.Unlock()
	_ = s.saveMeta()
	s.say("Task \"" + t.Name + "\": " + result)
	s.m.log.Info("scheduled task ran", "id", s.ID(), "task", t.Name, "why", why, "result", result)
	// Tasks that follow this one run after their wait, unless it was skipped.
	if strings.HasPrefix(result, "skipped") {
		return
	}
	ts := s.m.tasks
	ts.mu.Lock()
	for _, f := range followers {
		ts.next[taskKey(s.ID(), f.ID)] = now.Add(time.Duration(f.Schedule.Delay) * time.Minute)
	}
	ts.mu.Unlock()
	if len(followers) > 0 {
		select {
		case ts.wake <- struct{}{}:
		default:
		}
	}
}

// doTask does what a task says and describes how it went.
func (s *Server) doTask(t Task) string {
	s.mu.Lock()
	running := s.cmd != nil && s.state == StateRunning
	playing := len(s.players)
	s.mu.Unlock()
	if t.SkipIfPlaying && playing > 0 {
		return fmt.Sprintf("skipped: %d playing", playing)
	}
	switch t.Action {
	case "command":
		if !running {
			return "skipped: the server isn't running"
		}
		lines := strings.Split(t.Command, "\n")
		for i, line := range lines {
			if strings.TrimSpace(line) == "" {
				continue
			}
			if i > 0 {
				time.Sleep(time.Second)
			}
			if err := s.Command(line); err != nil {
				return "failed: " + err.Error()
			}
		}
		if len(lines) == 1 {
			return "ran " + lines[0]
		}
		return fmt.Sprintf("ran %d commands", len(lines))
	case "restart", "stop":
		if !running {
			return "skipped: the server isn't running"
		}
		verb := map[string]string{"restart": "restarts", "stop": "stops"}[t.Action]
		if !s.warnPlayers(t.Warn, verb) {
			return "skipped: the panel is shutting down"
		}
		if t.Action == "stop" {
			if err := s.Stop(); err != nil {
				return "failed: " + err.Error()
			}
			return "stopped the server"
		}
		if err := s.Restart(); err != nil {
			return "failed: " + err.Error()
		}
		return "restarted the server"
	case "start":
		if running || s.isBusy() {
			return "skipped: the server is already on"
		}
		if why := s.ClosedNow(); why != "" {
			return "skipped: " + why
		}
		if err := s.Start(); err != nil {
			return "failed: " + err.Error()
		}
		return "started the server"
	case "backup":
		if err := s.StartBackup(); err != nil {
			return "failed: " + err.Error()
		}
		return "started a backup"
	}
	return "failed: unknown action"
}

// warnPlayers tells anyone playing the server restarts or stops in warn
// minutes, again a minute before, then waits. With nobody on it doesn't
// wait. It returns false if the panel shut down meanwhile.
func (s *Server) warnPlayers(warn int, verb string) bool {
	if warn <= 0 {
		return true
	}
	s.mu.Lock()
	playing := len(s.players)
	s.mu.Unlock()
	if playing == 0 {
		return true
	}
	say := func(left int) {
		unit := "minutes"
		if left == 1 {
			unit = "minute"
		}
		_ = s.Command(fmt.Sprintf("say The server %s in %d %s.", verb, left, unit))
	}
	say(warn)
	wait := func(d time.Duration) bool {
		select {
		case <-s.m.ctx.Done():
			return false
		case <-time.After(d):
			return true
		}
	}
	if warn > 1 {
		if !wait(time.Duration(warn-1) * taskMinute) {
			return false
		}
		say(1)
	}
	return wait(taskMinute)
}

// taskMinute is a minute of warning; tests shorten it.
var taskMinute = time.Minute
