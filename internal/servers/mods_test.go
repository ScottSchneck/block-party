package servers

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ScottSchneck/block-party/internal/notify"
)

func TestModMeta(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		loader, id, name, version string
		needs                     []string
		want                      string
	}{
		{"fabric", "sodium", "Sodium", "0.6.0", []string{"fabric", "cloth-config"}, "cloth-config,fabric-api"},
		{"forge", "create", "Create", "6.0.1", []string{"flywheel"}, "flywheel"},
		{"neoforge", "mekanism", "Mekanism", "10.7", nil, ""},
	} {
		p := filepath.Join(dir, c.id+".jar")
		os.WriteFile(p, modJar(c.loader, c.id, c.name, c.version, c.needs...), 0o644)
		id, name, version, needs, err := modMeta(p)
		if err != nil || id != c.id || name != c.name || version != c.version || strings.Join(needs, ",") != c.want {
			t.Errorf("%s: %q %q %q %v %v", c.loader, id, name, version, needs, err)
		}
	}
	p := filepath.Join(dir, "plugin.jar")
	os.WriteFile(p, pluginJarFor("Essentials", "2.0"), 0o644)
	if _, _, _, _, err := modMeta(p); err == nil || !strings.Contains(err.Error(), "isn't a mod") {
		t.Errorf("a plugin isn't a mod: %v", err)
	}
}

func TestMods(t *testing.T) {
	java := newFakeJavaWorld(t)
	f := newFakeLoaders(t)
	f.mods = map[string]*fakeMod{
		"fabric-api": {title: "Fabric API", modID: "fabric-api", version: "0.110.0", loader: "fabric"},
		"cloth":      {title: "Cloth Config", modID: "cloth-config", version: "15.0", loader: "fabric"},
		"storage":    {title: "Storage Drawers", modID: "storagedrawers", version: "1.0", loader: "fabric", needs: []string{"cloth", "fabric-api"}, needIDs: []string{"cloth-config", "fabric"}},
		"forgy":      {title: "Storage Forge", modID: "forgy", version: "1.0", loader: "forge"},
		"floodgate":  {title: "Floodgate", modID: "floodgate", version: "2.2", loader: "fabric"},
	}
	cf := newFakeCurseForge(t)
	allowFakeCurseForge(t, cf)
	u := f.srv.URL
	var mu sync.Mutex
	var sent []notify.Event
	m := New(Options{DataDir: t.TempDir(), StopTimeout: time.Second,
		Notify:   func(e notify.Event) { mu.Lock(); sent = append(sent, e); mu.Unlock() },
		PaperAPI: java.srv.URL + "/paper", AdoptiumAPI: java.srv.URL + "/adoptium", MojangVersions: u + "/manifest", MojangProfiles: java.srv.URL + "/profiles/",
		GeyserAPI: u + "/geyser", HangarAPI: java.srv.URL + "/hangar", XboxLookup: java.srv.URL + "/xbox/", ModrinthAPI: u + "/modrinth",
		PurpurAPI: u + "/purpur", FabricMeta: u + "/fabric", ForgePromos: u + "/forge/promos.json", ForgeMaven: u + "/forgemaven", NeoForgeMaven: u + "/neo",
		CurseForgeAPI: cf.srv.URL})
	if err := m.Load(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.StopAll)
	st, err := m.CreateJavaOf("Modded", 1024, JavaChoice{LoaderFabric, ""})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m.Get(st.ID)
	waitFor(t, s, "running", state(StateRunning))
	mods := filepath.Join(s.dir(), "mods")

	v, err := s.Plugins()
	if err != nil || v.Kind != "mods" || v.Loader != LoaderFabric {
		t.Fatalf("view: %+v %v", v, err)
	}
	for _, p := range v.Plugins {
		if !p.Console {
			t.Errorf("only console support yet: %+v", p)
		}
	}

	// Search: Fabric mods for 1.21.4, not Forge ones; Modrinth only without
	// a CurseForge key.
	res, _ := m.SearchMods(t.Context(), "storage", "1.21.4", LoaderFabric)
	if len(res) != 1 || res[0].ID != "storage" || res[0].Page != "https://modrinth.com/mod/storage" {
		t.Fatalf("search: %+v", res)
	}

	// Install: with the mod it needs; Fabric API comes with console support.
	if title, err := s.InstallPlugin(SourceModrinth, "storage"); err != nil || title != "Storage Drawers" {
		t.Fatalf("install: %q %v", title, err)
	}
	if !fileExists(filepath.Join(mods, "storage-1.0.jar")) || !fileExists(filepath.Join(mods, "cloth-15.0.jar")) || fileExists(filepath.Join(mods, "fabric-api-0.110.0.jar")) {
		entries, _ := os.ReadDir(mods)
		t.Fatalf("mods folder: %v", entries)
	}
	got := byFile(t, s)
	if p := got["storage-1.0.jar"]; p.Source != SourceModrinth || p.Key != "storagedrawers" || strings.Join(p.Needs, ",") != "cloth-config,fabric-api" {
		t.Errorf("storage: %+v", p)
	}
	if p := got["cloth-15.0.jar"]; p.For != "Storage Drawers" || p.Name != "Cloth Config" {
		t.Errorf("cloth: %+v", p)
	}
	if _, err := s.InstallPlugin(SourceModrinth, "floodgate"); err == nil || !strings.Contains(err.Error(), "console support") {
		t.Errorf("console support's mod: %v", err)
	}

	// An update.
	f.mu.Lock()
	f.mods["storage"].version = "1.1"
	f.mu.Unlock()
	if u := s.PluginUpdates(t.Context()); u["storage-1.0.jar"] != "1.1" {
		t.Fatalf("updates: %v", u)
	}
	// The daily note: once, until there's something new.
	updateNotes := func() []notify.Event {
		mu.Lock()
		defer mu.Unlock()
		var out []notify.Event
		for _, e := range sent {
			if e.Kind == notify.UpdatesOut {
				out = append(out, e)
			}
		}
		return out
	}
	s.noteUpdates(t.Context())
	s.noteUpdates(t.Context())
	if n := updateNotes(); len(n) != 1 || n[0].Title != "Updates for Modded" || !strings.Contains(n[0].Text, "Storage Drawers 1.1") || !strings.Contains(n[0].Text, "Mods tab") {
		t.Fatalf("update notes: %+v", n)
	}
	if v, err := s.UpdatePlugin("storage-1.0.jar"); err != nil || v != "1.1" {
		t.Fatalf("update: %q %v", v, err)
	}
	if !fileExists(filepath.Join(mods, "storage-1.1.jar")) || fileExists(filepath.Join(mods, "storage-1.0.jar")) {
		t.Error("the update didn't replace the file")
	}
	if err := s.SetPluginEnabled("storage-1.1.jar", false); err != nil || !fileExists(filepath.Join(mods, "storage-1.1.jar.disabled")) {
		t.Errorf("turn off: %v", err)
	}
	if err := s.RemovePlugin("cloth-15.0.jar"); err != nil || fileExists(filepath.Join(mods, "cloth-15.0.jar")) {
		t.Errorf("remove: %v", err)
	}
	if err := s.RemovePlugin("fabric-api.jar"); err != errConsolePlugin {
		t.Errorf("console support's own file: %v", err)
	}

	// CurseForge, with a key: its Fabric file, not the newer Forge one; one
	// kept to CurseForge's site is left out.
	if err := m.SetCurseForgeKey(t.Context(), "good-key"); err != nil {
		t.Fatal(err)
	}
	res, _ = m.SearchMods(t.Context(), "pipes", "1.21.4", LoaderFabric)
	if len(res) != 1 || res[0].Source != SourceCurseForge || res[0].ID != "304" {
		t.Fatalf("CurseForge search: %+v", res)
	}
	if _, notes := m.SearchMods(t.Context(), "kept", "1.21.4", LoaderFabric); !strings.Contains(strings.Join(notes, " "), "1 more found on CurseForge can only be downloaded") {
		t.Errorf("kept notes: %v", notes)
	}
	if _, err := s.InstallPlugin(SourceCurseForge, "304"); err != nil {
		t.Fatal(err)
	}
	if p := byFile(t, s)["pipes-fabric.jar"]; p.Source != SourceCurseForge || p.Version != "pipes-fabric" || p.Key != "pipes" {
		t.Errorf("CurseForge mod: %+v", p)
	}
	if _, err := s.InstallPlugin(SourceHangar, "x"); err == nil {
		t.Error("Hangar is for plugins")
	}
}

func byFile(t *testing.T, s *Server) map[string]InstalledPlugin {
	t.Helper()
	v, err := s.Plugins()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]InstalledPlugin{}
	for _, p := range v.Plugins {
		out[p.File] = p
	}
	return out
}
