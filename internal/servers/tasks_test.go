package servers

import (
	"testing"
	"time"
)

func TestCronNext(t *testing.T) {
	loc := time.UTC
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct{ spec, from, want string }{
		{"0 4 * * *", "2026-11-03 20:44", "2026-11-04 04:00"},
		{"*/15 * * * *", "2026-11-03 20:44", "2026-11-03 20:45"},
		{"30 6 * * 1-5", "2026-11-06 07:00", "2026-11-09 06:30"}, // Friday after: Monday
		{"0 0 1 * *", "2026-11-03 00:00", "2026-12-01 00:00"},
		{"0 12 * * 7", "2026-11-03 00:00", "2026-11-08 12:00"}, // 7 is Sunday
		{"0 9 13 * 5", "2026-11-03 00:00", "2026-11-06 09:00"}, // day 13 or a Friday
		{"5,35 10-11 * 2 *", "2026-11-03 00:00", "2027-02-01 10:05"},
	}
	for _, c := range cases {
		spec, err := parseCron(c.spec)
		if err != nil {
			t.Fatalf("%s: %v", c.spec, err)
		}
		if got := spec.next(at(c.from)); !got.Equal(at(c.want)) {
			t.Errorf("%s from %s: got %s, want %s", c.spec, c.from, got.Format("2006-01-02 15:04"), c.want)
		}
	}
	for _, bad := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "a * * * *", "*/0 * * * *", "5-1 * * * *"} {
		if _, err := parseCron(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestDailyAndEveryNext(t *testing.T) {
	loc := time.FixedZone("MT", -7*3600)
	tue := time.Date(2026, 11, 3, 20, 44, 0, 0, loc) // a Tuesday
	sc := TaskSchedule{Kind: "daily", At: "04:00"}
	if got := sc.nextAfter(tue); !got.Equal(time.Date(2026, 11, 4, 4, 0, 0, 0, loc)) {
		t.Errorf("daily: %s", got)
	}
	sc.Days = []int{0, 6} // weekends
	if got := sc.nextAfter(tue); !got.Equal(time.Date(2026, 11, 7, 4, 0, 0, 0, loc)) {
		t.Errorf("weekends: %s", got)
	}
	sc = TaskSchedule{Kind: "daily", At: "20:44"}
	if got := sc.nextAfter(tue); !got.Equal(time.Date(2026, 11, 4, 20, 44, 0, 0, loc)) {
		t.Errorf("the same minute is next day's: %s", got)
	}
	if got := (TaskSchedule{Kind: "every", Every: 90}).nextAfter(tue); !got.Equal(tue.Add(90 * time.Minute)) {
		t.Errorf("every: %s", got)
	}
	if !(TaskSchedule{Kind: "after", After: "x"}).nextAfter(tue).IsZero() {
		t.Error("a follower has no time of its own")
	}
}

func TestScheduleCheck(t *testing.T) {
	tasks := []Task{
		{ID: "a", Schedule: TaskSchedule{Kind: "daily", At: "04:00"}},
		{ID: "b", Schedule: TaskSchedule{Kind: "after", After: "a"}},
		{ID: "c", Schedule: TaskSchedule{Kind: "after", After: "b"}},
	}
	if err := (TaskSchedule{Kind: "after", After: "c", Delay: 5}).check("d", tasks); err != nil {
		t.Errorf("a chain a→b→c→d is fine: %v", err)
	}
	if err := (TaskSchedule{Kind: "after", After: "c"}).check("a", tasks); err == nil {
		t.Error("a following its own chain should be refused")
	}
	if err := (TaskSchedule{Kind: "after", After: "zzz"}).check("d", tasks); err == nil {
		t.Error("following a missing task should be refused")
	}
	for _, bad := range []TaskSchedule{{Kind: "every"}, {Kind: "daily", At: "25:00"}, {Kind: "daily", At: "04:00", Days: []int{7}}, {Kind: "cron", Cron: "x"}, {Kind: "soon"}} {
		if bad.check("x", tasks) == nil {
			t.Errorf("%+v should be refused", bad)
		}
	}
}

func TestTasksRun(t *testing.T) {
	taskMinute = 200 * time.Millisecond
	m := newTestManager(t, newFakeMojang(t, "1.0.0.1"), 2*time.Second)
	s := createRunning(t, m, "Tasks")

	// Bad tasks are refused.
	for _, bad := range []Task{
		{Name: "", Action: "command", Command: "say hi", Schedule: TaskSchedule{Kind: "every", Every: 5}},
		{Name: "x", Action: "dance", Schedule: TaskSchedule{Kind: "every", Every: 5}},
		{Name: "x", Action: "command", Schedule: TaskSchedule{Kind: "every", Every: 5}},
	} {
		if _, err := s.SaveTask(bad); err == nil {
			t.Errorf("%+v should be refused", bad)
		}
	}

	hello, err := s.SaveTask(Task{Name: "Hello", Enabled: true, Action: "command", Command: "say one\nsay two", Schedule: TaskSchedule{Kind: "every", Every: 60}})
	if err != nil {
		t.Fatal(err)
	}
	after, err := s.SaveTask(Task{Name: "Then", Enabled: true, Action: "command", Command: "say three", Schedule: TaskSchedule{Kind: "after", After: hello.ID}})
	if err != nil {
		t.Fatal(err)
	}
	// Not due yet.
	m.taskPass(time.Now())
	if v := s.Tasks(); v[0].Next == nil || time.Until(*v[0].Next) < 50*time.Minute || v[1].Next != nil {
		t.Fatalf("next runs: %+v", v)
	}
	// An hour on, it runs, and the follower runs straight after (no wait).
	m.taskPass(time.Now().Add(61 * time.Minute))
	waitConsole(t, s, "got: say two")
	m.taskPass(time.Now().Add(61 * time.Minute))
	waitConsole(t, s, "got: say three")
	got := s.Tasks()
	if got[0].LastResult != "ran 2 commands" || got[0].LastRun.IsZero() || got[1].LastResult != "ran say three" {
		t.Fatalf("results: %+v", got)
	}

	// Skipped while someone plays; a restart warns them first.
	skip, _ := s.SaveTask(Task{Name: "Quiet restart", Enabled: true, Action: "restart", SkipIfPlaying: true, Schedule: TaskSchedule{Kind: "every", Every: 600}})
	warn, _ := s.SaveTask(Task{Name: "Restart", Enabled: true, Action: "restart", Warn: 2, Schedule: TaskSchedule{Kind: "every", Every: 600}})
	_ = s.Command("join Steve")
	waitFor(t, s, "player", func(st Status) bool { return len(st.Players) == 1 })
	if err := s.RunTaskNow(skip.ID); err != nil {
		t.Fatal(err)
	}
	waitConsole(t, s, `Task "Quiet restart": skipped: 1 playing`)
	if err := s.RunTaskNow(warn.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RunTaskNow(warn.ID); err == nil {
		t.Error("a task that's running can't be started again")
	}
	waitConsole(t, s, "got: say The server restarts in 2 minutes.")
	waitConsole(t, s, "got: say The server restarts in 1 minute.")
	waitConsole(t, s, `Task "Restart": restarted the server`)
	waitFor(t, s, "running again", state(StateRunning))

	// Deleting a task turns off the ones that followed it.
	if err := s.DeleteTask(hello.ID); err != nil {
		t.Fatal(err)
	}
	for _, v := range s.Tasks() {
		if v.ID == after.ID && v.Enabled {
			t.Error("the follower should be turned off")
		}
	}
	// A stopped server skips commands.
	_ = s.Stop()
	waitFor(t, s, "stopped", state(StateStopped))
	cmd, _ := s.SaveTask(Task{Name: "Say", Enabled: true, Action: "command", Command: "say hi", Schedule: TaskSchedule{Kind: "every", Every: 5}})
	_ = s.RunTaskNow(cmd.ID)
	waitConsole(t, s, `Task "Say": skipped: the server isn't running`)
	// A scheduled start starts it.
	start, _ := s.SaveTask(Task{Name: "Morning", Enabled: true, Action: "start", Schedule: TaskSchedule{Kind: "cron", Cron: "0 7 * * *"}})
	_ = s.RunTaskNow(start.ID)
	waitFor(t, s, "started", state(StateRunning))
}
