package servers

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCleanRelAndCode(t *testing.T) {
	for in, want := range map[string]string{"": ".", "/": ".", "a/b/": "a/b", "a//b": "a/b", "a/./b": "a/b", `a\b`: "a/b"} {
		if got, err := cleanRel(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"..", "../x", "a/../../x", "a/../b", "x\x00"} {
		if _, err := cleanRel(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
	for rel, want := range map[string]bool{
		"bedrock_server": true, "paper.jar": true, "plugins": true, "plugins/x.jar": true, "plugins/LuckPerms": true,
		"plugins/LuckPerms/config.yml": false, "plugins/a/b/c.jar": true, "mods/fabric-api.jar": true, "libraries/x/y.txt": true,
		"server.properties": false, "worlds/Bedrock level/level.dat": false, "config/x.json": false, "lib.so": true, "x/run.sh": true,
		"world/datapacks/pack.zip": false, "behavior_packs/a/scripts/main.js": false,
	} {
		if codeFile(rel) != want {
			t.Errorf("codeFile(%q) = %v", rel, !want)
		}
	}
}

func TestFileManager(t *testing.T) {
	m := newTestManager(t, newFakeMojang(t, "1.0.0.1"), 2*time.Second)
	s := createRunning(t, m, "Files")
	dir := s.dir()
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644)
	_ = os.Symlink(outside, filepath.Join(dir, "escape"))
	_ = os.WriteFile(filepath.Join(dir, "bin.dat"), []byte{1, 0, 2}, 0o644)

	list, err := s.ListFiles("")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]FileEntry{}
	for _, e := range list {
		names[e.Name] = e
	}
	if _, ok := names[metaFile]; ok {
		t.Error("the panel's records must be hidden")
	}
	if !names["server.properties"].Editable || names["bin.dat"].Editable || !names["bedrock_server"].Locked || !names["config"].Dir {
		t.Errorf("listing: %+v", list)
	}
	if list[0].Dir != true {
		t.Error("folders come first")
	}

	// Nothing outside the folder, by path or symlink.
	if _, err := s.ReadText("escape/secret.txt"); err == nil {
		t.Error("a symlink out of the folder must not be followed")
	}
	if _, err := s.ListFiles("escape"); err == nil {
		t.Error("listing through a symlink out of the folder must fail")
	}
	if err := s.WriteText("escape/new.txt", "x", time.Time{}, true); err == nil {
		t.Error("writing through a symlink out must fail")
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); err == nil {
		t.Fatal("a file was written outside the server's folder")
	}
	if _, err := s.ReadText(metaFile); err == nil {
		t.Error("the panel's records can't be opened")
	}
	if err := s.WriteText(metaFile, "{}", time.Time{}, true); err == nil {
		t.Error("the panel's records can't be written")
	}

	// Edit with a change check.
	ft, err := s.ReadText("server.properties")
	if err != nil || !strings.Contains(ft.Text, "server-port") {
		t.Fatalf("%v %q", err, ft.Text)
	}
	if err := s.WriteText("server.properties", ft.Text+"motd=hi\n", ft.ModTime, false); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteText("server.properties", "x", ft.ModTime.Add(-time.Hour), false); !errors.Is(err, ErrFileChanged) {
		t.Errorf("a stale edit should be refused: %v", err)
	}

	// Code: owner only.
	if err := s.SaveUpload("", "bedrock_server", strings.NewReader("evil"), false); !errors.Is(err, ErrFileNotAllowed) {
		t.Errorf("a kid replacing the server program: %v", err)
	}
	if err := s.MakeFolder("plugins", false); !errors.Is(err, ErrFileNotAllowed) {
		t.Errorf("plugins folder: %v", err)
	}
	if err := s.SaveUpload("config", "x.jar", strings.NewReader("x"), false); !errors.Is(err, ErrFileNotAllowed) {
		t.Errorf("a jar anywhere: %v", err)
	}
	if err := s.SaveUpload("", "notes.txt", strings.NewReader("hello"), false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "notes.txt")); string(b) != "hello" {
		t.Errorf("upload: %q", b)
	}
	if err := s.SaveUpload("", "../up.txt", strings.NewReader("x"), true); err != nil {
		t.Errorf("a name with a path is reduced to its last part: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "up.txt")); err != nil {
		t.Error("the upload should land in the folder")
	}

	// Folders: kids can make them but not delete or move them.
	if err := s.MakeFolder("notes", false); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameFile("notes.txt", "notes/today.txt", false); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameFile("notes", "notes2", false); !errors.Is(err, ErrFileNotAllowed) {
		t.Errorf("a kid moving a folder: %v", err)
	}
	if err := s.RenameFile("notes", "notes/inner", true); err == nil {
		t.Error("a folder can't go inside itself")
	}
	if err := s.DeleteFile("notes", false); !errors.Is(err, ErrFileNotAllowed) {
		t.Errorf("a kid deleting a folder: %v", err)
	}
	if err := s.DeleteFile("notes/today.txt", false); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFile("notes", true); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFile("", true); err == nil {
		t.Error("the server's folder itself can't be deleted")
	}
	f, st, err := s.OpenDownload("server.properties")
	if err != nil || st.Size() == 0 {
		t.Fatal(err)
	}
	f.Close()
	if _, _, err := s.OpenDownload("config"); err == nil {
		t.Error("folders can't be downloaded as files")
	}
}
