package servers

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Play time: what the family's kids play, and when they may.
//
//   - Play history: every visit to a server (who, which server, from when
//     to when), kept for historyDays.
//   - Server hours: a server can be open only at certain times. Players are
//     warned in the game, the server stops (saving the world) at closing
//     time, and starts again when it opens. The owner can keep it open
//     later for one night.
//   - Kids: the owner lists each kid with the names they play as (gamertag,
//     Java username, Hytale username). A kid can have a daily time limit
//     and hours they may play, across every server. They're warned, then
//     moved off the server when their time is up. The owner can give extra
//     time for the day.
//
// Everything is off until the owner turns it on.

// historyDays is how long play history is kept.
const historyDays = 62

// playTick is how often hours and limits are checked. Tests shorten it.
var playTick = 15 * time.Second

// warnings are the minutes-left at which players are warned.
var warnings = []int{10, 5, 1}

// Session is one visit to a server.
type Session struct {
	Server string    `json:"server"` // server ID
	Name   string    `json:"name"`   // player name as the server knows it
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
}

// playHistory keeps sessions in <data>/play-history.json.
type playHistory struct {
	path     string
	mu       sync.Mutex
	saveMu   sync.Mutex
	sessions []Session
}

func newPlayHistory(dataDir string) *playHistory {
	h := &playHistory{path: filepath.Join(dataDir, "play-history.json")}
	if b, err := os.ReadFile(h.path); err == nil {
		_ = json.Unmarshal(b, &h.sessions)
	}
	h.prune(time.Now())
	return h
}

func (h *playHistory) prune(now time.Time) {
	cut := now.AddDate(0, 0, -historyDays)
	h.mu.Lock()
	kept := h.sessions[:0]
	for _, x := range h.sessions {
		if x.End.After(cut) {
			kept = append(kept, x)
		}
	}
	h.sessions = kept
	h.mu.Unlock()
}

// add records a finished session. It only touches memory: save writes the
// file (callers may hold a server's lock).
func (h *playHistory) add(x Session) {
	if x.Name == "" || !x.End.After(x.Start) {
		return
	}
	h.mu.Lock()
	h.sessions = append(h.sessions, x)
	h.mu.Unlock()
}

func (h *playHistory) save() {
	h.saveMu.Lock()
	defer h.saveMu.Unlock()
	h.prune(time.Now())
	h.mu.Lock()
	b, err := json.Marshal(h.sessions)
	h.mu.Unlock()
	if err != nil {
		return
	}
	if err := os.WriteFile(h.path+".tmp", b, 0o644); err == nil {
		_ = os.Rename(h.path+".tmp", h.path)
	}
}

func (h *playHistory) since(t time.Time) []Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Session
	for _, x := range h.sessions {
		if x.End.After(t) {
			out = append(out, x)
		}
	}
	return out
}

// playerLeftLocked records the end of a player's visit and forgets them.
// Caller holds s.mu.
func (s *Server) playerLeftLocked(name string) {
	p, ok := s.players[name]
	if !ok {
		return
	}
	delete(s.players, name)
	s.m.history.add(Session{Server: s.meta.ID, Name: p.Name, Start: p.Since, End: time.Now()})
	s.m.saves.Add(1)
	go func() {
		defer s.m.saves.Done()
		s.m.history.save()
	}()
}

// everyoneLeftLocked ends every visit (the server stopped). Caller holds
// s.mu.
func (s *Server) everyoneLeftLocked() {
	for name := range s.players {
		s.playerLeftLocked(name)
	}
	s.players = map[string]Player{}
}

// ---- hours ----

// Span is a part of a day: from From until Until ("07:00", "21:00").
type Span struct {
	From  string `json:"from"`
	Until string `json:"until"`
}

// Hours are when play is allowed: one span for school nights (Sunday to
// Thursday) and one for weekend nights (Friday and Saturday), named for
// the night because bedtime is what changes.
type Hours struct {
	On      bool `json:"on"`
	School  Span `json:"school"`
	Weekend Span `json:"weekend"`
}

// DefaultHours are offered when hours are first turned on.
var DefaultHours = Hours{School: Span{"07:00", "21:00"}, Weekend: Span{"08:00", "22:00"}}

func (sp Span) check() error {
	fh, fm, ok1 := clock(sp.From)
	uh, um, ok2 := clock(sp.Until)
	if !ok1 || !ok2 {
		return errors.New("pick times like 07:00 and 21:00")
	}
	if uh*60+um <= fh*60+fm {
		return errors.New("the end has to be later in the day than the start")
	}
	return nil
}

func (h Hours) clean() (Hours, error) {
	if h.School.From == "" && h.School.Until == "" {
		h.School = DefaultHours.School
	}
	if h.Weekend.From == "" && h.Weekend.Until == "" {
		h.Weekend = DefaultHours.Weekend
	}
	for _, sp := range []*Span{&h.School, &h.Weekend} {
		if err := sp.check(); err != nil {
			return h, err
		}
		fh, fm, _ := clock(sp.From)
		uh, um, _ := clock(sp.Until)
		sp.From, sp.Until = fmt.Sprintf("%02d:%02d", fh, fm), fmt.Sprintf("%02d:%02d", uh, um)
	}
	return h, nil
}

// weekendNight: Friday and Saturday nights have the weekend's hours.
func weekendNight(t time.Time) bool {
	return t.Weekday() == time.Friday || t.Weekday() == time.Saturday
}

// weekendDay: Saturday and Sunday have the weekend's time limit.
func weekendDay(t time.Time) bool { return t.Weekday() == time.Saturday || t.Weekday() == time.Sunday }

func at(day time.Time, hhmm string) time.Time {
	h, m, _ := clock(hhmm)
	return time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, day.Location())
}

func midnight(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// span returns the day's open span.
func (h Hours) span(day time.Time) (time.Time, time.Time) {
	sp := h.School
	if weekendNight(day) {
		sp = h.Weekend
	}
	return at(day, sp.From), at(day, sp.Until)
}

// window says whether now is within the hours, and when the current open
// span ends (if open) or the next one starts (if not).
func (h Hours) window(now time.Time) (open bool, closes, opens time.Time) {
	day := midnight(now)
	from, until := h.span(day)
	switch {
	case now.Before(from):
		return false, time.Time{}, from
	case now.Before(until):
		return true, until, time.Time{}
	}
	next, _ := h.span(day.AddDate(0, 0, 1))
	return false, time.Time{}, next
}

// HoursStatus is how a server stands with its hours, for the page.
type HoursStatus struct {
	Hours
	Open bool `json:"open"`
	// Closes is when it closes (open), Opens when it opens next (closed).
	Closes *time.Time `json:"closes,omitempty"`
	Opens  *time.Time `json:"opens,omitempty"`
	// Late is set while the owner has kept it open past its hours.
	Late *time.Time `json:"late,omitempty"`
}

// hoursStatusLocked works out the server's hours now. Caller holds s.mu.
func (s *Server) hoursStatusLocked(now time.Time) *HoursStatus {
	h := s.meta.Hours
	if h == nil || !h.On {
		return nil
	}
	open, closes, opens := h.window(now)
	st := &HoursStatus{Hours: *h, Open: open}
	if late := s.meta.OpenLate; late.After(now) && (!open || late.After(closes)) {
		st.Open, closes, opens = true, late, time.Time{}
		st.Late = &late
	}
	if st.Open {
		st.Closes = &closes
	} else {
		st.Opens = &opens
	}
	return st
}

// HoursNow reports the server's hours now, or nil when it has none.
func (s *Server) HoursNow() *HoursStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hoursStatusLocked(time.Now())
}

// SetHours sets when the server is open. Turning hours off forgets a late
// night.
func (s *Server) SetHours(h Hours) error {
	if h.On {
		var err error
		if h, err = h.clean(); err != nil {
			return err
		}
	}
	s.mu.Lock()
	if !h.On && s.meta.Hours != nil {
		h.School, h.Weekend = s.meta.Hours.School, s.meta.Hours.Weekend // remembered for next time
	}
	s.meta.Hours = &h
	if !h.On {
		s.meta.OpenLate = time.Time{}
	}
	reopen := !h.On && s.meta.ClosedForHours
	s.mu.Unlock()
	if err := s.saveMeta(); err != nil {
		return err
	}
	if reopen {
		s.m.playtime.kick() // start it again now, if it was closed for the night
	}
	return nil
}

// KeepOpenUntil keeps the server open past its hours until t (the zero
// time ends that). The server starts if it was closed for the night.
func (s *Server) KeepOpenUntil(t time.Time) error {
	s.mu.Lock()
	if s.meta.Hours == nil || !s.meta.Hours.On {
		s.mu.Unlock()
		return errors.New("this server has no hours set")
	}
	if !t.IsZero() && !t.After(time.Now()) {
		s.mu.Unlock()
		return errors.New("pick a time later than now")
	}
	s.meta.OpenLate = t
	s.mu.Unlock()
	if err := s.saveMeta(); err != nil {
		return err
	}
	s.m.playtime.kick()
	return nil
}

// ClosedNow reports why a person can't start the server now (it's outside
// its hours), or "".
func (s *Server) ClosedNow() string {
	st := s.HoursNow()
	if st == nil || st.Open {
		return ""
	}
	return "it's outside " + s.Status().Name + "'s hours: it opens " + friendlyTime(*st.Opens, time.Now())
}

// friendlyTime says when t is, from now: "at 7:00 am", "tomorrow at 7:00 am".
func friendlyTime(t, now time.Time) string {
	clockStr := strings.ToLower(t.Format("3:04 pm"))
	switch d := midnight(t).Sub(midnight(now)); {
	case d <= 0:
		return "at " + clockStr
	case d <= 24*time.Hour+time.Hour: // a daylight saving change makes a day 23 or 25 hours
		return "tomorrow at " + clockStr
	}
	return t.Format("Monday") + " at " + clockStr
}

// ---- kids ----

// Kid is one of the family's kids, as far as play time goes.
type Kid struct {
	Name string `json:"name"`
	// Players are the names they play as: gamertags, Java usernames and
	// Hytale usernames.
	Players []string `json:"players"`
	// Hours they may play, across every server.
	Hours Hours `json:"hours"`
	// Limit is how long they may play a day.
	Limit Limit `json:"limit"`
	// Extra is extra time the owner gave, by day ("2026-10-24": minutes).
	Extra map[string]int `json:"extra,omitempty"`
}

// Limit is a daily time limit in minutes: one for school days (Monday to
// Friday), one for weekends.
type Limit struct {
	On      bool `json:"on"`
	School  int  `json:"school"`
	Weekend int  `json:"weekend"`
}

// DefaultLimit is offered when a limit is first turned on.
var DefaultLimit = Limit{School: 120, Weekend: 180}

const maxLimit = 24 * 60

func dayKey(t time.Time) string { return t.Format("2006-01-02") }

// playerKey compares names across games: a console player on a Java server
// is ".Gamer_Tag" for the gamertag "Gamer Tag".
func playerKey(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	name = strings.TrimPrefix(name, ".")
	return strings.ReplaceAll(name, " ", "_")
}

func (k Kid) plays(name string) bool {
	key := playerKey(name)
	for _, p := range k.Players {
		if playerKey(p) == key {
			return true
		}
	}
	return false
}

func (k Kid) limitFor(day time.Time) int {
	if weekendDay(day) {
		return k.Limit.Weekend
	}
	return k.Limit.School
}

// playRules are the kids, in <data>/play-time.json.
type playRules struct {
	Kids []Kid `json:"kids"`
}

// playtime runs hours and limits.
type playtime struct {
	m      *Manager
	path   string
	mu     sync.Mutex
	rules  playRules
	warned map[string]int // what was last warned about, by "server/<id>/<closing>" or "kid/<name>/<day>"
	wake   chan struct{}
	// closing is each running server's next closing time, as last seen
	// while it was open (players were warned about it). grace is when a
	// server found running outside its hours without warnings (its hours
	// were just set, or it was started by an update) closes.
	closing map[string]time.Time
	grace   map[string]time.Time
}

func newPlaytime(m *Manager) *playtime {
	p := &playtime{m: m, path: filepath.Join(m.opts.DataDir, "play-time.json"), warned: map[string]int{}, wake: make(chan struct{}, 1),
		closing: map[string]time.Time{}, grace: map[string]time.Time{}}
	if b, err := os.ReadFile(p.path); err == nil {
		_ = json.Unmarshal(b, &p.rules)
	}
	return p
}

// kick has the loop look again now.
func (p *playtime) kick() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *playtime) saveLocked() error {
	b, _ := json.MarshalIndent(p.rules, "", "  ")
	if err := os.WriteFile(p.path+".tmp", b, 0o644); err != nil {
		return err
	}
	return os.Rename(p.path+".tmp", p.path)
}

func (p *playtime) kids() []Kid {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Kid, len(p.rules.Kids))
	for i, k := range p.rules.Kids {
		k.Players = append([]string(nil), k.Players...)
		extra := map[string]int{}
		for d, n := range k.Extra {
			extra[d] = n
		}
		k.Extra = extra
		out[i] = k
	}
	return out
}

var errKidName = errors.New("type the kid's name (up to 32 letters)")

func cleanKid(k Kid) (Kid, error) {
	k.Name = strings.Join(strings.Fields(k.Name), " ")
	if k.Name == "" || len([]rune(k.Name)) > 32 {
		return k, errKidName
	}
	var players []string
	seen := map[string]bool{}
	for _, n := range k.Players {
		n = strings.Join(strings.Fields(n), " ")
		if n == "" {
			continue
		}
		if len([]rune(n)) > 32 {
			return k, fmt.Errorf("%q is too long for a player name", n)
		}
		if !seen[playerKey(n)] {
			seen[playerKey(n)] = true
			players = append(players, n)
		}
	}
	k.Players = players
	if k.Hours.On {
		var err error
		if k.Hours, err = k.Hours.clean(); err != nil {
			return k, err
		}
	} else if k.Hours.School.From == "" {
		k.Hours.School, k.Hours.Weekend = DefaultHours.School, DefaultHours.Weekend
	}
	if k.Limit.School == 0 && k.Limit.Weekend == 0 {
		k.Limit.School, k.Limit.Weekend = DefaultLimit.School, DefaultLimit.Weekend
	}
	if k.Limit.School < 0 || k.Limit.Weekend < 0 || k.Limit.School > maxLimit || k.Limit.Weekend > maxLimit {
		return k, errors.New("a daily limit is between 0 minutes and 24 hours")
	}
	return k, nil
}

// SaveKid adds or changes a kid. old is their name before (empty for a new
// kid). Extra time already given today is kept.
func (m *Manager) SaveKid(old string, k Kid) error {
	k, err := cleanKid(k)
	if err != nil {
		return err
	}
	p := m.playtime
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := -1
	for i, x := range p.rules.Kids {
		if old != "" && strings.EqualFold(x.Name, old) {
			idx = i
		} else if strings.EqualFold(x.Name, k.Name) {
			return fmt.Errorf("there's already a kid called %s", x.Name)
		}
	}
	for _, x := range p.rules.Kids {
		if strings.EqualFold(x.Name, old) || strings.EqualFold(x.Name, k.Name) {
			continue
		}
		for _, n := range k.Players {
			if x.plays(n) {
				return fmt.Errorf("%s is already one of %s's players", n, x.Name)
			}
		}
	}
	if old != "" && idx < 0 {
		return errors.New("that kid isn't on the list any more")
	}
	if idx >= 0 {
		k.Extra = p.rules.Kids[idx].Extra
		p.rules.Kids[idx] = k
	} else {
		k.Extra = nil
		p.rules.Kids = append(p.rules.Kids, k)
	}
	sort.Slice(p.rules.Kids, func(i, j int) bool {
		return strings.ToLower(p.rules.Kids[i].Name) < strings.ToLower(p.rules.Kids[j].Name)
	})
	if err := p.saveLocked(); err != nil {
		return err
	}
	p.kick()
	return nil
}

// DeleteKid takes a kid off the list.
func (m *Manager) DeleteKid(name string) error {
	p := m.playtime
	p.mu.Lock()
	defer p.mu.Unlock()
	kept := p.rules.Kids[:0]
	found := false
	for _, x := range p.rules.Kids {
		if strings.EqualFold(x.Name, name) {
			found = true
			continue
		}
		kept = append(kept, x)
	}
	if !found {
		return errors.New("that kid isn't on the list any more")
	}
	p.rules.Kids = kept
	return p.saveLocked()
}

// GiveExtraTime gives a kid more time today (minutes; 0 takes it back).
func (m *Manager) GiveExtraTime(name string, minutes int) error {
	if minutes < 0 || minutes > maxLimit {
		return errors.New("extra time is between 0 minutes and 24 hours")
	}
	p := m.playtime
	p.mu.Lock()
	defer p.mu.Unlock()
	today := dayKey(time.Now())
	for i, x := range p.rules.Kids {
		if !strings.EqualFold(x.Name, name) {
			continue
		}
		extra := map[string]int{}
		for d, n := range x.Extra {
			if d >= dayKey(time.Now().AddDate(0, 0, -7)) {
				extra[d] = n // a week of it is plenty to look back on
			}
		}
		if minutes == 0 {
			delete(extra, today)
		} else {
			extra[today] += minutes
		}
		p.rules.Kids[i].Extra = extra
		// Warnings start over with the new time.
		delete(p.warned, "kid/"+strings.ToLower(x.Name)+"/"+today)
		if err := p.saveLocked(); err != nil {
			return err
		}
		p.kick()
		return nil
	}
	return errors.New("that kid isn't on the list any more")
}

// ---- how much they've played ----

// liveSessions are the visits going on now, as sessions ending now.
func (m *Manager) liveSessions(now time.Time) []Session {
	var out []Session
	for _, s := range m.all() {
		s.mu.Lock()
		for _, pl := range s.players {
			out = append(out, Session{Server: s.meta.ID, Name: pl.Name, Start: pl.Since, End: now})
		}
		s.mu.Unlock()
	}
	return out
}

// playedOn is how long sessions cover the day starting at day, for players
// matching match.
func playedOn(day time.Time, sessions []Session, match func(name string) bool) time.Duration {
	start, end := day, day.AddDate(0, 0, 1)
	var total time.Duration
	add := func(a, b time.Time) {
		if a.Before(start) {
			a = start
		}
		if b.After(end) {
			b = end
		}
		if b.After(a) {
			total += b.Sub(a)
		}
	}
	for _, x := range sessions {
		if match(x.Name) {
			add(x.Start, x.End)
		}
	}
	return total
}

// left works out how long a kid may still play now, and why it ends: "limit"
// or "bedtime". ok is false when nothing limits them.
func (k Kid) left(now time.Time, played time.Duration) (left time.Duration, why string, ok bool) {
	extra := time.Duration(k.Extra[dayKey(now)]) * time.Minute
	if k.Limit.On {
		left, why, ok = time.Duration(k.limitFor(now))*time.Minute+extra-played, "limit", true
	}
	if k.Hours.On {
		from, until := k.Hours.span(midnight(now))
		var h time.Duration
		if !now.Before(from) {
			h = until.Add(extra).Sub(now)
		}
		if !ok || h < left {
			left, why, ok = h, "bedtime", true
		}
	}
	if left < 0 {
		left = 0
	}
	return left, why, ok
}

// ---- the loop ----

// RunPlayTime keeps hours and limits until the panel shuts down.
func (m *Manager) RunPlayTime() {
	go func() {
		t := time.NewTicker(playTick)
		defer t.Stop()
		for {
			m.playtime.pass(time.Now())
			select {
			case <-m.ctx.Done():
				return
			case <-t.C:
			case <-m.playtime.wake:
			}
		}
	}()
}

// pass looks at every server and kid once.
func (p *playtime) pass(now time.Time) {
	for _, s := range p.m.all() {
		if p.m.ctx.Err() != nil {
			return
		}
		p.serverHours(s, now)
	}
	p.kidLimits(now)
}

// minutesLeft rounds up: 9m30s left is "10 minutes".
func minutesLeft(d time.Duration) int { return int((d + time.Minute - 1) / time.Minute) }

// dueWarning returns the warning to give now (10, 5 or 1 minutes), or 0,
// remembering it under key so each is given once.
func (p *playtime) dueWarning(key string, left time.Duration) int {
	n := minutesLeft(left)
	p.mu.Lock()
	defer p.mu.Unlock()
	last, given := p.warned[key]
	for _, w := range warnings {
		if n <= w && (!given || w < last) {
			due := w
			for _, w2 := range warnings {
				if n <= w2 && w2 < due {
					due = w2 // only the most urgent one, if several are due at once
				}
			}
			p.warned[key] = due
			return n
		}
	}
	return 0
}

func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %ss", n, one)
}

// serverHours closes and opens a server on time.
func (p *playtime) serverHours(s *Server, now time.Time) {
	s.mu.Lock()
	st := s.hoursStatusLocked(now)
	running := s.cmd != nil && (s.state == StateRunning || s.state == StateStarting)
	stopped := s.cmd == nil && s.state == StateStopped && !s.busy && !s.backupActive && !s.coldBackup && s.restart == nil
	closedForHours, autoStart, deleted := s.meta.ClosedForHours, s.meta.AutoStart, s.deleted
	id, name := s.meta.ID, s.meta.Name
	if !s.meta.OpenLate.IsZero() && !s.meta.OpenLate.After(now) {
		s.meta.OpenLate = time.Time{}
		s.saveMetaLater()
	}
	s.mu.Unlock()
	if deleted {
		return
	}
	if st == nil || st.Open {
		if closedForHours && autoStart && stopped {
			s.setClosedForHours(false)
			s.say("Opening for the day.")
			p.m.log.Info("opening a server for its hours", "id", id)
			if err := s.Start(); err != nil {
				s.say("It didn't start: " + err.Error())
			}
		} else if closedForHours && (st == nil || !autoStart) {
			s.setClosedForHours(false)
		}
		p.mu.Lock()
		delete(p.grace, id)
		if st != nil && running {
			p.closing[id] = *st.Closes
		}
		p.mu.Unlock()
		if st != nil && running {
			left := st.Closes.Sub(now)
			key := "server/" + id + "/" + st.Closes.Format(time.RFC3339)
			if n := p.dueWarning(key, left); n > 0 {
				_ = s.Message("", fmt.Sprintf("%s closes for the night in %s. Find a safe spot!", name, plural(n, "minute")))
			}
		}
		return
	}
	// Outside its hours.
	if running {
		// Players were warned about this closing while it was open; if not,
		// they get 10 minutes' warning now.
		p.mu.Lock()
		last, warned := p.closing[id]
		warned = warned && !now.Before(last) && now.Sub(last) < 5*time.Minute
		g, ok := p.grace[id]
		if !warned && !ok {
			g = now.Add(time.Duration(warnings[0]) * time.Minute)
			p.grace[id] = g
		}
		p.mu.Unlock()
		if !warned && now.Before(g) {
			if n := p.dueWarning("server/"+id+"/"+g.Format(time.RFC3339), g.Sub(now)); n > 0 {
				_ = s.Message("", fmt.Sprintf("%s is outside its hours and closes in %s. Find a safe spot!", name, plural(n, "minute")))
			}
			return
		}
		p.mu.Lock()
		delete(p.grace, id)
		delete(p.closing, id)
		p.mu.Unlock()
		s.say("Closing for the night (outside its hours).")
		p.m.log.Info("closing a server for the night", "id", id)
		_ = s.Message("", "Goodnight! "+name+" is closing for the night.")
		s.setClosedForHours(true)
		s.stop(true)
		return
	}
	if autoStart && !closedForHours {
		s.setClosedForHours(true) // it opens again with its hours
	}
}

func (s *Server) setClosedForHours(v bool) {
	s.mu.Lock()
	changed := s.meta.ClosedForHours != v
	s.meta.ClosedForHours = v
	s.mu.Unlock()
	if changed {
		if err := s.saveMeta(); err != nil {
			s.m.log.Warn("could not save server details", "id", s.ID(), "error", err)
		}
	}
}

// kidLimits warns kids whose time is nearly up and moves them off servers
// when it is.
func (p *playtime) kidLimits(now time.Time) {
	kids := p.kids()
	if len(kids) == 0 {
		return
	}
	day := midnight(now)
	sessions := append(p.m.history.since(day), p.m.liveSessions(now)...)
	for _, k := range kids {
		if !k.Limit.On && !k.Hours.On {
			continue
		}
		played := playedOn(day, sessions, k.plays)
		left, why, ok := k.left(now, played)
		if !ok {
			continue
		}
		// Where are they playing?
		type where struct {
			s    *Server
			name string
		}
		var on []where
		for _, s := range p.m.all() {
			s.mu.Lock()
			for _, pl := range s.players {
				if k.plays(pl.Name) {
					on = append(on, where{s, pl.Name})
				}
			}
			s.mu.Unlock()
		}
		if len(on) == 0 {
			continue
		}
		key := "kid/" + strings.ToLower(k.Name) + "/" + dayKey(now)
		if left <= 0 {
			reason := "That's all your play time for today. See you tomorrow!"
			if why == "bedtime" {
				reason = "It's past your bedtime. See you tomorrow!"
				if from, _ := k.Hours.span(day); now.Before(from) {
					reason = "It's too early to play. You can play from " + strings.ToLower(from.Format("3:04 pm")) + "."
				}
			}
			for _, w := range on {
				if w.s.isHytale() {
					continue // Hytale servers can't be told to kick yet
				}
				w.s.say(fmt.Sprintf("%s's time is up for today (%s): moving %s off the server.", k.Name, map[string]string{"limit": "daily limit", "bedtime": "play hours"}[why], w.name))
				p.m.log.Info("play time is up", "kid", k.Name, "player", w.name, "server", w.s.ID(), "why", why)
				_ = w.s.Kick(w.name, reason)
			}
			continue
		}
		if n := p.dueWarning(key, left); n > 0 {
			text := fmt.Sprintf("You have %s of play left today.", plural(n, "minute"))
			if why == "bedtime" {
				text = fmt.Sprintf("Bedtime in %s! Find a safe spot.", plural(n, "minute"))
			}
			for _, w := range on {
				if !w.s.isHytale() {
					_ = w.s.Message(w.name, text)
				}
			}
		}
	}
}

// ---- for the page ----

// KidView is a kid with how they're doing.
type KidView struct {
	Kid
	// Today is minutes played today; Left is minutes left (nil: no limit
	// or hours) and Why what ends it ("limit" or "bedtime").
	Today      int    `json:"today"`
	Left       *int   `json:"left,omitempty"`
	Why        string `json:"why,omitempty"`
	ExtraToday int    `json:"extraToday"`
	// Playing is where they're playing now (server names).
	Playing []string `json:"playing"`
	// Week is minutes played on each of the last 7 days, oldest first.
	Week []DayPlay `json:"week"`
}

// DayPlay is time played on a day.
type DayPlay struct {
	Day     string `json:"day"` // "2026-10-24"
	Minutes int    `json:"minutes"`
}

// PlayerWeek is a player's week, for players nobody has claimed yet and a
// server's Players tab.
type PlayerWeek struct {
	Name    string    `json:"name"`
	Minutes int       `json:"minutes"`
	Last    time.Time `json:"last"`
	Servers []string  `json:"servers,omitempty"`
}

// PlayTimeView is the Play time page.
type PlayTimeView struct {
	Kids []KidView `json:"kids"`
	// Others played this week but aren't on any kid's list.
	Others []PlayerWeek `json:"others"`
	// Servers with hours.
	Servers []ServerHoursView `json:"servers"`
	// Defaults for turning things on.
	DefaultHours Hours `json:"defaultHours"`
	DefaultLimit Limit `json:"defaultLimit"`
}

// ServerHoursView is a server and its hours.
type ServerHoursView struct {
	ID    string       `json:"id"`
	Name  string       `json:"name"`
	Hours *HoursStatus `json:"hours"`
}

func roundMinutes(d time.Duration) int { return int((d + 30*time.Second) / time.Minute) }

// PlayTime returns the Play time page.
func (m *Manager) PlayTime() PlayTimeView {
	now := time.Now()
	today := midnight(now)
	weekStart := today.AddDate(0, 0, -6)
	sessions := append(m.history.since(weekStart), m.liveSessions(now)...)
	names := m.serverNames()
	v := PlayTimeView{Kids: []KidView{}, Others: []PlayerWeek{}, Servers: []ServerHoursView{}, DefaultHours: DefaultHours, DefaultLimit: DefaultLimit}
	kids := m.playtime.kids()
	for _, k := range kids {
		kv := KidView{Kid: k, Playing: []string{}, ExtraToday: k.Extra[dayKey(now)]}
		for i := 0; i < 7; i++ {
			day := weekStart.AddDate(0, 0, i)
			kv.Week = append(kv.Week, DayPlay{Day: dayKey(day), Minutes: roundMinutes(playedOn(day, sessions, k.plays))})
		}
		played := playedOn(today, sessions, k.plays)
		kv.Today = roundMinutes(played)
		if left, why, ok := k.left(now, played); ok {
			n := minutesLeft(left)
			kv.Left, kv.Why = &n, why
		}
		for _, s := range m.all() {
			s.mu.Lock()
			for _, pl := range s.players {
				if k.plays(pl.Name) {
					kv.Playing = append(kv.Playing, s.meta.Name)
				}
			}
			s.mu.Unlock()
		}
		v.Kids = append(v.Kids, kv)
	}
	claimed := func(name string) bool {
		for _, k := range kids {
			if k.plays(name) {
				return true
			}
		}
		return false
	}
	v.Others = playerWeeks(sessions, weekStart, names, func(name string) bool { return !claimed(name) })
	for _, s := range m.all() {
		s.mu.Lock()
		if st := s.hoursStatusLocked(now); st != nil {
			v.Servers = append(v.Servers, ServerHoursView{ID: s.meta.ID, Name: s.meta.Name, Hours: st})
		}
		s.mu.Unlock()
	}
	sort.Slice(v.Servers, func(i, j int) bool { return strings.ToLower(v.Servers[i].Name) < strings.ToLower(v.Servers[j].Name) })
	return v
}

func (m *Manager) serverNames() map[string]string {
	out := map[string]string{}
	for _, s := range m.all() {
		s.mu.Lock()
		out[s.meta.ID] = s.meta.Name
		s.mu.Unlock()
	}
	return out
}

// playerWeeks totals each player's time in sessions since from, for players
// keep accepts, most played first.
func playerWeeks(sessions []Session, from time.Time, names map[string]string, keep func(string) bool) []PlayerWeek {
	byKey := map[string]*PlayerWeek{}
	secs := map[string]int{}
	for _, x := range sessions {
		if !keep(x.Name) {
			continue
		}
		a, b := x.Start, x.End
		if a.Before(from) {
			a = from
		}
		k := playerKey(x.Name)
		w := byKey[k]
		if w == nil {
			w = &PlayerWeek{Name: x.Name}
			byKey[k] = w
		}
		if b.After(a) {
			secs[k] += int(b.Sub(a) / time.Second)
		}
		if b.After(w.Last) {
			w.Last = b
		}
		if n := names[x.Server]; n != "" && !contains(w.Servers, n) {
			w.Servers = append(w.Servers, n)
		}
	}
	out := []PlayerWeek{}
	for k, w := range byKey {
		w.Minutes = (secs[k] + 30) / 60
		sort.Strings(w.Servers)
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Minutes != out[j].Minutes {
			return out[i].Minutes > out[j].Minutes
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// PlayedThisWeek is who played on the server in the last 7 days.
func (s *Server) PlayedThisWeek() []PlayerWeek {
	now := time.Now()
	from := midnight(now).AddDate(0, 0, -6)
	id := s.ID()
	var mine []Session
	for _, x := range append(s.m.history.since(from), s.m.liveSessions(now)...) {
		if x.Server == id {
			mine = append(mine, x)
		}
	}
	return playerWeeks(mine, from, nil, func(string) bool { return true })
}
