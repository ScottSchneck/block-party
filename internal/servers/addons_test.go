package servers

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeZip writes a zip with the given files (name -> content) to path.
func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func zipBytes(t *testing.T, files map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.zip")
	writeZip(t, p, files)
	b, _ := os.ReadFile(p)
	return string(b)
}

const behaviorManifest = `{
  // made by hand, with comments
  "format_version": 2,
  "header": {"name": "pack.name", "description": "pack.description", "uuid": "11111111-2222-3333-4444-555555555555", "version": [1, 2, 0],},
  "modules": [{"type": "data", "uuid": "aaaaaaaa-2222-3333-4444-555555555555", "version": [1, 0, 0]}],
  "dependencies": [{"module_name": "@minecraft/server", "version": "1.12.0-beta"}],
}`

const resourceManifest = `{"format_version": 3, "header": {"name": "§aCool Blocks", "uuid": "66666666-2222-3333-4444-555555555555", "version": "2.0.1"},
  "modules": [{"type": "resources", "uuid": "bbbbbbbb-2222-3333-4444-555555555555", "version": "2.0.1"}]}`

func TestAddons(t *testing.T) {
	m := newTestManager(t, newFakeMojang(t, "1.0.0.1"), time.Second)
	s := createRunning(t, m, "Family")
	ctx := context.Background()
	dir := t.TempDir()

	// An .mcaddon: a zip holding two .mcpack files.
	addon := filepath.Join(dir, "cool.mcaddon")
	writeZip(t, addon, map[string]string{
		"Cool BP.mcpack": zipBytes(t, map[string]string{
			"manifest.json":     behaviorManifest,
			"texts/en_US.lang":  "pack.name=Cool Mobs\npack.description=Mobs that are cool\t#comment\n",
			"entities/cow.json": "{}",
			"scripts/main.js":   "",
		}),
		"Cool RP.mcpack": zipBytes(t, map[string]string{
			"Cool RP/manifest.json":         resourceManifest,
			"Cool RP/textures/blocks/a.png": "png",
		}),
	})
	// Wrapped once more in a .zip, as sites often do.
	wrapped := filepath.Join(dir, "cool.zip")
	b0, _ := os.ReadFile(addon)
	writeZip(t, wrapped, map[string]string{"Cool Addon/cool.mcaddon": string(b0)})
	res, err := s.InstallPacks(ctx, wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Installed) != 2 || res.World != "Bedrock level" || !res.Running {
		t.Fatalf("result: %+v", res)
	}
	v, err := s.Packs()
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Packs) != 2 {
		t.Fatalf("packs: %+v", v.Packs)
	}
	bp, rp := v.Packs[0], v.Packs[1]
	if bp.Kind != "behavior" || bp.Name != "Cool Mobs" || bp.Description != "Mobs that are cool" || bp.Version != "1.2.0" || !bp.Enabled || !bp.NeedsBeta {
		t.Errorf("behavior pack: %+v", bp)
	}
	if rp.Kind != "resource" || rp.Name != "Cool Blocks" || rp.Version != "2.0.1" || !rp.Enabled || rp.NeedsBeta {
		t.Errorf("resource pack: %+v", rp)
	}
	wd := filepath.Join(s.dir(), "worlds", "Bedrock level")
	var on []worldPackEntry
	b, _ := os.ReadFile(filepath.Join(wd, "world_resource_packs.json"))
	json.Unmarshal(b, &on)
	if len(on) != 1 || on[0].PackID != rp.UUID || on[0].Version != [3]int{2, 0, 1} {
		t.Errorf("world_resource_packs.json: %s", b)
	}
	if _, err := os.Stat(filepath.Join(wd, "behavior_packs", bp.Folder, "entities", "cow.json")); err != nil {
		t.Error(err)
	}

	// Off, on, and an update of the same pack replaces it.
	if _, err := s.SetPackEnabled(bp.UUID, false); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(wd, "world_behavior_packs.json"))
	if strings.Contains(string(b), bp.UUID) {
		t.Errorf("still on: %s", b)
	}
	if _, err := s.SetPackEnabled(bp.UUID, true); err != nil {
		t.Fatal(err)
	}
	update := filepath.Join(dir, "update.mcpack")
	writeZip(t, update, map[string]string{"manifest.json": strings.Replace(resourceManifest, `"2.0.1"},`, `"2.1.0"},`, 1)})
	if _, err := s.InstallPacks(ctx, update); err != nil {
		t.Fatal(err)
	}
	v, _ = s.Packs()
	if len(v.Packs) != 2 || v.Packs[1].Version != "2.1.0" {
		t.Errorf("after update: %+v", v.Packs)
	}
	if _, err := s.RemovePack(rp.UUID); err != nil {
		t.Fatal(err)
	}
	v, _ = s.Packs()
	if len(v.Packs) != 1 {
		t.Errorf("after removing: %+v", v.Packs)
	}
	entries, _ := os.ReadDir(filepath.Join(wd, "resource_packs"))
	if len(entries) != 0 {
		t.Errorf("files left: %v", entries)
	}

	// Refused: unsafe names, worlds, skin packs, not zips; nothing left behind.
	for name, files := range map[string]map[string]string{
		"slip":  {"../../evil/manifest.json": resourceManifest},
		"world": {"level.dat": "x", "db/CURRENT": "x"},
		"skins": {"manifest.json": `{"header":{"name":"S","uuid":"77777777-2222-3333-4444-555555555555","version":[1,0,0]},"modules":[{"type":"skin_pack"}]}`},
		"none":  {"readme.txt": "hi"},
	} {
		p := filepath.Join(dir, name+".mcpack")
		writeZip(t, p, files)
		if _, err := s.InstallPacks(ctx, p); err == nil {
			t.Errorf("%s installed", name)
		}
	}
	os.WriteFile(filepath.Join(dir, "junk.mcpack"), []byte("not a zip"), 0o644)
	if _, err := s.InstallPacks(ctx, filepath.Join(dir, "junk.mcpack")); err == nil {
		t.Error("junk installed")
	}
	if _, err := os.Stat(filepath.Join(s.m.serversDir(), "..", "evil")); err == nil {
		t.Error("zip slip")
	}
	left, _ := filepath.Glob(filepath.Join(s.m.serversDir(), ".upload-*"))
	if len(left) != 0 {
		t.Errorf("staging left behind: %v", left)
	}
	// A pack missing its files still shows (and can be removed).
	os.RemoveAll(filepath.Join(wd, "behavior_packs"))
	v, _ = s.Packs()
	if len(v.Packs) != 1 || !v.Packs[0].Missing {
		t.Errorf("missing pack: %+v", v.Packs)
	}
	if _, err := s.RemovePack(bp.UUID); err != nil {
		t.Error(err)
	}
}

func TestWorlds(t *testing.T) {
	m := newTestManager(t, newFakeMojang(t, "1.0.0.1"), time.Second)
	s := createRunning(t, m, "Family")
	ctx := context.Background()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(s.dir(), "worlds", "Bedrock level", "db"), 0o755)
	os.WriteFile(filepath.Join(s.dir(), "worlds", "Bedrock level", "level.dat"), []byte("home"), 0o644)

	// A .mcworld with the world one folder down and a name in levelname.txt.
	up := filepath.Join(dir, "Castle.mcworld")
	writeZip(t, up, map[string]string{
		"My Castle/level.dat":     "castle",
		"My Castle/levelname.txt": "Ava's Castle: v2",
		"My Castle/db/000001.ldb": "data",
		"__MACOSX/._level.dat":    "junk",
	})
	res, err := s.AddWorld(ctx, up, "Castle.mcworld", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.World.Folder != "Ava's Castle v2" || res.Playing {
		t.Errorf("added: %+v", res)
	}
	// Uploading again doesn't replace it.
	res2, err := s.AddWorld(ctx, up, "Castle.mcworld", true)
	if err != nil {
		t.Fatal(err)
	}
	if res2.World.Folder != "Ava's Castle v2 2" || !res2.Playing {
		t.Errorf("second: %+v", res2)
	}
	waitFor(t, s, "running again", state(StateRunning))
	v, err := s.Worlds()
	if err != nil {
		t.Fatal(err)
	}
	if v.Current != "Ava's Castle v2 2" || len(v.Worlds) != 3 || !v.Worlds[0].Current {
		t.Errorf("worlds: %+v", v)
	}
	if err := s.PlayWorld("../../etc"); err == nil {
		t.Error("played a path")
	}
	if err := s.PlayWorld("Bedrock level"); err != nil {
		t.Fatal(err)
	}

	// Download: a zip with level.dat at the top.
	var buf bytes.Buffer
	waitFor(t, s, "running", state(StateRunning))
	if err := s.ExportWorld(ctx, "Ava's Castle v2", &buf); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["level.dat"] || !names["db/000001.ldb"] || names["__MACOSX/._level.dat"] {
		t.Errorf("exported: %v", names)
	}
	left, _ := filepath.Glob(filepath.Join(m.backupsDir(s.ID()), ".export-*"))
	if len(left) != 0 {
		t.Errorf("export left behind: %v", left)
	}

	// Delete: not the current world; the others after a backup.
	waitFor(t, s, "running", state(StateRunning))
	if err := s.DeleteWorld("Bedrock level"); err == nil {
		t.Error("deleted the current world")
	}
	if err := s.DeleteWorld("Ava's Castle v2 2"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.worldDir("Ava's Castle v2 2")); err == nil {
		t.Error("not deleted")
	}
	var kept string
	for _, b := range s.Backups().Backups {
		if b.Kind == "before-delete" {
			kept = b.Name
		}
	}
	if kept == "" {
		t.Fatal("no backup before deleting")
	}
	// Restoring it puts back just that world; the server keeps running.
	if err := s.Restore(kept); err != nil {
		t.Fatal(err)
	}
	if readFileT(t, filepath.Join(s.worldDir("Ava's Castle v2 2"), "level.dat")) != "castle" || s.Status().State != StateRunning {
		t.Error("deleted world not put back")
	}
	if err := s.Restore(kept); err == nil {
		t.Error("restored over an existing world")
	}
	// Odd names still make usable folders.
	odd := filepath.Join(dir, "odd.mcworld")
	writeZip(t, odd, map[string]string{"level.dat": "x", "levelname.txt": ". . hi\u0085"})
	res3, err := s.AddWorld(ctx, odd, "odd.mcworld", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PlayWorld(res3.World.Folder); err != nil {
		t.Errorf("odd folder %q: %v", res3.World.Folder, err)
	}
	// A pack upload is refused as a world, and a world as a pack.
	pk := filepath.Join(dir, "p.mcpack")
	writeZip(t, pk, map[string]string{"manifest.json": resourceManifest})
	if _, err := s.AddWorld(ctx, pk, "p.mcpack", false); err == nil || !strings.Contains(err.Error(), "add-on") {
		t.Errorf("pack as a world: %v", err)
	}
	if _, err := s.InstallPacks(ctx, up); err == nil || !strings.Contains(err.Error(), "world") {
		t.Errorf("world as a pack: %v", err)
	}
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAddonsNeedMinecraft(t *testing.T) {
	for _, c := range []struct {
		engine, server, want string
	}{
		{`[1, 21, 50]`, "1.26.52.3", ""}, {`[26, 60, 0]`, "1.26.52.3", "26.60"}, {`[1, 26, 60]`, "1.26.52.3", "1.26.60"},
		{`[1, 26, 50]`, "1.26.52.3", ""}, {`[26, 60, 0]`, "", ""}, {``, "1.26.52.3", ""},
	} {
		var m manifest
		if c.engine != "" {
			m.Header.MinEngine = json.RawMessage(c.engine)
		}
		if got := m.tooNewFor(c.server); got != c.want {
			t.Errorf("%s on %q: %q", c.engine, c.server, got)
		}
	}
}
