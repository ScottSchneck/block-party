package offsite

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests use the real rclone when it's installed.
func needRclone(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rclone"); err != nil {
		t.Skip("rclone isn't installed")
	}
}

type fakePanel struct {
	mu      sync.Mutex
	dir     string
	backups []LocalBackup
	live    map[string]bool
	failed  []string
}

func (f *fakePanel) add(t *testing.T, server string, when time.Time, body string) LocalBackup {
	t.Helper()
	name := fmt.Sprintf("%s-%s-scheduled.tar.gz", server, when.Format("20060102-150405"))
	dir := filepath.Join(f.dir, "backups", server)
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	b := LocalBackup{Server: server, Name: name, Path: p, Time: when, Size: int64(len(body))}
	f.mu.Lock()
	f.backups = append(f.backups, b)
	f.mu.Unlock()
	return b
}

func newTestManager(t *testing.T) (*Manager, *fakePanel) {
	t.Helper()
	f := &fakePanel{dir: t.TempDir(), live: map[string]bool{"kids": true}}
	m := New(Options{
		DataDir: f.dir,
		Backups: func() []LocalBackup {
			f.mu.Lock()
			defer f.mu.Unlock()
			return append([]LocalBackup(nil), f.backups...)
		},
		Note: func(id string) []byte {
			b, _ := json.Marshal(map[string]any{"meta": map[string]any{"id": id, "name": "Kids World", "type": "bedrock", "port": 19140}})
			return b
		},
		Live:   func(id string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.live[id] },
		Failed: func(title, text string) { f.mu.Lock(); f.failed = append(f.failed, title+": "+text); f.mu.Unlock() },
	})
	return m, f
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func TestCopiesToAFolder(t *testing.T) {
	needRclone(t)
	m, f := newTestManager(t)
	ctx := context.Background()
	dest := t.TempDir()
	base := time.Date(2026, 10, 20, 3, 0, 0, 0, time.Local)
	for i := 0; i < 5; i++ {
		f.add(t, "kids", base.Add(time.Duration(i)*24*time.Hour), fmt.Sprintf("world %d", i))
	}
	if _, err := m.Connect(ctx, ConnectRequest{Kind: KindFolder, Path: "relative"}); err == nil {
		t.Error("a relative path")
	}
	if _, err := m.Connect(ctx, ConnectRequest{Kind: KindFolder, Path: "/no/such/folder"}); err == nil {
		t.Error("a missing folder")
	}
	step, err := m.Connect(ctx, ConnectRequest{Kind: KindFolder, Path: dest, Keep: 3})
	if err != nil || step.Step != "done" {
		t.Fatalf("connect: %+v %v", step, err)
	}
	if fi, err := os.Stat(m.confPath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("config file: %v %v", fi, err)
	}
	if s := m.Settings(); !s.On || s.Kind != KindFolder || s.Folder != DefaultFolder || s.Keep != 3 {
		t.Errorf("settings %+v", s)
	}
	if err := m.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	got := names(t, filepath.Join(dest, DefaultFolder))
	want := []string{"kids/kids-20261022-030000-scheduled.tar.gz", "kids/kids-20261023-030000-scheduled.tar.gz", "kids/kids-20261024-030000-scheduled.tar.gz", "kids/server.json"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("copied %v", got)
	}
	if st := m.Status(); st.Copied != 3 || st.LastError != "" || st.LastOK.IsZero() {
		t.Errorf("status %+v", st)
	}
	// Nothing new: nothing copied.
	m.Pass(ctx)
	if st := m.Status(); st.Copied != 0 || st.Removed != 0 {
		t.Errorf("second pass %+v", st)
	}
	// A new backup: it's copied and the oldest copy goes.
	f.add(t, "kids", base.Add(5*24*time.Hour), "world 5")
	m.Pass(ctx)
	if st := m.Status(); st.Copied != 1 || st.Removed != 1 {
		t.Errorf("third pass %+v", st)
	}
	got = names(t, filepath.Join(dest, DefaultFolder, "kids"))
	if len(got) != 4 || got[0] != "kids-20261023-030000-scheduled.tar.gz" {
		t.Errorf("after a new backup %v", got)
	}

	// What's off-site, and getting one back for a server that's gone.
	list, err := m.List(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "Kids World" || len(list[0].Copies) != 3 || !list[0].Copies[0].Local || !list[0].Live {
		t.Fatalf("list %+v %v", list, err)
	}
	os.RemoveAll(filepath.Join(f.dir, "backups"))
	f.mu.Lock()
	f.backups, f.live = nil, map[string]bool{}
	f.mu.Unlock()
	if err := m.Download(ctx, "kids", "../etc/passwd"); err == nil {
		t.Error("a bad name")
	}
	if err := m.Download(ctx, "kids", "kids-20261025-030000-scheduled.tar.gz"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(f.dir, "backups", "kids", "kids-20261025-030000-scheduled.tar.gz")); string(b) != "world 5" {
		t.Errorf("downloaded %q", b)
	}
	var note map[string]any
	b, _ := os.ReadFile(filepath.Join(f.dir, "backups", "kids", "deleted-server.json"))
	if json.Unmarshal(b, &note) != nil || note["final"] != "kids-20261025-030000-scheduled.tar.gz" || note["meta"].(map[string]any)["name"] != "Kids World" {
		t.Errorf("note %s", b)
	}
	if err := m.Download(ctx, "kids", "kids-20261025-030000-scheduled.tar.gz"); err == nil {
		t.Error("downloaded twice")
	}

	// Off: nothing happens. Disconnect forgets the credentials.
	if _, err := m.SetOptions(false, 3); err != nil {
		t.Fatal(err)
	}
	if err := m.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.confPath()); err == nil {
		t.Error("the config file is still there")
	}
	if s := m.Settings(); s.Kind != "" || s.On {
		t.Errorf("after disconnect %+v", s)
	}
}

func TestEncryptedCopies(t *testing.T) {
	needRclone(t)
	m, f := newTestManager(t)
	ctx := context.Background()
	dest := t.TempDir()
	f.add(t, "kids", time.Date(2026, 10, 25, 3, 0, 0, 0, time.Local), "secret world")
	if _, err := m.Connect(ctx, ConnectRequest{Kind: KindFolder, Path: dest, Encrypt: true, Passphrase: "short"}); err == nil {
		t.Error("a short passphrase")
	}
	if _, err := m.Connect(ctx, ConnectRequest{Kind: KindFolder, Path: dest, Folder: "Family", Encrypt: true, Passphrase: "correct horse battery"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	got := names(t, filepath.Join(dest, "Family"))
	if len(got) != 2 {
		t.Fatalf("files %v", got)
	}
	for _, n := range got {
		if strings.Contains(n, "kids") || strings.Contains(n, "server.json") {
			t.Errorf("a name isn't encrypted: %s", n)
		}
		b, _ := os.ReadFile(filepath.Join(dest, "Family", n))
		if strings.Contains(string(b), "secret world") {
			t.Error("the contents aren't encrypted")
		}
	}
	list, err := m.List(ctx)
	if err != nil || len(list) != 1 || list[0].Copies[0].Name != "kids-20261025-030000-scheduled.tar.gz" || list[0].Copies[0].Size != int64(len("secret world")) {
		t.Fatalf("list %+v %v", list, err)
	}
	// The same backup isn't copied again.
	m.Pass(ctx)
	if st := m.Status(); st.Copied != 0 {
		t.Errorf("copied again: %+v", st)
	}
}

// A failure is told once, with rclone's reason, and the old connection
// keeps working when a new one fails.
func TestFailuresAndBadConnections(t *testing.T) {
	needRclone(t)
	m, f := newTestManager(t)
	ctx := context.Background()
	dest := t.TempDir()
	f.add(t, "kids", time.Date(2026, 10, 25, 3, 0, 0, 0, time.Local), "world")
	if _, err := m.Connect(ctx, ConnectRequest{Kind: KindFolder, Path: dest}); err != nil {
		t.Fatal(err)
	}
	// B2 with made-up keys can't be checked: the folder connection stays.
	if _, err := m.Connect(ctx, ConnectRequest{Kind: KindB2, KeyID: "x", Key: "y", Path: "bucket"}); err == nil {
		t.Error("made-up keys connected")
	}
	if s := m.Settings(); s.Kind != KindFolder {
		t.Errorf("the working connection was replaced: %+v", s)
	}
	if err := m.Pass(ctx); err != nil {
		t.Fatalf("the old connection broke: %v", err)
	}
	// The folder disappears: one notice, however many passes fail.
	os.RemoveAll(dest)
	os.WriteFile(dest, []byte("not a folder"), 0o644)
	f.add(t, "kids", time.Date(2026, 10, 26, 3, 0, 0, 0, time.Local), "world 2")
	for i := 0; i < 3; i++ {
		if err := m.Pass(ctx); err == nil {
			t.Fatal("copying into a file worked")
		}
	}
	if len(f.failed) != 1 || !strings.Contains(f.failed[0], "Off-site backup copy failed") {
		t.Errorf("notices %v", f.failed)
	}
	if st := m.Status(); st.LastError == "" {
		t.Error("no error shown")
	}
}

func TestAutoAnswers(t *testing.T) {
	read := func(s string) configState {
		var cs configState
		if err := json.Unmarshal([]byte(s), &cs); err != nil {
			t.Fatal(err)
		}
		return cs
	}
	for js, want := range map[string]string{
		`{"State":"x","Option":{"Name":"config_refresh_token","Default":true}}`:                        "false",
		`{"State":"x","Option":{"Name":"config_shared_client_id","Default":false}}`:                    "true",
		`{"State":"x","Option":{"Name":"config_driveid","Default":"","Examples":[{"Value":"b!abc"}]}}`: "b!abc",
		`{"State":"x","Option":{"Name":"config_something","Default":false}}`:                           "false",
	} {
		if got, ok := read(js).autoAnswer(); !ok || got != want {
			t.Errorf("%s: %q %v", js, got, ok)
		}
	}
	if _, ok := read(`{"State":"x","Option":{"Name":"config_2fa","Default":""}}`).autoAnswer(); ok {
		t.Error("the 2FA code was answered by itself")
	}
}

func TestPastedAddress(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	if _, err := m.FinishSignIn(ctx, "http://127.0.0.1:53682/?code=x&state=y"); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("no sign-in going: %v", err)
	}
	m.auth = &authFlow{done: make(chan struct{}), cancel: func() {}, out: &lockedBuffer{}}
	close(m.auth.done)
	for _, bad := range []string{"https://evil.example/?code=x&state=y", "http://127.0.0.1:8080/?code=x&state=y", "http://127.0.0.1:53682/?state=y"} {
		if _, err := m.FinishSignIn(ctx, bad); err == nil {
			t.Errorf("%s was taken", bad)
		}
	}
}

func TestRcloneError(t *testing.T) {
	stderr := "Usage:\n  rclone config create\n\n2026/10/26 00:40:00 NOTICE: Fatal error: authStart: Forbidden\n"
	if got := rcloneError(stderr, fmt.Errorf("exit status 1")); got != "Fatal error: authStart: Forbidden" {
		t.Errorf("%q", got)
	}
}

// Signing in to Google Drive: rclone's own page leads to Google, asking only
// for the files rclone makes.
func TestSignInLink(t *testing.T) {
	needRclone(t)
	m, _ := newTestManager(t)
	step, err := m.Connect(context.Background(), ConnectRequest{Kind: KindDrive})
	if err != nil {
		t.Fatal(err)
	}
	defer m.stopAuth()
	if step.Step != "oauth" || !strings.HasPrefix(step.URL, "https://accounts.google.com/") || !strings.Contains(step.URL, "drive.file") {
		t.Errorf("step %+v", step)
	}
}
