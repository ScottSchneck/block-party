package servers

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestConsoleNames(t *testing.T) {
	for in, want := range map[string][2]string{
		"Kid One":          {"Kid One", ".Kid_One"},
		".Kid_One":         {"Kid One", ".Kid_One"},
		"  Kid   One ":     {"Kid One", ".Kid_One"},
		"Fifteen Chars Ab": {"Fifteen Chars Ab", ".Fifteen_Chars_A"},
		".Dad_Gamer":       {"Dad Gamer", ".Dad_Gamer"},
		"Gamer#1234":       {"Gamer#1234", ".Gamer#1234"},
	} {
		g, j, err := consoleIDs(in)
		if err != nil || g != want[0] || j != want[1] {
			t.Errorf("%q: got %q %q %v, want %v", in, g, j, err, want)
		}
	}
	if _, _, err := consoleIDs(`."; stop`); err == nil {
		t.Error("a command got through as a gamertag")
	}
	if u, _ := floodgateUUID("2535400000000001"); u != "00000000-0000-0000-0009-01eed05d1001" {
		t.Errorf("uuid %s", u)
	}
	if _, err := floodgateUUID("abc"); err == nil {
		t.Error("bad Xbox ID")
	}
	for name, ok := range map[string]bool{"Steve_1": true, ".Kid_One": true, ".": false, ".a b": false, `.x"y`: false, "a b": false} {
		if _, err := cleanJavaPlayer(name); (err == nil) != ok {
			t.Errorf("cleanJavaPlayer(%q): %v", name, err)
		}
	}
	if baseVersion("2.11.3 (build 9)") != "2.11.3" || baseVersion("5.5.1") != "5.5.1" {
		t.Error("baseVersion")
	}
}

func TestJavaConsoles(t *testing.T) {
	f := newFakeJavaWorld(t)
	m := newJavaManager(t, f)
	if err := m.SetOwnerGamertag("Dad Gamer"); err != nil {
		t.Fatal(err)
	}
	st, err := m.CreateJava("Both Kinds", 0)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m.Get(st.ID)
	waitFor(t, s, "running", state(StateRunning))
	plugins := filepath.Join(s.dir(), "plugins")
	for file, want := range map[string]string{"Geyser-Spigot.jar": "geyser 2.11.3 build 100", "floodgate-spigot.jar": "floodgate 2.11.3 build 100", "ViaVersion.jar": "viaversion 5.5.1 build 0"} {
		if got := readFile(t, filepath.Join(plugins, file)); got != want {
			t.Errorf("%s: %q", file, got)
		}
	}
	st = s.Status()
	if !st.Consoles || !st.ConsolesLive || st.ConsolesUpdate {
		t.Errorf("status: %+v", st)
	}
	waitConsole(t, s, fmt.Sprintf("-DgeyserUdpPort=%d -DgeyserTransport=both -DgeyserSignalingPort=%d -DgeyserWebrtcPort=%d", st.Port, st.Port+1, st.Port+1))
	// The console menu sends consoles that came over NetherNet to Geyser's
	// NetherNet port.
	if j := m.Joinable(); len(j) != 1 || j[0].Port != st.Port || j[0].NetherNetPort != st.Port+1 {
		t.Errorf("joinable: %+v", j)
	}
	m.SetHostAddresses(func() HostAddresses { return HostAddresses{Home: "10.0.1.117", Internet: "203.0.113.7"} })
	s.mu.Lock()
	_, args, err := s.javaCommand()
	s.mu.Unlock()
	if err != nil || !slices.Contains(args, fmt.Sprintf("-DgeyserAdvertiseAddresses=10.0.1.117:%d,203.0.113.7:%d", st.Port+1, st.Port+1)) {
		t.Errorf("advertised addresses: %v %v", args, err)
	}
	m.SetHostAddresses(nil)
	// The owner's gamertag went on the lists before the first start, with
	// the account ID Floodgate gives them.
	if wl := readFile(t, s.whitelistPath()); !strings.Contains(wl, `".Dad_Gamer"`) || !strings.Contains(wl, "00000000-0000-0000-0009-01eed05d1001") {
		t.Errorf("whitelist: %s", wl)
	}
	if !strings.Contains(readFile(t, s.opsPath()), ".Dad_Gamer") {
		t.Error("owner isn't an operator")
	}
	if v, _ := s.Players(); !v.Consoles || len(v.Allowlist) != 1 || !v.Allowlist[0].Console {
		t.Errorf("players: %+v", v)
	}
	running := false
	for _, j := range m.Joinable() {
		running = running || (j.Port == st.Port && j.Running)
	}
	if !running {
		t.Error("not in the console list")
	}

	// Running: Floodgate's command adds them; operator waits until they're on.
	if err := s.AllowlistAdd(".Kid One", ""); err != nil {
		t.Fatal(err)
	}
	waitConsole(t, s, "got: whitelist reload")
	if wl := readFile(t, s.whitelistPath()); !strings.Contains(wl, `".Kid_One"`) || !strings.Contains(wl, "00000000-0000-0000-0009-01eed05d1002") {
		t.Errorf("whitelist: %s", wl)
	}
	pending, err := s.OpAdd(".Kid_One")
	if err != nil || !pending {
		t.Fatalf("op: %v %v", pending, err)
	}
	if consoleHas(s, "got: op .Kid_One") {
		t.Error("op sent before they joined")
	}
	s.Command("join .Kid_One")
	waitConsole(t, s, "got: op .Kid_One")
	if err := s.Kick(".Kid_One", ""); err != nil {
		t.Fatal(err)
	}
	waitConsole(t, s, "got: kick .Kid_One")
	s.Command("reject .Stranger")
	waitConsole(t, s, "not white-listed")
	time.Sleep(200 * time.Millisecond)
	v, _ := s.Players()
	if len(v.Attempts) != 1 || v.Attempts[0].Name != ".Stranger" {
		t.Errorf("attempts: %+v", v.Attempts)
	}

	// Stopped: the files, with the account ID from their Xbox ID.
	s.Stop()
	waitFor(t, s, "stopped", state(StateStopped))
	if err := s.AllowlistAdd(".Stranger", ""); err != nil {
		t.Fatal(err)
	}
	if wl := readFile(t, s.whitelistPath()); !strings.Contains(wl, "00000000-0000-0000-0009-01eed05d1009") {
		t.Errorf("whitelist: %s", wl)
	}
	if v, _ := s.Players(); len(v.Attempts) != 0 {
		t.Errorf("still waiting: %+v", v.Attempts)
	}
	if err := s.AllowlistRemove(".Stranger"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readFile(t, s.whitelistPath()), "Stranger") {
		t.Error("not removed")
	}
	if err := s.AllowlistAdd(".No Such Tag", ""); err == nil || !strings.Contains(err.Error(), "isn't known yet") {
		t.Errorf("unknown gamertag: %v", err)
	}
	// The lookup is down: added by command at the next start.
	f.mu.Lock()
	f.xboxDown = true
	f.mu.Unlock()
	if err := s.AllowlistAdd(".Stranger", ""); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Players(); !hasPending(v.Allowlist, ".Stranger") {
		t.Errorf("not pending: %+v", v.Allowlist)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	waitConsole(t, s, `got: fwhitelist add "Stranger"`)
	waitFor(t, s, "running", state(StateRunning))

	// A new Geyser version: an update is offered, and Update installs it
	// without touching Paper.
	f.mu.Lock()
	f.geyser, f.geyserB = "2.11.4", 101
	f.mu.Unlock()
	recheckPlugins(m)
	waitFor(t, s, "console update offered", func(st Status) bool { return st.ConsolesUpdate })
	if err := s.Update(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, s, "updated", func(st Status) bool { return !st.ConsolesUpdate && st.State == StateRunning })
	if got := readFile(t, filepath.Join(plugins, "Geyser-Spigot.jar")); got != "geyser 2.11.4 build 101" {
		t.Errorf("geyser: %q", got)
	}
	if st := s.Status(); st.Build != 5 {
		t.Errorf("Paper changed: %+v", st)
	}
	// A new build of the same version isn't worth an update by itself.
	f.mu.Lock()
	f.geyserB = 102
	f.mu.Unlock()
	recheckPlugins(m)
	time.Sleep(300 * time.Millisecond)
	if s.Status().ConsolesUpdate {
		t.Error("a new build alone offered as an update")
	}

	// Turned off: Geyser and Floodgate go, consoles can't be added, and the
	// server leaves the console list.
	if err := s.SetConsoles(false); err != nil {
		t.Fatal(err)
	}
	waitConsole(t, s, "Removed console support")
	for file, gone := range map[string]bool{"Geyser-Spigot.jar": true, "floodgate-spigot.jar": true, "ViaVersion.jar": false} {
		if _, err := os.Stat(filepath.Join(plugins, file)); os.IsNotExist(err) != gone {
			t.Errorf("%s: %v", file, err)
		}
	}
	if err := s.AllowlistAdd(".Kid One", ""); err != errNoConsoles {
		t.Errorf("console player added with consoles off: %v", err)
	}
	if err := s.Restart(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, s, "running", state(StateRunning))
	if st := s.Status(); st.Consoles || st.ConsolesLive {
		t.Errorf("status: %+v", st)
	}
	for _, j := range m.Joinable() {
		if j.Port == st.Port {
			t.Error("still in the console list")
		}
	}
	// Taking a console player off while it runs without Floodgate: the file,
	// then a reload. Operator removal is also made in the file before the
	// next start (the server may not know them).
	if err := s.AllowlistRemove(".Kid One"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readFile(t, s.whitelistPath()), "Kid_One") {
		t.Error("still on the allowlist")
	}
	if err := s.OpRemove(".Dad_Gamer"); err != nil {
		t.Fatal(err)
	}
	waitConsole(t, s, "got: deop .Dad_Gamer")
	lines, _, cancel := s.Subscribe()
	cancel()
	last := ""
	for _, l := range lines {
		if strings.Contains(l, "args:") {
			last = l
		}
	}
	if strings.Contains(last, "geyser") {
		t.Errorf("Geyser port given with consoles off: %s", last)
	}

	if err := s.Restart(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readFile(t, s.opsPath()), "Dad_Gamer") {
		t.Error("still an operator after the restart")
	}
	waitFor(t, s, "running", state(StateRunning))

	// On again: installed again.
	if err := s.SetConsoles(true); err != nil {
		t.Fatal(err)
	}
	waitConsole(t, s, "Console players can join after the server restarts")
	if !s.consolesInstalled() {
		t.Error("not installed again")
	}
}

func TestConsolePluginsSurviveViaVersionOutage(t *testing.T) {
	f := newFakeJavaWorld(t)
	f.viaDown = true
	m := newJavaManager(t, f)
	st, err := m.CreateJava("No Via", 0)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m.Get(st.ID)
	waitFor(t, s, "running", state(StateRunning))
	if !s.Status().ConsolesLive {
		t.Error("consoles not live without ViaVersion")
	}
	waitConsole(t, s, "Couldn't check for a newer ViaVersion")
	// A restore that brings back older plugins is noticed.
	os.WriteFile(filepath.Join(s.dir(), "plugins", "Geyser-Spigot.jar"), []byte("old geyser"), 0o644)
	s.notePluginFiles()
	m.LatestConsolePlugins()
	waitFor(t, s, "update offered", func(st Status) bool { return st.ConsolesUpdate })
}

// Overnight, a held Java server gets new console support but keeps its
// Minecraft version.
func TestNightlyConsoleSupport(t *testing.T) {
	f := newFakeJavaWorld(t)
	m := newJavaManager(t, f)
	st, err := m.CreateJava("Held", 0)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m.Get(st.ID)
	waitFor(t, s, "running", state(StateRunning))
	s.SetHoldVersion(true)
	f.mu.Lock()
	f.build, f.geyser, f.geyserB = 9, "2.12.0", 200
	f.mu.Unlock()
	recheckPlugins(m)
	recheckPaper(m)
	waitFor(t, s, "both updates known", func(st Status) bool { return st.ConsolesUpdate && st.UpdateTo != "" })

	plan := Nightly{AutoUpdate: true, At: "23:30"}
	in := time.Date(2026, 10, 1, 1, 0, 0, 0, time.Local)
	s.nightly(in, plan, "")
	waitConsole(t, s, "Updating console support overnight")
	waitFor(t, s, "updated", func(st Status) bool { return !st.ConsolesUpdate && st.State == StateRunning })
	if st := s.Status(); st.Build != 5 {
		t.Errorf("a held server's Paper was updated: %+v", st)
	}
	if got := readFile(t, filepath.Join(s.dir(), "plugins", "Geyser-Spigot.jar")); got != "geyser 2.12.0 build 200" {
		t.Errorf("geyser: %q", got)
	}
}

// recheckPlugins makes the next look at the newest console plugins ask
// again (after a check already under way, which may have old answers).
func recheckPlugins(m *Manager) {
	for {
		m.plugins.mu.Lock()
		if !m.plugins.busy {
			m.plugins.at = time.Time{}
			m.plugins.mu.Unlock()
			m.LatestConsolePlugins()
			return
		}
		m.plugins.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
}

func recheckPaper(m *Manager) {
	for {
		m.paper.mu.Lock()
		if !m.paper.busy {
			m.paper.at = time.Time{}
			m.paper.mu.Unlock()
			m.LatestPaper(true)
			return
		}
		m.paper.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
}

// Geyser always gets the server's port, even when it isn't fully set up (a
// restore, a failed Floodgate download): never its default, the console
// list's. With consoles off, leftover jars are taken out before a start.
func TestGeyserNeverOnItsDefaultPort(t *testing.T) {
	f := newFakeJavaWorld(t)
	m := newJavaManager(t, f)
	st, err := m.CreateJava("Ports", 0)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m.Get(st.ID)
	waitFor(t, s, "running", state(StateRunning))
	s.Stop()
	waitFor(t, s, "stopped", state(StateStopped))
	plugins := filepath.Join(s.dir(), "plugins")
	os.Remove(filepath.Join(plugins, "floodgate-spigot.jar"))
	s.Start()
	waitFor(t, s, "running", state(StateRunning))
	if s.Status().ConsolesLive {
		t.Error("live without Floodgate")
	}
	waitConsole(t, s, fmt.Sprintf("-DgeyserUdpPort=%d", st.Port))

	// Off, and a restore brings Geyser back: it's taken out before the start.
	s.Stop()
	waitFor(t, s, "stopped", state(StateStopped))
	s.mu.Lock()
	s.meta.NoConsoles = true
	s.mu.Unlock()
	os.WriteFile(filepath.Join(plugins, "Geyser-Spigot.jar"), []byte("restored"), 0o644)
	s.Start()
	waitFor(t, s, "running", state(StateRunning))
	if fileExists(filepath.Join(plugins, "Geyser-Spigot.jar")) {
		t.Error("Geyser left in with consoles off")
	}
}

// When console support is all an update is for and it can't be downloaded,
// the update says it failed (and the overnight one isn't reported as done).
func TestConsoleOnlyUpdateFailure(t *testing.T) {
	f := newFakeJavaWorld(t)
	m := newJavaManager(t, f)
	st, err := m.CreateJava("Fails", 0)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m.Get(st.ID)
	waitFor(t, s, "running", state(StateRunning))
	f.mu.Lock()
	f.geyser, f.badJars = "2.12.0", true
	f.mu.Unlock()
	if err := s.UpdateOvernight(); err != nil {
		t.Fatal(err)
	}
	waitConsole(t, s, "Update failed")
	waitFor(t, s, "running again", state(StateRunning))
	if !consoleHas(s, "couldn't be updated (the one installed is kept)") {
		t.Error("no reason given")
	}
	if !s.Status().ConsolesLive {
		t.Error("the old Geyser should still work")
	}
}

// Changes queued while the server started: console removals are reloaded
// before anything else makes the server save its own copy of the lists,
// and a removed operator is taken off straight away too.
func TestQueuedConsoleChangesOrder(t *testing.T) {
	f := newFakeJavaWorld(t)
	m := newJavaManager(t, f)
	st, err := m.CreateJava("Order", 0)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m.Get(st.ID)
	waitFor(t, s, "running", state(StateRunning))
	s.mu.Lock()
	s.meta.JavaPending = []JavaPending{{Name: "Steve", Action: "allow"}, {Name: ".Kid_One", Action: "unallow"}, {Name: ".Kid_One", Action: "deop"}}
	s.applyJavaPendingLocked()
	left := append([]JavaPending(nil), s.meta.JavaPending...)
	s.mu.Unlock()
	waitConsole(t, s, "got: deop .Kid_One")
	lines, _, cancel := s.Subscribe()
	cancel()
	all := strings.Join(lines, "\n")
	if r, a := strings.Index(all, "> whitelist reload"), strings.Index(all, "> whitelist add Steve"); r < 0 || a < 0 || r > a {
		t.Errorf("order:\n%s", all)
	}
	// Steve stays queued until the server confirms him.
	if len(left) != 2 || left[0].Name != "Steve" || left[1].Action != "deop" {
		t.Errorf("kept %+v", left)
	}
	waitConsole(t, s, "Added Steve to the whitelist")
	s.mu.Lock()
	left = append([]JavaPending(nil), s.meta.JavaPending...)
	s.mu.Unlock()
	if len(left) != 1 || left[0].Action != "deop" {
		t.Errorf("after the server confirmed Steve: %+v", left)
	}
}

// A Java server with console players gets the newest Minecraft version that
// Geyser supports, not a newer one it can't load on. Without consoles it gets
// the newest; turned on later, it says it's waiting for Geyser.
func TestJavaVersionGeyserSupports(t *testing.T) {
	f := newFakeJavaWorld(t)
	f.newer, f.geyserMC = "1.21.5", []string{"1.21.3", "1.21.4"}
	m := newJavaManager(t, f)
	st, err := m.CreateJava("Consoles", 0)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m.Get(st.ID)
	st = waitFor(t, s, "running", state(StateRunning))
	if st.Version != "1.21.4" || st.ConsolesWaiting != "" {
		t.Errorf("with consoles: %+v", st)
	}
	waitConsole(t, s, "Geyser, which lets consoles join, doesn't support 1.21.5 yet")
	// Update doesn't move it past what Geyser supports.
	if err := s.Update(); err != nil {
		t.Fatal(err)
	}
	waitConsole(t, s, "is up to date")
	if v := s.Status().Version; v != "1.21.4" {
		t.Errorf("updated to %s", v)
	}

	st2, err := m.CreateJava("Java Only", 0)
	if err != nil {
		t.Fatal(err)
	}
	j, _ := m.Get(st2.ID)
	waitFor(t, j, "running", state(StateRunning))
	if err := j.SetConsoles(false); err != nil {
		t.Fatal(err)
	}
	if err := j.Update(); err != nil {
		t.Fatal(err)
	}
	st2 = waitFor(t, j, "updated", func(st Status) bool { return st.Version == "1.21.5" && st.State == StateRunning })
	if st2.ConsolesWaiting != "" {
		t.Errorf("waiting without consoles: %+v", st2)
	}
	if err := j.SetConsoles(true); err != nil {
		t.Fatal(err)
	}
	if w := j.Status().ConsolesWaiting; w != "1.21.5" {
		t.Errorf("waiting: %q", w)
	}

	// Geyser catches up: the server is fine again.
	f.mu.Lock()
	f.geyserMC = []string{"1.21.4", "1.21.5"}
	f.mu.Unlock()
	if _, err := m.geyserJavaVersions(t.Context()); err != nil {
		t.Fatal(err)
	}
	if w := j.Status().ConsolesWaiting; w != "" {
		t.Errorf("still waiting: %q", w)
	}
}
