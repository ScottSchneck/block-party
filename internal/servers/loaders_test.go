package servers

import (
	"archive/zip"
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNeoForgeAndForgeVersions(t *testing.T) {
	for v, want := range map[string]string{"21.1.77": "1.21.1", "21.0.5-beta": "1.21", "20.4.237": "1.20.4", "20.2.3-beta": "1.20.2",
		"26.1.0.12": "26.1", "26.1.1.3-beta": "26.1.1", "26.1.0": "", "19.1.1": "", "junk": ""} {
		if got := neoForgeMC(v); got != want {
			t.Errorf("neoForgeMC(%q) = %q, want %q", v, got, want)
		}
	}
	vs := []string{"21.1.70", "21.1.77", "21.1.80-beta", "21.0.1-beta", "21.0.2-beta"}
	if got := neoForgeFor(vs, "1.21.1"); got != "21.1.77" {
		t.Errorf("a finished build over a beta: %s", got)
	}
	if got := neoForgeFor(vs, "1.21"); got != "21.0.2-beta" {
		t.Errorf("only betas: %s", got)
	}
	promos := map[string]string{"1.21.1-latest": "52.0.20", "1.21.1-recommended": "52.0.16", "1.20.1-latest": "47.3.0", "1.7.10-latest": "10.13.4", "latest-1.7.10": "x"}
	got := forgeMCVersions(promos)
	if len(got) != 2 || forgeFor(promos, "1.21.1") != "52.0.16" || forgeFor(promos, "1.20.1") != "47.3.0" {
		t.Errorf("forge: %v", got)
	}
	if guessJavaMajor("1.12.2") != 8 || guessJavaMajor("1.18.2") != 17 || guessJavaMajor("1.21.4") != 21 || guessJavaMajor("26.1") != 25 {
		t.Error("java guesses")
	}
}

// fakeLoaders serves Mojang's version list, Purpur, Fabric, Forge and
// NeoForge, and Geyser and Floodgate for Fabric.
type fakeLoaders struct {
	mu         sync.Mutex
	srv        *httptest.Server
	fabricLoad string
	purpurB    string
	geyserFor  []string // Minecraft versions Geyser for Fabric supports
	packs      map[string][]byte
	mods       map[string]*fakeMod // Modrinth mods with real jars, by project ID
}

// fakeMod is a mod on the fake Modrinth.
type fakeMod struct {
	title, modID, version string
	loader                string   // "fabric", "forge" or "neoforge"
	needs                 []string // project IDs
	needIDs               []string // mod IDs its own file names
}

func (m *fakeMod) jar() []byte {
	return modJar(m.loader, m.modID, m.title, m.version, m.needIDs...)
}

// modJar makes a mod's jar, with fabric.mod.json or mods.toml.
func modJar(loader, id, name, version string, needs ...string) []byte {
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	switch loader {
	case "fabric":
		deps := map[string]string{"minecraft": "~1.21.4", "fabricloader": ">=0.16"}
		for _, n := range needs {
			deps[n] = "*"
		}
		b, _ := json.Marshal(map[string]any{"schemaVersion": 1, "id": id, "name": name, "version": version, "depends": deps})
		w, _ := z.Create("fabric.mod.json")
		w.Write(b)
	default:
		file := "META-INF/mods.toml"
		if loader == "neoforge" {
			file = "META-INF/neoforge.mods.toml"
		}
		w, _ := z.Create(file)
		fmt.Fprintf(w, "modLoader=\"javafml\"\nloaderVersion=\"[1,)\"\n[[mods]]\nmodId=\"%s\"\nversion=\"${file.jarVersion}\"\ndisplayName=\"%s\"\ndescription='''\nA mod.\n[[not a section]]\n'''\n", id, name)
		fmt.Fprintf(w, "[[dependencies.%s]]\nmodId=\"minecraft\"\nmandatory=true\n", id)
		for _, n := range needs {
			fmt.Fprintf(w, "[[dependencies.%s]]\n  modId = \"%s\" # needed\n  type=\"required\"\n  side=\"BOTH\"\n", id, n)
		}
		fmt.Fprintf(w, "[[dependencies.%s]]\nmodId=\"jei\"\nmandatory=false\n", id)
		mf, _ := z.Create("META-INF/MANIFEST.MF")
		fmt.Fprintf(mf, "Manifest-Version: 1.0\nImplementation-Version: %s\n", version)
	}
	z.Close()
	return buf.Bytes()
}

func hexOf(sum []byte) string { return hex.EncodeToString(sum) }

func newFakeLoaders(t *testing.T) *fakeLoaders {
	f := &fakeLoaders{fabricLoad: "0.16.9", purpurB: "2400", geyserFor: []string{"1.21.4"}, packs: map[string][]byte{}}
	vanilla := func(v string) []byte { return []byte("vanilla server " + v) }
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := r.URL.Path
		enc := json.NewEncoder(w)
		switch {
		case p == "/manifest":
			var vs []map[string]string
			for _, v := range []string{"1.21.4", "1.21.1", "1.20.4", "1.12.2", "24w14a"} {
				typ := "release"
				if v == "24w14a" {
					typ = "snapshot"
				}
				vs = append(vs, map[string]string{"id": v, "type": typ, "url": f.srv.URL + "/v/" + v + ".json"})
			}
			enc.Encode(map[string]any{"latest": map[string]string{"release": "1.21.4"}, "versions": vs})
		case strings.HasPrefix(p, "/v/"):
			v := strings.TrimSuffix(strings.TrimPrefix(p, "/v/"), ".json")
			sum := sha1.Sum(vanilla(v))
			enc.Encode(map[string]any{"javaVersion": map[string]int{"majorVersion": 21},
				"downloads": map[string]any{"server": map[string]string{"url": f.srv.URL + "/vjar/" + v, "sha1": hexOf(sum[:])}}})
		case strings.HasPrefix(p, "/vjar/"):
			w.Write(vanilla(strings.TrimPrefix(p, "/vjar/")))
		case p == "/purpur":
			enc.Encode(map[string]any{"project": "purpur", "versions": []string{"1.20.4", "1.21.1", "1.21.4"}})
		case strings.HasPrefix(p, "/purpur/"):
			parts := strings.Split(strings.TrimPrefix(p, "/purpur/"), "/")
			jar := []byte("purpur " + parts[0] + " " + f.purpurB)
			switch len(parts) {
			case 1:
				enc.Encode(map[string]any{"builds": map[string]any{"latest": f.purpurB, "all": []string{"1", f.purpurB}}})
			case 2:
				sum := md5.Sum(jar)
				enc.Encode(map[string]any{"md5": hexOf(sum[:])})
			default:
				w.Write(jar)
			}
		case p == "/fabric/versions/game":
			enc.Encode([]map[string]any{{"version": "24w14a", "stable": false}, {"version": "1.21.4", "stable": true}, {"version": "1.21.1", "stable": true}})
		case p == "/fabric/versions/loader":
			enc.Encode([]map[string]any{{"version": "0.17.0-beta", "stable": false}, {"version": f.fabricLoad, "stable": true}})
		case p == "/fabric/versions/installer":
			enc.Encode([]map[string]any{{"version": "1.0.1", "stable": true}})
		case strings.HasPrefix(p, "/fabric/versions/loader/") && strings.HasSuffix(p, "/server/jar"):
			w.Write([]byte("fabric launcher " + p))
		case p == "/forge/promos.json":
			enc.Encode(map[string]any{"promos": map[string]string{"1.21.1-latest": "52.0.20", "1.21.1-recommended": "52.0.16", "1.20.1-latest": "47.3.0"}})
		case strings.HasPrefix(p, "/forgemaven/") && strings.HasSuffix(p, "-installer.jar"):
			full := strings.Split(strings.TrimPrefix(p, "/forgemaven/"), "/")[0]
			w.Write([]byte("libraries/net/minecraftforge/forge/" + full + "/unix_args.txt"))
		case strings.HasPrefix(p, "/forgemaven/") && strings.HasSuffix(p, "-installer.jar.sha1"):
			full := strings.Split(strings.TrimPrefix(p, "/forgemaven/"), "/")[0]
			sum := sha1.Sum([]byte("libraries/net/minecraftforge/forge/" + full + "/unix_args.txt"))
			fmt.Fprintf(w, "%s", hexOf(sum[:]))
		case p == "/neo/maven-metadata.xml":
			fmt.Fprint(w, `<metadata><versioning><release>21.1.77</release><versions><version>21.1.70</version><version>21.1.77</version><version>21.4.10-beta</version></versions></versioning></metadata>`)
		case strings.HasPrefix(p, "/neo/") && strings.HasSuffix(p, "-installer.jar"):
			v := strings.Split(strings.TrimPrefix(p, "/neo/"), "/")[0]
			w.Write([]byte("libraries/net/neoforged/neoforge/" + v + "/unix_args.txt"))
		case p == "/geyser/geyser/versions/latest/builds/latest":
			jar := []byte("geyser fabric")
			sum := sha256Of(jar)
			enc.Encode(map[string]any{"version": "2.11.3", "build": 50, "downloads": map[string]any{"fabric": map[string]string{"name": "Geyser-Fabric.jar", "sha256": sum}}})
		case p == "/geyser/geyser/versions/2.11.3/builds/50/downloads/fabric":
			w.Write([]byte("geyser fabric"))
		case strings.HasPrefix(p, "/modrinth/project/pack") && strings.HasSuffix(p, "/version"):
			id := strings.TrimSuffix(strings.TrimPrefix(p, "/modrinth/project/"), "/version")
			pack := f.packs[id]
			sum := sha512.Sum512(pack)
			enc.Encode([]map[string]any{{"name": "Pack 1.0", "version_number": "1.0", "version_type": "release", "loaders": []string{"fabric"},
				"files": []map[string]any{{"url": f.srv.URL + "/packfile/" + id, "filename": id + ".mrpack", "primary": true, "hashes": map[string]string{"sha512": hexOf(sum[:])}}}}})
		case strings.HasPrefix(p, "/packfile/"):
			w.Write(f.packs[strings.TrimPrefix(p, "/packfile/")])
		case strings.HasPrefix(p, "/packmod/"):
			w.Write([]byte("packmod " + strings.TrimPrefix(p, "/packmod/")))
		case p == "/modrinth/project/geyser/version":
			enc.Encode([]map[string]any{{"game_versions": f.geyserFor, "date_published": "2026-10-10T00:00:00Z"}})
		case p == "/modrinth/search":
			q := r.URL.Query()
			var out []map[string]any
			for id, x := range f.mods {
				if strings.Contains(q.Get("facets"), `"categories:`+x.loader+`"`) && strings.Contains(q.Get("facets"), "project_type:mod") &&
					strings.Contains(strings.ToLower(x.title), strings.ToLower(q.Get("query"))) {
					out = append(out, map[string]any{"project_id": id, "slug": id, "title": x.title, "author": "dev", "downloads": 10})
				}
			}
			enc.Encode(map[string]any{"hits": out})
		case strings.HasPrefix(p, "/modrinth/project/") && strings.HasSuffix(p, "/version") && f.mods[strings.TrimSuffix(strings.TrimPrefix(p, "/modrinth/project/"), "/version")] != nil:
			id := strings.TrimSuffix(strings.TrimPrefix(p, "/modrinth/project/"), "/version")
			x := f.mods[id]
			if r.URL.Query().Get("loaders") != `["`+x.loader+`"]` || r.URL.Query().Get("game_versions") != `["1.21.4"]` {
				enc.Encode([]any{})
				return
			}
			jar := x.jar()
			sum := sha512.Sum512(jar)
			var deps []map[string]string
			for _, n := range x.needs {
				deps = append(deps, map[string]string{"project_id": n, "dependency_type": "required"})
			}
			enc.Encode([]map[string]any{{"version_number": x.version, "version_type": "release", "dependencies": deps,
				"files": []map[string]any{{"url": f.srv.URL + "/realmod/" + id, "filename": id + "-" + x.version + ".jar", "primary": true, "hashes": map[string]string{"sha512": hexOf(sum[:])}}}}})
		case strings.HasPrefix(p, "/realmod/"):
			w.Write(f.mods[strings.TrimPrefix(p, "/realmod/")].jar())
		case strings.HasPrefix(p, "/modrinth/project/") && f.mods[strings.TrimPrefix(p, "/modrinth/project/")] != nil:
			id := strings.TrimPrefix(p, "/modrinth/project/")
			enc.Encode(map[string]string{"id": id, "slug": id, "title": f.mods[id].title})
		case strings.HasPrefix(p, "/modrinth/project/") && strings.HasSuffix(p, "/version"):
			id := strings.TrimSuffix(strings.TrimPrefix(p, "/modrinth/project/"), "/version")
			if r.URL.Query().Get("loaders") != `["fabric"]` {
				enc.Encode([]any{})
				return
			}
			jar := []byte("mod " + id)
			sum := sha512.Sum512(jar)
			enc.Encode([]map[string]any{{"version_number": "1.0", "version_type": "release",
				"files": []map[string]any{{"url": f.srv.URL + "/modjar/" + id, "filename": id + ".jar", "primary": true, "hashes": map[string]string{"sha512": hexOf(sum[:])}}}}})
		case strings.HasPrefix(p, "/modrinth/project/"):
			id := strings.TrimPrefix(p, "/modrinth/project/")
			enc.Encode(map[string]string{"id": id, "slug": id, "title": id})
		case strings.HasPrefix(p, "/modjar/"):
			w.Write([]byte("mod " + strings.TrimPrefix(p, "/modjar/")))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func sha256Of(b []byte) string { return sha(b) }

func newLoaderManager(t *testing.T, java *fakeJavaWorld, f *fakeLoaders) *Manager {
	t.Helper()
	u := f.srv.URL
	m := New(Options{DataDir: t.TempDir(), StopTimeout: time.Second,
		PaperAPI: java.srv.URL + "/paper", AdoptiumAPI: java.srv.URL + "/adoptium", MojangVersions: u + "/manifest", MojangProfiles: java.srv.URL + "/profiles/",
		GeyserAPI: u + "/geyser", HangarAPI: java.srv.URL + "/hangar", XboxLookup: java.srv.URL + "/xbox/", ModrinthAPI: u + "/modrinth",
		PurpurAPI: u + "/purpur", FabricMeta: u + "/fabric", ForgePromos: u + "/forge/promos.json", ForgeMaven: u + "/forgemaven", NeoForgeMaven: u + "/neo"})
	if err := m.Load(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.StopAll)
	return m
}

func lastArgs(t *testing.T, s *Server) string {
	t.Helper()
	backlog, _, stop := s.Subscribe()
	stop()
	for i := len(backlog) - 1; i >= 0; i-- {
		if _, a, ok := strings.Cut(backlog[i], "args: "); ok {
			return a
		}
	}
	t.Fatalf("no start in the console:\n%s", strings.Join(backlog, "\n"))
	return ""
}

func TestJavaServerTypes(t *testing.T) {
	java := newFakeJavaWorld(t)
	f := newFakeLoaders(t)
	m := newLoaderManager(t, java, f)

	versions, err := m.JavaVersions(t.Context(), LoaderVanilla)
	if err != nil || strings.Join(versions, ",") != "1.21.4,1.21.1,1.20.4,1.12.2" {
		t.Fatalf("vanilla versions: %v %v", versions, err)
	}
	if v, _ := m.JavaVersions(t.Context(), LoaderNeoForge); strings.Join(v, ",") != "1.21.4,1.21.1" {
		t.Errorf("neoforge versions: %v", v)
	}

	cases := []struct {
		choice             JavaChoice
		version, launch    string
		args               string
		consoles, keep     bool
		loaderVersionCheck string
	}{
		{JavaChoice{LoaderVanilla, "1.20.4"}, "1.20.4", "server.jar", "-jar server.jar --nogui --universe worlds", false, true, ""},
		{JavaChoice{LoaderPurpur, ""}, "1.21.4", "paper.jar", "-jar paper.jar --nogui --world-container worlds", true, false, "2400"},
		{JavaChoice{LoaderFabric, ""}, "1.21.4", "fabric-server-launch.jar", "-jar fabric-server-launch.jar --nogui --universe worlds", true, true, "0.16.9"},
		{JavaChoice{LoaderForge, "1.21.1"}, "1.21.1", "@libraries/net/minecraftforge/forge/1.21.1-52.0.16/unix_args.txt", "@libraries/net/minecraftforge/forge/1.21.1-52.0.16/unix_args.txt --nogui --universe worlds", false, true, "52.0.16"},
		{JavaChoice{LoaderNeoForge, ""}, "1.21.1", "@libraries/net/neoforged/neoforge/21.1.77/unix_args.txt", "@libraries/net/neoforged/neoforge/21.1.77/unix_args.txt --nogui --universe worlds", false, true, "21.1.77"},
	}
	for _, c := range cases {
		st, err := m.CreateJavaOf(c.choice.Loader+" world", 1024, c.choice)
		if err != nil {
			t.Fatal(err)
		}
		s, _ := m.Get(st.ID)
		waitFor(t, s, c.choice.Loader+" running", state(StateRunning))
		st = s.Status()
		if st.Loader != c.choice.Loader || st.Version != c.version || st.KeepVersion != c.keep || st.Consoles != c.consoles {
			t.Errorf("%s: status %+v", c.choice.Loader, st)
		}
		if c.loaderVersionCheck != "" && st.LoaderVersion != c.loaderVersionCheck {
			t.Errorf("%s: loader version %q", c.choice.Loader, st.LoaderVersion)
		}
		s.mu.Lock()
		launch := s.meta.Launch
		s.mu.Unlock()
		if launch != c.launch {
			t.Errorf("%s: launch %q", c.choice.Loader, launch)
		}
		if got := lastArgs(t, s); !strings.HasSuffix(got, c.args) || !strings.Contains(got, "-Xmx1024M") {
			t.Errorf("%s: started with %q", c.choice.Loader, got)
		}
		if !strings.Contains(readFile(t, filepath.Join(s.dir(), "eula.txt")), "eula=true") {
			t.Errorf("%s: eula", c.choice.Loader)
		}
		if c.choice.Loader == LoaderFabric {
			for _, jar := range []string{"Geyser-Fabric.jar", "floodgate-fabric.jar", "fabric-api.jar"} {
				if !fileExists(filepath.Join(s.dir(), "mods", jar)) {
					t.Errorf("fabric console support: %s missing", jar)
				}
			}
			if !strings.Contains(lastArgs(t, s), "-DgeyserUdpPort=") {
				t.Error("fabric: Geyser's ports weren't given")
			}
			if readFile(t, filepath.Join(s.dir(), "server.jar")) != "vanilla server 1.21.4" {
				t.Error("fabric: the Minecraft server underneath")
			}
		}
		_ = s.Stop()
		waitFor(t, s, "stopped", state(StateStopped))
	}

	// A modded server keeps its Minecraft version on Update, taking a newer
	// loader.
	f.mu.Lock()
	f.fabricLoad = "0.16.10"
	f.mu.Unlock()
	var fab *Server
	for _, st := range m.List() {
		if st.Loader == LoaderFabric {
			fab, _ = m.Get(st.ID)
		}
	}
	if err := fab.Update(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, fab, "updated", func(st Status) bool { return st.LoaderVersion == "0.16.10" && st.State == StateStopped })
	if st := fab.Status(); st.Version != "1.21.4" {
		t.Errorf("the update moved Minecraft: %+v", st)
	}

	// An old version Geyser doesn't support: consoles are turned off.
	st, err := m.CreateJavaOf("old purpur", 1024, JavaChoice{LoaderPurpur, "1.20.4"})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m.Get(st.ID)
	waitFor(t, s, "running", state(StateRunning))
	if st := s.Status(); st.Consoles || st.Version != "1.20.4" {
		t.Errorf("old purpur: %+v", st)
	}
	waitConsole(t, s, "Consoles can't join Minecraft 1.20.4")

	if _, err := m.CreateJavaOf("bad", 1024, JavaChoice{"bukkit", ""}); err == nil {
		t.Error("an unknown type should be refused")
	}
	if _, err := m.CreateJavaOf("bad", 1024, JavaChoice{LoaderVanilla, "1.21; rm -rf"}); err == nil {
		t.Error("a bad version should be refused")
	}
}

// mrpack makes a modpack file.
func mrpack(t *testing.T, base string, files []map[string]any, extra map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	idx, _ := json.Marshal(map[string]any{"formatVersion": 1, "game": "minecraft", "versionId": "1.0", "name": "Test Pack",
		"files": files, "dependencies": map[string]string{"minecraft": "1.21.1", "fabric-loader": "0.16.5"}})
	w, _ := zw.Create("modrinth.index.json")
	w.Write(idx)
	for name, body := range extra {
		w, _ := zw.Create(name)
		w.Write([]byte(body))
	}
	zw.Close()
	return buf.Bytes()
}

func packEntry(base, rel, name, server string) map[string]any {
	sum := sha512.Sum512([]byte("packmod " + name))
	return map[string]any{"path": rel, "hashes": map[string]string{"sha512": hexOf(sum[:])}, "env": map[string]string{"client": "required", "server": server},
		"downloads": []string{base + "/packmod/" + name}}
}

func TestModpackServer(t *testing.T) {
	mrpackSchemes["http"] = true
	defer delete(mrpackSchemes, "http")
	java := newFakeJavaWorld(t)
	f := newFakeLoaders(t)
	host := strings.TrimPrefix(f.srv.URL, "http://")
	host, _, _ = strings.Cut(host, ":")
	mrpackHosts[host] = true
	defer delete(mrpackHosts, host)
	m := newLoaderManager(t, java, f)
	f.mu.Lock()
	f.packs["pack1"] = mrpack(t, f.srv.URL, []map[string]any{
		packEntry(f.srv.URL, "mods/lithium.jar", "lithium", "required"),
		packEntry(f.srv.URL, "mods/sodium.jar", "sodium", "unsupported"),
	}, map[string]string{"overrides/config/pack.toml": "a = 1", "server-overrides/config/pack.toml": "a = 2", "overrides/mods/extra.jar": "extra"})
	f.packs["pack2"] = mrpack(t, f.srv.URL, []map[string]any{packEntry(f.srv.URL, "../escape.jar", "evil", "required")}, nil)
	f.mu.Unlock()

	st, err := m.CreateModpack("Packed", 4096, ModpackSource{Project: "pack1"})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m.Get(st.ID)
	waitFor(t, s, "running", state(StateRunning))
	st = s.Status()
	if st.Loader != LoaderFabric || st.Version != "1.21.1" || st.LoaderVersion != "0.16.5" || st.Modpack != "Test Pack 1.0" || !st.KeepVersion || st.Consoles {
		t.Errorf("status: %+v", st)
	}
	if got := readFile(t, filepath.Join(s.dir(), "mods", "lithium.jar")); got != "packmod lithium" {
		t.Errorf("the pack's mod: %q", got)
	}
	if fileExists(filepath.Join(s.dir(), "mods", "sodium.jar")) {
		t.Error("a client-only mod was put on the server")
	}
	if got := readFile(t, filepath.Join(s.dir(), "config", "pack.toml")); got != "a = 2" {
		t.Errorf("server overrides come last: %q", got)
	}
	if !fileExists(filepath.Join(s.dir(), "mods", "extra.jar")) {
		t.Error("the pack's own files")
	}
	s.mu.Lock()
	left := s.meta.PackSource
	s.mu.Unlock()
	if left != "" {
		t.Error("the pack should be marked installed")
	}

	st, err = m.CreateModpack("Evil", 2048, ModpackSource{Project: "pack2"})
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := m.Get(st.ID)
	waitFor(t, s2, "refused", state(StateError))
	if !strings.Contains(s2.Status().Message, "unsafe") {
		t.Errorf("message: %s", s2.Status().Message)
	}
	if fileExists(filepath.Join(m.serversDir(), "escape.jar")) {
		t.Fatal("a pack file escaped the server's folder")
	}
	if _, err := m.CreateModpack("x", 2048, ModpackSource{File: "/etc/passwd"}); err == nil {
		t.Error("only uploads in the servers folder")
	}
}

// A queued allowlist change survives Mojang being slow for the server: it
// stays queued and is sent again.
func TestQueuedAllowlistRetries(t *testing.T) {
	javaRetryEvery = 100 * time.Millisecond
	defer func() { javaRetryEvery = 2 * time.Minute }()
	java := newFakeJavaWorld(t)
	java.profiles = false // the panel can't look anyone up: changes are queued
	m := newJavaManager(t, java)
	st, err := m.CreateJava("Retry", 1024, "Slow_Mojang")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m.Get(st.ID)
	waitFor(t, s, "running", state(StateRunning))
	waitConsole(t, s, "trying again")
	waitConsole(t, s, "Added Slow_Mojang to the whitelist")
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		left := len(s.meta.JavaPending)
		s.mu.Unlock()
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still queued: %+v", s.meta.JavaPending)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
