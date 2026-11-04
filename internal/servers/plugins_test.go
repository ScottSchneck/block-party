package servers

import (
	"archive/zip"
	"bytes"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePlugin is a plugin on the fake Hangar or Modrinth.
type fakePlugin struct {
	source, id, title, name, version string
	needs                            []string // IDs on the same site
	external                         bool     // Hangar: only on its own site
	damaged                          bool
}

// pluginJarFor makes a jar with a plugin.yml.
func pluginJarFor(name, version string) []byte {
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	w, _ := z.Create("plugin.yml")
	fmt.Fprintf(w, "name: %s\nversion: '%s'\nmain: x.Main\ncommands:\n  name: nested\n", name, version)
	z.Close()
	return buf.Bytes()
}

func (f *fakeJavaWorld) plugin(source, id string) *fakePlugin {
	for _, p := range f.fakePlugins {
		if p.source == source && p.id == id {
			return p
		}
	}
	return nil
}

// servePlugins answers as Hangar's and Modrinth's APIs do. f.mu is held.
func (f *fakeJavaWorld) servePlugins(w http.ResponseWriter, r *http.Request) {
	p, q := r.URL.Path, r.URL.Query()
	enc := json.NewEncoder(w)
	switch {
	case p == "/hangar":
		var out []map[string]any
		for _, x := range f.fakePlugins {
			if x.source == SourceHangar && strings.Contains(strings.ToLower(x.title), strings.ToLower(q.Get("q"))) && q.Get("platform") == "PAPER" {
				out = append(out, map[string]any{"name": x.title, "namespace": map[string]string{"owner": "someone", "slug": x.id},
					"stats": map[string]int{"downloads": 500}, "description": "a " + x.title, "avatarUrl": "https://hangarcdn.papermc.io/x.webp"})
			}
		}
		enc.Encode(map[string]any{"result": out})
	case strings.HasPrefix(p, "/hangar/") && strings.HasSuffix(p, "/versions"):
		x := f.plugin(SourceHangar, strings.TrimSuffix(strings.TrimPrefix(p, "/hangar/"), "/versions"))
		if x == nil || q.Get("platformVersion") != f.version {
			enc.Encode(map[string]any{"result": []any{}})
			return
		}
		jar := pluginJarFor(x.name, x.version)
		var deps []map[string]any
		for _, n := range x.needs {
			var id int64
			fmt.Sscan(n, &id)
			deps = append(deps, map[string]any{"name": "dep", "projectId": id, "required": true})
		}
		deps = append(deps, map[string]any{"name": "Optional", "required": false, "externalUrl": "https://example.com"})
		d := map[string]any{"fileInfo": map[string]any{"name": x.name + "-" + x.version + ".jar", "sha256Hash": sha(jar)}, "downloadUrl": f.srv.URL + "/pluginjar/" + x.source + "/" + x.id}
		if x.external {
			d = map[string]any{"externalUrl": "https://example.com/" + x.id}
		}
		enc.Encode(map[string]any{"result": []any{map[string]any{"name": x.version, "channel": map[string]string{"name": "Release"},
			"downloads": map[string]any{"PAPER": d}, "pluginDependencies": map[string]any{"PAPER": deps}}}})
	case strings.HasPrefix(p, "/hangar/"):
		x := f.plugin(SourceHangar, strings.TrimPrefix(p, "/hangar/"))
		if x == nil {
			w.WriteHeader(404)
			return
		}
		enc.Encode(map[string]any{"name": x.title, "namespace": map[string]string{"owner": "someone", "slug": x.id}})
	case p == "/modrinth/search":
		var out []map[string]any
		for _, x := range f.fakePlugins {
			if x.source == SourceModrinth && strings.Contains(strings.ToLower(x.title), strings.ToLower(q.Get("query"))) && strings.Contains(q.Get("facets"), "versions:"+f.version) {
				out = append(out, map[string]any{"project_id": x.id, "slug": strings.ToLower(x.title), "title": x.title, "author": "dev", "downloads": 900})
			}
		}
		enc.Encode(map[string]any{"hits": out})
	case strings.HasPrefix(p, "/modrinth/project/") && strings.HasSuffix(p, "/version"):
		x := f.plugin(SourceModrinth, strings.TrimSuffix(strings.TrimPrefix(p, "/modrinth/project/"), "/version"))
		if x == nil || !strings.Contains(q.Get("loaders"), `"paper"`) || q.Get("game_versions") != `["`+f.version+`"]` {
			enc.Encode([]any{})
			return
		}
		jar := pluginJarFor(x.name, x.version)
		h := sha512.Sum512(jar)
		var deps []map[string]any
		for _, n := range x.needs {
			deps = append(deps, map[string]any{"project_id": n, "dependency_type": "required"})
		}
		deps = append(deps, map[string]any{"project_id": "nope", "dependency_type": "optional"})
		enc.Encode([]any{
			map[string]any{"version_number": x.version + "-beta", "version_type": "beta", "files": []any{}},
			map[string]any{"version_number": x.version, "version_type": "release", "dependencies": deps, "files": []any{
				map[string]any{"url": f.srv.URL + "/pluginjar/sources.jar", "filename": "sources.jar", "hashes": map[string]string{"sha512": strings.Repeat("0", 128)}},
				map[string]any{"url": f.srv.URL + "/pluginjar/" + x.source + "/" + x.id, "filename": x.name + ".jar", "primary": true, "hashes": map[string]string{"sha512": hex.EncodeToString(h[:])}},
			}},
		})
	case strings.HasPrefix(p, "/modrinth/project/"):
		x := f.plugin(SourceModrinth, strings.TrimPrefix(p, "/modrinth/project/"))
		if x == nil {
			w.WriteHeader(404)
			return
		}
		enc.Encode(map[string]any{"id": x.id, "slug": strings.ToLower(x.title), "title": x.title})
	case strings.HasPrefix(p, "/pluginjar/"):
		parts := strings.Split(strings.TrimPrefix(p, "/pluginjar/"), "/")
		if len(parts) != 2 {
			w.WriteHeader(404)
			return
		}
		x := f.plugin(parts[0], parts[1])
		if x == nil {
			w.WriteHeader(404)
			return
		}
		if x.damaged {
			w.Write([]byte("damaged"))
			return
		}
		w.Write(pluginJarFor(x.name, x.version))
	default:
		w.WriteHeader(404)
	}
}

func installed(t *testing.T, s *Server) map[string]InstalledPlugin {
	t.Helper()
	v, err := s.Plugins()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]InstalledPlugin{}
	for _, p := range v.Plugins {
		out[p.Name] = p
	}
	return out
}

func TestJavaPlugins(t *testing.T) {
	f := newFakeJavaWorld(t)
	f.fakePlugins = []*fakePlugin{
		{source: SourceHangar, id: "LandClaim", title: "Land Claim", name: "LandClaim", version: "1.0", needs: []string{"7"}},
		{source: SourceHangar, id: "7", title: "Core Lib", name: "CoreLib", version: "3.2"},
		{source: SourceHangar, id: "OwnSite", title: "Own Site Land", name: "OwnSite", version: "1.0", external: true},
		{source: SourceModrinth, id: "abc123", title: "Homes", name: "Homes", version: "2.0", needs: []string{"vault99"}},
		{source: SourceModrinth, id: "vault99", title: "Vault", name: "Vault", version: "1.7"},
		{source: SourceModrinth, id: "extra1", title: "Extra Land", name: "Extra", version: "1.0"},
		{source: SourceModrinth, id: "broken", title: "Broken", name: "Broken", version: "1.0", damaged: true},
	}
	m := newJavaManager(t, f)
	st, err := m.CreateJava("Plugged", 0)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m.Get(st.ID)
	waitFor(t, s, "running", state(StateRunning))

	// Search: both sites, made for this version.
	res, notes := m.SearchPlugins(t.Context(), "land", "1.21.4")
	if len(notes) != 0 || len(res) != 3 || res[0].Source != SourceHangar || res[1].Source != SourceModrinth || res[0].Page != "https://hangar.papermc.io/someone/LandClaim" {
		t.Errorf("search: %+v %v", res, notes)
	}
	// Hangar's fake search doesn't filter versions, but its versions do:
	// what can't be installed on 1.20.1 isn't shown.
	if res, notes := m.SearchPlugins(t.Context(), "land", "1.20.1"); len(res) != 0 || len(notes) != 1 || !strings.Contains(notes[0], "2 more found") {
		t.Errorf("version filter: %+v %v", res, notes)
	}

	// Install: what it needs comes first.
	title, err := s.InstallPlugin(SourceHangar, "LandClaim")
	if err != nil || title != "Land Claim" {
		t.Fatalf("install: %q %v", title, err)
	}
	got := installed(t, s)
	if got["LandClaim"].Version != "1.0" || got["CoreLib"].For != "Land Claim" || !got["CoreLib"].Enabled || !got["Geyser-Spigot"].Console {
		t.Errorf("installed: %+v", got)
	}
	if v, _ := s.Plugins(); !v.RestartNeeded || !v.Running || v.Minecraft != "1.21.4" {
		t.Errorf("view: %+v", v)
	}
	if _, err := s.InstallPlugin(SourceHangar, "LandClaim"); err == nil || !strings.Contains(err.Error(), "already installed") {
		t.Errorf("twice: %v", err)
	}
	// Modrinth: SHA-512, the release rather than the newer beta, the jar
	// rather than the sources.
	if _, err := s.InstallPlugin(SourceModrinth, "abc123"); err != nil {
		t.Fatal(err)
	}
	if got := installed(t, s); got["Homes"].Version != "2.0" || got["Vault"].For != "Homes" || got["Homes"].Page != "https://modrinth.com/plugin/homes" {
		t.Errorf("modrinth: %+v", got)
	}
	// Refused: only on its own site, a damaged download, one already there
	// by hand, nonsense.
	if _, err := s.InstallPlugin(SourceHangar, "OwnSite"); err == nil || !strings.Contains(err.Error(), "own site") {
		t.Errorf("external: %v", err)
	}
	if _, err := s.InstallPlugin(SourceModrinth, "broken"); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Errorf("damaged: %v", err)
	}
	os.WriteFile(filepath.Join(s.pluginsDir(), "MyExtra.jar"), pluginJarFor("Extra", "0.9"), 0o644)
	var byHand errByHand
	if _, err := s.InstallPlugin(SourceModrinth, "extra1"); !errors.As(err, &byHand) {
		t.Errorf("by hand: %v", err)
	}
	if _, err := s.InstallPlugin(SourceModrinth, "../etc"); err == nil {
		t.Error("odd ID accepted")
	}
	if _, err := s.InstallPlugin("curseforge", "x"); err == nil {
		t.Error("odd source accepted")
	}
	if got := installed(t, s); got["Extra"].Source != "" || got["Extra"].File != "MyExtra.jar" || got["Broken"].File != "" {
		t.Errorf("after refusals: %+v", got)
	}

	// Turned off: Paper skips it; updating keeps it off.
	homes := installed(t, s)["Homes"].File
	if err := s.SetPluginEnabled(homes, false); err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(s.pluginsDir(), homes+disabledSuffix)) || installed(t, s)["Homes"].Enabled {
		t.Error("not turned off")
	}
	f.mu.Lock()
	f.plugin(SourceModrinth, "abc123").version = "2.1"
	f.plugin(SourceHangar, "LandClaim").version = "1.1"
	f.mu.Unlock()
	ups := s.PluginUpdates(t.Context())
	land := installed(t, s)["LandClaim"].File
	if ups[homes] != "2.1" || ups[land] != "1.1" || len(ups) != 2 {
		t.Errorf("updates: %v", ups)
	}
	if v, err := s.UpdatePlugin(homes); err != nil || v != "2.1" {
		t.Fatalf("update: %q %v", v, err)
	}
	if got := installed(t, s)["Homes"]; got.Version != "2.1" || got.Enabled {
		t.Errorf("updated: %+v", got)
	}
	if _, err := s.UpdatePlugin("MyExtra.jar"); err == nil {
		t.Error("a plugin added by hand updated")
	}

	// Removing: console support and odd names are refused.
	if err := s.RemovePlugin(land); err != nil {
		t.Fatal(err)
	}
	if _, ok := installed(t, s)["LandClaim"]; ok {
		t.Error("not removed")
	}
	for _, bad := range []string{"Geyser-Spigot.jar", "../paper.jar", ".hidden.jar", "nothere.jar", "paper.jar"} {
		if err := s.RemovePlugin(bad); err == nil {
			t.Errorf("removed %s", bad)
		}
		if err := s.SetPluginEnabled(bad, false); err == nil {
			t.Errorf("turned off %s", bad)
		}
	}
	// Added again after its file went (a restore): the old record is forgotten.
	if _, err := s.InstallPlugin(SourceHangar, "LandClaim"); err != nil {
		t.Fatal(err)
	}
	vault := installed(t, s)["Vault"].File
	os.Remove(filepath.Join(s.pluginsDir(), vault))
	if _, err := s.InstallPlugin(SourceModrinth, "vault99"); err != nil {
		t.Errorf("after a restore: %v", err)
	}
	// A restart picks it all up.
	if err := s.Restart(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, s, "running", state(StateRunning))
	if v, _ := s.Plugins(); v.RestartNeeded {
		t.Error("still needs a restart")
	}
}

func TestPluginFileNames(t *testing.T) {
	s := &Server{}
	files := map[string]bool{"Homes.jar": true, "Geyser-Spigot.jar": true}
	for in, want := range map[string]string{
		"Homes.jar":         "Homes-2.jar",
		"../../etc/passwd":  "passwd.jar",
		"My Plugin (1).jar": "My_Plugin_1_.jar",
		".hidden.jar":       "hidden.jar",
		"Geyser-Spigot.jar": "Geyser-Spigot-2.jar",
		"":                  "plugin.jar",
	} {
		if got := s.pluginFileName(in, files, ""); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	if got := s.pluginFileName("Homes.jar", files, "Homes.jar"); got != "Homes.jar" {
		t.Errorf("replacing: %q", got)
	}
}

// What plugins need: console support's own plugins, cycles, partial
// failures (nothing goes in), and an update that needs something new.
func TestPluginNeeds(t *testing.T) {
	f := newFakeJavaWorld(t)
	f.fakePlugins = []*fakePlugin{
		{source: SourceModrinth, id: "viaback", title: "ViaBackwards", name: "ViaBackwards", version: "5.0", needs: []string{"viaver"}},
		{source: SourceModrinth, id: "viaver", title: "ViaVersion", name: "ViaVersion", version: "5.5.1"},
		{source: SourceModrinth, id: "aaa", title: "Alpha", name: "Alpha", version: "1", needs: []string{"bbb"}},
		{source: SourceModrinth, id: "bbb", title: "Beta", name: "Beta", version: "1", needs: []string{"aaa"}},
		{source: SourceModrinth, id: "ccc", title: "Gamma", name: "Gamma", version: "1", needs: []string{"ddd", "broken"}},
		{source: SourceModrinth, id: "ddd", title: "Delta", name: "Delta", version: "1"},
		{source: SourceModrinth, id: "broken", title: "Broken", name: "Broken", version: "1", damaged: true},
		{source: SourceHangar, id: "Grow", title: "Grow", name: "Grow", version: "1.0"},
		{source: SourceHangar, id: "9", title: "Grow Lib", name: "GrowLib", version: "2.0"},
	}
	m := newJavaManager(t, f)
	st, err := m.CreateJava("Needs", 0)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m.Get(st.ID)
	waitFor(t, s, "running", state(StateRunning))
	// The fake console jars aren't real plugins: give ViaVersion a plugin.yml.
	os.WriteFile(filepath.Join(s.pluginsDir(), "ViaVersion.jar"), pluginJarFor("ViaVersion", "5.5.1"), 0o644)

	// Needs ViaVersion: console support has it.
	if _, err := s.InstallPlugin(SourceModrinth, "viaback"); err != nil {
		t.Fatal(err)
	}
	got := installed(t, s)
	if got["ViaBackwards"].Source != SourceModrinth || got["ViaVersion"].File != "ViaVersion.jar" || len(got) != 4 {
		t.Errorf("installed: %+v", got)
	}
	// ViaVersion itself is console support's.
	if _, err := s.InstallPlugin(SourceModrinth, "viaver"); err == nil || !strings.Contains(err.Error(), "console support") {
		t.Errorf("console plugin: %v", err)
	}
	// A needs B needs A: both go in once.
	if _, err := s.InstallPlugin(SourceModrinth, "aaa"); err != nil {
		t.Fatal(err)
	}
	if got := installed(t, s); got["Alpha"].File == "" || got["Beta"].For != "Alpha" {
		t.Errorf("cycle: %+v", got)
	}
	// One of its needs fails: nothing goes in.
	if _, err := s.InstallPlugin(SourceModrinth, "ccc"); err == nil {
		t.Error("installed with a broken need")
	}
	if got := installed(t, s); got["Delta"].File != "" || got["Gamma"].File != "" {
		t.Errorf("partly installed: %+v", got)
	}
	leftovers, _ := filepath.Glob(filepath.Join(m.serversDir(), ".download-*"))
	if len(leftovers) != 0 {
		t.Errorf("downloads left behind: %v", leftovers)
	}
	// An update (Hangar's file name changes with the version) that needs a
	// new plugin, while turned on.
	if _, err := s.InstallPlugin(SourceHangar, "Grow"); err != nil {
		t.Fatal(err)
	}
	old := installed(t, s)["Grow"].File
	f.mu.Lock()
	f.plugin(SourceHangar, "Grow").version, f.plugin(SourceHangar, "Grow").needs = "1.1", []string{"9"}
	f.mu.Unlock()
	if _, err := s.UpdatePlugin(old); err != nil {
		t.Fatal(err)
	}
	got = installed(t, s)
	if got["Grow"].Version != "1.1" || got["Grow"].File == old || !got["Grow"].Enabled || got["GrowLib"].For != "Grow" || fileExists(filepath.Join(s.pluginsDir(), old)) {
		t.Errorf("update: %+v", got)
	}

	// While a backup, restore or update runs, nothing changes.
	s.backupMu.Lock()
	if err := s.SetPluginEnabled(got["Grow"].File, false); !errors.Is(err, errPluginsBusy) {
		t.Errorf("toggle during a backup: %v", err)
	}
	if _, err := s.InstallPlugin(SourceModrinth, "ddd"); !errors.Is(err, errPluginsBusy) {
		t.Errorf("install during a backup: %v", err)
	}
	s.backupMu.Unlock()
	if got := installed(t, s); got["Delta"].File != "" {
		t.Error("installed during a backup")
	}

	// A jar changed by hand isn't the panel's any more.
	os.WriteFile(filepath.Join(s.pluginsDir(), got["GrowLib"].File), pluginJarFor("GrowLib", "hacked"), 0o644)
	if got := installed(t, s); got["GrowLib"].Source != "" {
		t.Errorf("changed jar still the panel's: %+v", got["GrowLib"])
	}
	// Removing what Grow was installed for leaves GrowLib, no longer "for" it.
	if err := s.RemovePlugin(got["Grow"].File); err != nil {
		t.Fatal(err)
	}
	for _, a := range s.pluginRecords() {
		if a.For == "Grow" {
			t.Errorf("still for Grow: %+v", a)
		}
	}
}

// The records travel with plugins/, so a restore brings back the versions
// that were installed then.
func TestPluginRestore(t *testing.T) {
	f := newFakeJavaWorld(t)
	f.fakePlugins = []*fakePlugin{{source: SourceModrinth, id: "homes", title: "Homes", name: "Homes", version: "1.0"}}
	m := newJavaManager(t, f)
	st, err := m.CreateJava("Rewind", 0)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m.Get(st.ID)
	waitFor(t, s, "running", state(StateRunning))
	if _, err := s.InstallPlugin(SourceModrinth, "homes"); err != nil {
		t.Fatal(err)
	}
	if err := s.StartBackup(); err != nil {
		t.Fatal(err)
	}
	waitConsole(t, s, "Backup saved")
	backup := s.Backups().Backups[0].Name
	f.mu.Lock()
	f.plugin(SourceModrinth, "homes").version = "2.0"
	f.mu.Unlock()
	file := installed(t, s)["Homes"].File
	if _, err := s.UpdatePlugin(file); err != nil {
		t.Fatal(err)
	}
	if err := s.Restore(backup); err != nil {
		t.Fatal(err)
	}
	waitConsole(t, s, "[Panel] Restored ")
	waitFor(t, s, "running", state(StateRunning))
	got := installed(t, s)["Homes"]
	if got.Version != "1.0" || got.Source != SourceModrinth {
		t.Errorf("after restore: %+v", got)
	}
	if ups := s.PluginUpdates(t.Context()); ups[got.File] != "2.0" {
		t.Errorf("update not offered after the restore: %v", ups)
	}
}

func TestPluginMeta(t *testing.T) {
	dir := t.TempDir()
	for i, yml := range []string{
		"name: Homes\nversion: 1.2\ndepend: [Vault, 'WorldEdit']\nsoftdepend: [Other]\n",
		"name: Homes\nversion: \"1.2\"\ndepend:\n  - Vault\n  - WorldEdit\ncommands:\n  home:\n    - x\n",
		"name: Homes\nversion: 1.2\ndepend:\n- Vault\n- WorldEdit\n",
	} {
		var buf bytes.Buffer
		z := zip.NewWriter(&buf)
		w, _ := z.Create("plugin.yml")
		w.Write([]byte(yml))
		z.Close()
		p := filepath.Join(dir, fmt.Sprint(i)+".jar")
		os.WriteFile(p, buf.Bytes(), 0o644)
		name, version, depends, err := pluginMeta(p)
		if err != nil || name != "Homes" || version != "1.2" || strings.Join(depends, ",") != "Vault,WorldEdit" {
			t.Errorf("%d: %q %q %v %v", i, name, version, depends, err)
		}
	}
}

// A copy of console support's ViaVersion put in by hand is turned off
// (two copies make Paper refuse one); plugins can still be turned off while
// the Minecraft version isn't known; a plan whose plugins went meanwhile
// isn't committed.
func TestPluginEdges(t *testing.T) {
	f := newFakeJavaWorld(t)
	m := newJavaManager(t, f)
	st, err := m.CreateJava("Edges", 0)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m.Get(st.ID)
	waitFor(t, s, "running", state(StateRunning))
	hand := filepath.Join(s.pluginsDir(), "ViaVersion-hand.jar")
	os.WriteFile(hand, pluginJarFor("ViaVersion", "5.0"), 0o644)
	if _, err := s.ensureConsolePlugins(t.Context()); err != nil {
		t.Fatal(err)
	}
	if fileExists(hand) || !fileExists(hand+disabledSuffix) {
		t.Error("the second copy is still on")
	}

	os.WriteFile(filepath.Join(s.pluginsDir(), "Mine.jar"), pluginJarFor("Mine", "1"), 0o644)
	s.mu.Lock()
	s.meta.Version = "" // after a full restore, until it starts
	s.mu.Unlock()
	if err := s.SetPluginEnabled("Mine.jar", false); err != nil {
		t.Errorf("turning off without a version: %v", err)
	}
	if _, err := s.InstallPlugin(SourceModrinth, "x"); err == nil {
		t.Error("installed without knowing the version")
	}
	s.mu.Lock()
	s.meta.Version = "1.21.4"
	s.mu.Unlock()

	unlock, err := s.lockPlugins("1.21.4")
	if err != nil {
		t.Fatal(err)
	}
	err = s.commitPlugins(&pluginPlan{relies: []string{"Mine.jar"}}, "")
	unlock()
	if err == nil || !strings.Contains(err.Error(), "meanwhile") {
		t.Errorf("relied on a plugin that's off: %v", err)
	}
	if _, err := s.lockPlugins("1.20.1"); err == nil {
		t.Error("committed after the version changed")
	}
}
