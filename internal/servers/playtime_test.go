package servers

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func day(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestHoursWindow(t *testing.T) {
	h := Hours{On: true, School: Span{"07:00", "21:00"}, Weekend: Span{"08:00", "22:30"}}
	// 2026-10-22 is a Thursday (a school night), 2026-10-23 a Friday.
	for _, c := range []struct {
		now          string
		open         bool
		closes, opns string
	}{
		{"2026-10-22 06:59", false, "", "2026-10-22 07:00"},
		{"2026-10-22 07:00", true, "2026-10-22 21:00", ""},
		{"2026-10-22 20:59", true, "2026-10-22 21:00", ""},
		{"2026-10-22 21:00", false, "", "2026-10-23 08:00"}, // Friday opens with the weekend's hours
		{"2026-10-23 22:00", true, "2026-10-23 22:30", ""},
		{"2026-10-24 23:00", false, "", "2026-10-25 07:00"}, // Sunday is a school night
	} {
		open, closes, opens := h.window(day(t, c.now))
		if open != c.open || (c.closes != "" && !closes.Equal(day(t, c.closes))) || (c.opns != "" && !opens.Equal(day(t, c.opns))) {
			t.Errorf("%s: open %v closes %v opens %v", c.now, open, closes, opens)
		}
	}
	if _, err := (Hours{On: true, School: Span{"21:00", "07:00"}, Weekend: Span{"08:00", "22:00"}}).clean(); err == nil {
		t.Error("an end before the start must be refused")
	}
	if _, err := (Hours{On: true, School: Span{"7", "21:00"}, Weekend: Span{"08:00", "22:00"}}).clean(); err == nil {
		t.Error("a bad time must be refused")
	}
	if got, err := (Hours{On: true}).clean(); err != nil || got.School != DefaultHours.School {
		t.Errorf("empty hours get the defaults: %+v %v", got, err)
	}
}

func TestFriendlyTime(t *testing.T) {
	now := day(t, "2026-10-22 22:00")
	for when, want := range map[string]string{
		"2026-10-22 23:30": "at 11:30 pm",
		"2026-10-23 07:00": "tomorrow at 7:00 am",
		"2026-10-25 07:00": "Sunday at 7:00 am",
	} {
		if got := friendlyTime(day(t, when), now); got != want {
			t.Errorf("%s: %q, want %q", when, got, want)
		}
	}
}

func TestKidLeft(t *testing.T) {
	k := Kid{Name: "Ava", Players: []string{"Ava Plays", "ava_java"},
		Limit: Limit{On: true, School: 60, Weekend: 120},
		Hours: Hours{On: true, School: Span{"07:00", "20:00"}, Weekend: Span{"08:00", "21:00"}}}
	thu := day(t, "2026-10-22 15:00")
	if left, why, ok := k.left(thu, 45*time.Minute); !ok || left != 15*time.Minute || why != "limit" {
		t.Errorf("limit: %v %s %v", left, why, ok)
	}
	if left, why, _ := k.left(day(t, "2026-10-22 19:50"), 0); left != 10*time.Minute || why != "bedtime" {
		t.Errorf("bedtime: %v %s", left, why)
	}
	if left, _, _ := k.left(day(t, "2026-10-22 06:30"), 0); left != 0 {
		t.Errorf("too early: %v", left)
	}
	if left, _, _ := k.left(day(t, "2026-10-24 15:00"), 90*time.Minute); left != 30*time.Minute {
		t.Errorf("Saturday's limit: %v", left)
	}
	k.Extra = map[string]int{"2026-10-22": 30}
	if left, _, _ := k.left(thu, 75*time.Minute); left != 15*time.Minute {
		t.Errorf("extra time: %v", left)
	}
	if left, _, _ := k.left(day(t, "2026-10-22 20:10"), 0); left != 20*time.Minute {
		t.Errorf("extra time pushes bedtime back too: %v", left)
	}
	// A console player on a Java server is ".Ava_Plays".
	if !k.plays(".Ava_Plays") || !k.plays("AVA_JAVA") || k.plays("Sam") {
		t.Error("names")
	}
	if _, _, ok := (Kid{}).left(thu, 0); ok {
		t.Error("nothing limits a kid without a limit or hours")
	}
}

func TestPlayedOnClipsToTheDay(t *testing.T) {
	d := day(t, "2026-10-22 00:00")
	sessions := []Session{
		{Name: "Ava Plays", Start: day(t, "2026-10-21 23:30"), End: day(t, "2026-10-22 00:30")},
		{Name: ".Ava_Plays", Start: day(t, "2026-10-22 10:00"), End: day(t, "2026-10-22 11:00")},
		{Name: "Sam", Start: day(t, "2026-10-22 10:00"), End: day(t, "2026-10-22 11:00")},
	}
	k := Kid{Players: []string{"Ava Plays"}}
	if got := playedOn(d, sessions, k.plays); got != 90*time.Minute {
		t.Errorf("played %v", got)
	}
}

func TestWarningsOncePerStep(t *testing.T) {
	p := &playtime{warned: map[string]int{}}
	var got []int
	for _, left := range []time.Duration{20 * time.Minute, 9*time.Minute + 30*time.Second, 8 * time.Minute, 5 * time.Minute, 4 * time.Minute, 50 * time.Second, 10 * time.Second} {
		if n := p.dueWarning("k", left); n > 0 {
			got = append(got, n)
		}
	}
	if fmtInts(got) != "[10 5 1]" {
		t.Errorf("warnings %v", got)
	}
	// Joining with 3 minutes left: one warning, then the last one.
	got = nil
	for _, left := range []time.Duration{3 * time.Minute, 2 * time.Minute, 30 * time.Second} {
		if n := p.dueWarning("j", left); n > 0 {
			got = append(got, n)
		}
	}
	if fmtInts(got) != "[3 1]" {
		t.Errorf("late joiner %v", got)
	}
}

func fmtInts(v []int) string { return fmt.Sprint(v) }

func TestKidRules(t *testing.T) {
	m := New(Options{DataDir: t.TempDir()})
	if err := m.SaveKid("", Kid{Name: "  Ava ", Players: []string{"Ava Plays", " ava plays ", ""}, Limit: Limit{On: true}}); err != nil {
		t.Fatal(err)
	}
	ks := m.playtime.kids()
	if len(ks) != 1 || ks[0].Name != "Ava" || len(ks[0].Players) != 1 || ks[0].Limit.School != DefaultLimit.School {
		t.Fatalf("kid %+v", ks)
	}
	if err := m.SaveKid("", Kid{Name: "ava"}); err == nil {
		t.Error("two kids with one name")
	}
	if err := m.SaveKid("", Kid{Name: "Sam", Players: []string{".Ava_Plays"}}); err == nil {
		t.Error("a player can only be one kid's")
	}
	if err := m.SaveKid("", Kid{Name: "Sam", Limit: Limit{On: true, School: -5}}); err == nil {
		t.Error("a negative limit")
	}
	if err := m.GiveExtraTime("Ava", 15); err != nil {
		t.Fatal(err)
	}
	m.GiveExtraTime("Ava", 15)
	if err := m.SaveKid("Ava", Kid{Name: "Ava", Players: []string{"Ava Plays", "ava_java"}}); err != nil {
		t.Fatal(err)
	}
	ks = m.playtime.kids()
	if ks[0].Extra[dayKey(time.Now())] != 30 || len(ks[0].Players) != 2 {
		t.Errorf("extra time kept through an edit: %+v", ks[0])
	}
	// Kept in a file.
	m2 := New(Options{DataDir: m.opts.DataDir})
	if ks := m2.playtime.kids(); len(ks) != 1 || ks[0].Name != "Ava" {
		t.Errorf("reloaded %+v", ks)
	}
	if err := m.DeleteKid("ava"); err != nil || len(m.playtime.kids()) != 0 {
		t.Errorf("delete: %v", err)
	}
}

// A server with hours closes at bedtime (after warnings) and opens again
// in the morning; kids are warned and moved off when their time is up.
func TestPlayTimeOnAServer(t *testing.T) {
	m := newTestManager(t, newFakeMojang(t, "1.26.0.1"), 5*time.Second)
	s := createRunning(t, m, "Kids World")
	if err := s.SetHours(Hours{On: true, School: Span{"07:00", "21:00"}, Weekend: Span{"08:00", "22:00"}}); err != nil {
		t.Fatal(err)
	}
	thu := func(hm string) time.Time { return day(t, "2026-10-22 "+hm) }
	m.playtime.pass(thu("20:55"))
	waitConsole(t, s, "say Kids World closes for the night in 5 minutes")
	m.playtime.pass(thu("20:56"))
	m.playtime.pass(thu("20:59"))
	waitConsole(t, s, "in 1 minute.")
	m.playtime.pass(thu("21:00"))
	waitFor(t, s, "closed for the night", state(StateStopped))
	s.mu.Lock()
	closed, auto := s.meta.ClosedForHours, s.meta.AutoStart
	s.mu.Unlock()
	if !closed || !auto {
		t.Errorf("closed %v autostart %v", closed, auto)
	}
	m.playtime.pass(thu("23:00"))
	if st := s.Status(); st.State != StateStopped {
		t.Errorf("started at night: %s", st.State)
	}
	m.playtime.pass(day(t, "2026-10-23 08:00"))
	waitFor(t, s, "open in the morning", state(StateRunning))

	// Running outside its hours without warning (its hours were just set):
	// players get 10 minutes.
	m.playtime.pass(day(t, "2026-10-23 23:00"))
	waitConsole(t, s, "is outside its hours and closes in 10 minutes")
	m.playtime.pass(day(t, "2026-10-23 23:09"))
	waitConsole(t, s, "is outside its hours and closes in 1 minute")
	if st := s.Status(); st.State != StateRunning {
		t.Fatalf("closed before its 10 minutes: %s", st.State)
	}
	m.playtime.pass(day(t, "2026-10-23 23:10"))
	waitFor(t, s, "closed after 10 minutes", state(StateStopped))
	m.playtime.pass(day(t, "2026-10-24 08:00"))
	waitFor(t, s, "open the next morning", state(StateRunning))

	// A kid with 1 minute left is warned, then moved off (the server's own
	// hours are off for this, since it's really now).
	if err := s.SetHours(Hours{}); err != nil {
		t.Fatal(err)
	}
	if err := m.SaveKid("", Kid{Name: "Ava", Players: []string{"Ava Plays"}, Limit: Limit{On: true, School: 60, Weekend: 60}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	m.history.add(Session{Server: s.ID(), Name: "Ava Plays", Start: now.Add(-60 * time.Minute), End: now.Add(-1*time.Minute - 30*time.Second)})
	if midnight(now.Add(-60*time.Minute)) != midnight(now) {
		t.Skip("too close to midnight for this part")
	}
	s.Command("join Ava Plays")
	waitFor(t, s, "Ava joined", func(st Status) bool { return len(st.Players) == 1 })
	m.playtime.pass(now)
	waitConsole(t, s, `tell "Ava Plays" You have 2 minutes of play left today.`)
	m.playtime.pass(now.Add(2 * time.Minute))
	waitConsole(t, s, `kick "Ava Plays" That's all your play time for today. See you tomorrow!`)

	// Leaving records the visit, and history is kept in a file.
	s.Command("leave Ava Plays")
	waitFor(t, s, "Ava left", func(st Status) bool { return len(st.Players) == 0 })
	m.saves.Wait()
	b, err := os.ReadFile(filepath.Join(m.opts.DataDir, "play-history.json"))
	if err != nil || strings.Count(string(b), "Ava Plays") != 2 {
		t.Errorf("history file: %s %v", b, err)
	}
	if v := m.PlayTime(); len(v.Kids) != 1 || v.Kids[0].Today < 59 || v.Kids[0].Left == nil || *v.Kids[0].Left > 2 {
		t.Errorf("view %+v", v.Kids)
	}
	if w := s.PlayedThisWeek(); len(w) != 1 || w[0].Minutes < 59 {
		t.Errorf("week %+v", w)
	}
	// A stopped server ends everyone's visit.
	s.Command("join Sam")
	waitFor(t, s, "Sam joined", func(st Status) bool { return len(st.Players) == 1 })
	s.Stop()
	waitFor(t, s, "stopped", state(StateStopped))
	if v := m.PlayTime(); len(v.Others) != 1 || v.Others[0].Name != "Sam" {
		t.Errorf("others %+v", v.Others)
	}
}
