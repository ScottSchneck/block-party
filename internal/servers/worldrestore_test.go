package servers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func putWorld(t *testing.T, s *Server, folder, body string) {
	t.Helper()
	if err := os.MkdirAll(s.worldDir(folder), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.worldDir(folder), "level.dat"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// backupNow makes a manual backup of a stopped server and returns its name.
func backupNow(t *testing.T, s *Server) string {
	t.Helper()
	p, err := writeWorldBackup(t.Context(), s.dir(), s.m.backupsDir(s.meta.ID), s.meta.ID, "manual")
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Base(p)
}

func TestRestoreOneWorld(t *testing.T) {
	m := newTestManager(t, newFakeMojang(t, "1.26.52.3"), 2*time.Second)
	s := createRunning(t, m, "Family")
	_ = s.Stop()
	waitFor(t, s, "stopped", state(StateStopped))
	s.filesMu.Lock()
	current, _, err := s.currentWorld()
	s.filesMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	putWorld(t, s, current, "played 1")
	putWorld(t, s, "Other", "other 1")
	name := backupNow(t, s)

	putWorld(t, s, current, "played 2")
	putWorld(t, s, "Other", "other 2")
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, s, "running", state(StateRunning))

	worlds, err := s.BackupWorlds(name)
	if err != nil || strings.Join(worlds, ",") != strings.Join(sortedUnique([]string{current, "Other"}), ",") {
		t.Fatalf("worlds in the backup: %v %v", worlds, err)
	}
	// Not the one being played: the server keeps running.
	if err := s.RestoreWorld(name, "Other"); err != nil {
		t.Fatal(err)
	}
	waitConsole(t, s, "Put back the world Other")
	if got := readFile(t, filepath.Join(s.worldDir("Other"), "level.dat")); got != "other 1" {
		t.Errorf("other: %q", got)
	}
	if got := readFile(t, filepath.Join(s.worldDir(current), "level.dat")); got != "played 2" {
		t.Errorf("the played world changed: %q", got)
	}
	if st := s.Status(); st.State != StateRunning {
		t.Errorf("the server stopped: %+v", st)
	}
	var undo string
	for _, b := range s.Backups().Backups {
		if b.Kind == "before-restore" {
			undo = b.Name
		}
	}
	if w, _ := s.BackupWorlds(undo); strings.Join(w, ",") != "Other" {
		t.Errorf("the undo backup holds %v", w)
	}

	// The one being played: it stops, and starts again.
	if err := s.RestoreWorld(name, current); err != nil {
		t.Fatal(err)
	}
	waitConsole(t, s, "Put back the world "+current)
	waitFor(t, s, "running again", state(StateRunning))
	if got := readFile(t, filepath.Join(s.worldDir(current), "level.dat")); got != "played 1" {
		t.Errorf("played: %q", got)
	}
	if err := s.RestoreWorld(name, "Nope"); err == nil {
		t.Error("a world the backup doesn't have")
	}
}

func TestRestoreJavaWorld(t *testing.T) {
	f := newFakeJavaWorld(t)
	m := newJavaManager(t, f)
	st, err := m.CreateJava("Java Fun", 1024)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m.Get(st.ID)
	waitFor(t, s, "running", state(StateRunning))
	_ = s.Stop()
	waitFor(t, s, "stopped", state(StateStopped))
	for _, d := range []string{"world", "world_nether", "world_the_end", "creative"} {
		putWorld(t, s, d, d+" 1")
	}
	name := backupNow(t, s)
	for _, d := range []string{"world", "world_nether", "world_the_end"} {
		putWorld(t, s, d, d+" 2")
	}
	if w, _ := s.BackupWorlds(name); strings.Join(w, ",") != "creative,world" {
		t.Fatalf("a world's dimensions are part of it: %v", w)
	}
	if err := s.RestoreWorld(name, "world"); err != nil {
		t.Fatal(err)
	}
	waitConsole(t, s, "Put back the world world")
	for _, d := range []string{"world", "world_nether", "world_the_end"} {
		if got := readFile(t, filepath.Join(s.worldDir(d), "level.dat")); got != d+" 1" {
			t.Errorf("%s: %q", d, got)
		}
	}
	if err := s.RestoreWorld(name, "world_nether"); err == nil {
		t.Error("a dimension isn't a world of its own")
	}
}
