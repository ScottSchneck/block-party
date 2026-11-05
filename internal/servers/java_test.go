package servers

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeJava stands in for "java -jar paper.jar": it prints what Paper prints
// and answers the commands the panel sends.
const fakeJava = `#!/bin/sh
case "$*" in
  *--installServer*) spec=$(cat "$2"); mkdir -p "$(dirname "$spec")"; echo "-cp libraries/x.jar net.example.Main" > "$spec"; echo "The server installed successfully"; exit 0;;
esac
echo "[12:00:00 INFO]: args: $*"
case "$*" in
  *velocity.jar*) echo "[12:00:00 INFO]: Booting up Velocity 3.4.0-SNAPSHOT"; echo "[12:00:01 INFO]: Done (0.812s)!"
    while read line; do case "$line" in end) echo "[12:00:09 INFO]: Shutting down the proxy..."; exit 0;; *) echo "[12:00:08 INFO]: got: $line";; esac; done; exit 0;;
esac
if [ -f paper.jar ] || { [ ! -f server.jar ] && [ ! -d libraries ]; }; then
  v=$(cut -d' ' -f2 paper.jar 2>/dev/null)
  echo "[12:00:00 INFO]: Starting minecraft server version ${v:-VERSION}"
fi
echo "[12:00:01 INFO]: Done (1.234s)! For help, type \"help\""
while read line; do
  case "$line" in
    stop) echo "[12:00:09 INFO]: Stopping server"; exit 0;;
    crash) exit 3;;
    save-off) echo "[12:00:02 INFO]: Automatic saving is now disabled";;
    "save-all flush") echo "[12:00:02 INFO]: Saving the game (this may take a moment!)"; echo "[12:00:03 INFO]: Saved the game";;
    save-on) echo "[12:00:04 INFO]: Automatic saving is now enabled";;
    join\ *) echo "[12:00:05 INFO]: ${line#join } joined the game";;
    leave\ *) echo "[12:00:06 INFO]: ${line#leave } left the game";;
    reject\ *) echo "[12:00:07 INFO]: Disconnecting ${line#reject } (/192.168.1.20:50000): You are not white-listed on this server!";;
    "whitelist add Slow_Mojang"|"op Slow_Mojang") echo "[12:00:08 INFO]: got: $line"
      if [ ! -f .slow ]; then touch .slow; echo "[12:00:08 INFO]: That player does not exist"
      elif [ "$line" = "op Slow_Mojang" ]; then echo "[12:00:08 INFO]: Made Slow_Mojang a server operator"
      else echo "[12:00:08 INFO]: Added Slow_Mojang to the whitelist"; fi;;
    whitelist\ add\ *) echo "[12:00:08 INFO]: got: $line"; echo "[12:00:08 INFO]: Added ${line#whitelist add } to the whitelist";;
    op\ *) echo "[12:00:08 INFO]: got: $line"; echo "[12:00:08 INFO]: Made ${line#op } a server operator";;
    *) echo "[12:00:08 INFO]: got: $line";;
  esac
done
`

type fakeJavaWorld struct {
	mu       sync.Mutex
	version  string
	build    int
	jars     map[string][]byte
	jre      []byte
	jreSum   string
	profiles bool // Mojang's profile service is reachable
	srv      *httptest.Server
	jreFetch int
	// Console support: Geyser and Floodgate's version, ViaVersion's, and
	// gamertags the Xbox lookup knows (gamertag -> Xbox ID).
	geyser    string
	geyserB   int
	via       string
	viaDown   bool
	xboxDown  bool
	xbox      map[string]string
	pluginGet int
	badJars   bool // plugin downloads come back damaged
	// Java plugins on the fake Hangar and Modrinth.
	fakePlugins []*fakePlugin
	// newer is a Minecraft version newer than version that Paper also has,
	// and geyserMC the versions Modrinth says Geyser supports (nil: no answer).
	newer    string
	geyserMC []string
}

// pluginJar is the fake jar of a console plugin build.
func pluginJar(name, version string, build int) []byte {
	return []byte(fmt.Sprintf("%s %s build %d", name, version, build))
}

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func newFakeJavaWorld(t *testing.T) *fakeJavaWorld {
	f := &fakeJavaWorld{version: "1.21.4", build: 5, jars: map[string][]byte{}, profiles: true,
		geyser: "2.11.3", geyserB: 100, via: "5.5.1", xbox: map[string]string{"Dad Gamer": "2535400000000001", "Kid One": "2535400000000002", "Stranger": "2535400000000009"}}
	// A JRE whose bin/java is the fake server.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "jdk-21-jre/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "jdk-21-jre/bin/", Typeflag: tar.TypeDir, Mode: 0o755})
	body := []byte(strings.ReplaceAll(fakeJava, "VERSION", "1.21.4"))
	tw.WriteHeader(&tar.Header{Name: "jdk-21-jre/bin/java", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
	tw.Write(body)
	tw.Close()
	gz.Close()
	f.jre, f.jreSum = buf.Bytes(), sha(buf.Bytes())
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := r.URL.Path
		switch {
		case p == "/paper":
			vs := []string{f.version, "1.21.9-rc1", "1.20.6"}
			if f.newer != "" {
				vs = append(vs, f.newer)
			}
			json.NewEncoder(w).Encode(map[string]any{"versions": map[string][]string{"1.21": vs}})
		case strings.HasPrefix(p, "/paper/versions/") && strings.HasSuffix(p, "/builds"):
			v := strings.TrimSuffix(strings.TrimPrefix(p, "/paper/versions/"), "/builds")
			jar := []byte("paper " + v + " build " + fmt.Sprint(f.build))
			name := fmt.Sprintf("/jar/paper-%s-%d.jar", v, f.build)
			f.jars[name] = jar
			json.NewEncoder(w).Encode([]map[string]any{
				{"id": f.build - 1, "channel": "STABLE", "downloads": map[string]any{"server:default": map[string]any{"url": f.srv.URL + "/jar/old.jar"}}},
				{"id": f.build, "channel": "STABLE", "downloads": map[string]any{"server:default": map[string]any{"name": "paper.jar", "url": f.srv.URL + name, "checksums": map[string]string{"sha256": sha(jar)}}}},
				{"id": f.build + 1, "channel": "ALPHA", "downloads": map[string]any{}},
			})
		case strings.HasPrefix(p, "/jar/"):
			w.Write(f.jars[p])
		case p == "/manifest":
			json.NewEncoder(w).Encode(map[string]any{"versions": []map[string]string{{"id": f.version, "url": f.srv.URL + "/v.json"}}})
		case p == "/v.json":
			w.Write([]byte(`{"javaVersion":{"majorVersion":21}}`))
		case strings.HasPrefix(p, "/adoptium/assets/latest/21/hotspot"):
			json.NewEncoder(w).Encode([]map[string]any{{"release_name": "jdk-21-test", "binary": map[string]any{"package": map[string]any{"link": f.srv.URL + "/jre.tar.gz", "checksum": f.jreSum}}}})
		case p == "/jre.tar.gz":
			f.jreFetch++
			w.Write(f.jre)
		case strings.HasPrefix(p, "/profiles/"):
			name := strings.TrimPrefix(p, "/profiles/")
			if !f.profiles {
				w.WriteHeader(503)
				return
			}
			if name == "Nobody" {
				w.WriteHeader(204)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"id": "0123456789abcdef0123456789abcdef", "name": name})
		case p == "/velocity":
			json.NewEncoder(w).Encode(map[string]any{"versions": map[string][]string{"3.0.0": {"3.3.0-SNAPSHOT", "3.4.0-SNAPSHOT"}}})
		case p == "/velocity/versions/3.4.0-SNAPSHOT/builds":
			jar := []byte("velocity proxy")
			json.NewEncoder(w).Encode([]map[string]any{
				{"id": 510, "channel": "STABLE", "downloads": map[string]any{"server:default": map[string]any{"name": "velocity.jar", "url": f.srv.URL + "/velocityjar", "checksums": map[string]string{"sha256": sha(jar)}}}},
				{"id": 511, "channel": "BETA", "downloads": map[string]any{"server:default": map[string]any{"name": "velocity.jar", "url": f.srv.URL + "/velocityjar", "checksums": map[string]string{"sha256": sha(jar)}}}},
			})
		case p == "/velocityjar":
			w.Write([]byte("velocity proxy"))
		case strings.HasPrefix(p, "/geyser/"):
			// /geyser/<project>/versions/latest/builds/latest, and
			// /geyser/<project>/versions/<v>/builds/<b>/downloads/spigot
			parts := strings.Split(strings.TrimPrefix(p, "/geyser/"), "/")
			project := parts[0]
			file := map[string]string{"geyser": "Geyser-Spigot.jar", "floodgate": "floodgate-spigot.jar"}[project]
			if file == "" {
				w.WriteHeader(404)
				return
			}
			if strings.HasSuffix(p, "/versions/latest/builds/latest") {
				d := map[string]string{"name": file, "sha256": sha(pluginJar(project, f.geyser, f.geyserB))}
				json.NewEncoder(w).Encode(map[string]any{"version": f.geyser, "build": f.geyserB,
					"downloads": map[string]any{"spigot": d, "velocity": d}})
				return
			}
			if len(parts) == 7 && (parts[6] == "spigot" || parts[6] == "velocity") {
				b := 0
				fmt.Sscan(parts[4], &b)
				f.pluginGet++
				if f.badJars {
					w.Write([]byte("damaged"))
					return
				}
				w.Write(pluginJar(project, parts[2], b))
				return
			}
			w.WriteHeader(404)
		case p == "/hangar/ViaVersion/latestrelease":
			if f.viaDown {
				w.WriteHeader(503)
				return
			}
			w.Write([]byte(f.via))
		case strings.HasPrefix(p, "/hangar/ViaVersion/versions/"):
			v := strings.TrimPrefix(p, "/hangar/ViaVersion/versions/")
			json.NewEncoder(w).Encode(map[string]any{"downloads": map[string]any{"PAPER": map[string]any{
				"fileInfo":    map[string]any{"name": "ViaVersion-" + v + ".jar", "sha256Hash": sha(pluginJar("viaversion", v, 0))},
				"downloadUrl": f.srv.URL + "/viajar/" + v}}})
		case strings.HasPrefix(p, "/viajar/"):
			f.pluginGet++
			w.Write(pluginJar("viaversion", strings.TrimPrefix(p, "/viajar/"), 0))
		case strings.HasPrefix(p, "/xbox/"):
			if f.xboxDown {
				w.WriteHeader(503)
				return
			}
			x, ok := f.xbox[strings.TrimPrefix(p, "/xbox/")]
			if !ok {
				w.Write([]byte(`{}`)) // what GeyserMC answers for an unknown gamertag
				return
			}
			fmt.Fprintf(w, `{"xuid":%s}`, x)
		case p == "/modrinth/project/geyser/version":
			if f.geyserMC == nil || r.URL.Query().Get("loaders") != `["paper"]` {
				w.WriteHeader(404)
				return
			}
			json.NewEncoder(w).Encode([]map[string]any{
				{"game_versions": []string{"1.20.6"}, "date_published": "2026-01-01T00:00:00Z"},
				{"game_versions": f.geyserMC, "date_published": "2026-10-10T02:02:25Z"},
			})
		case p == "/hangar" || strings.HasPrefix(p, "/hangar/") || strings.HasPrefix(p, "/modrinth/") || strings.HasPrefix(p, "/pluginjar/"):
			f.servePlugins(w, r)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newJavaManager(t *testing.T, f *fakeJavaWorld) *Manager {
	t.Helper()
	m := New(Options{DataDir: t.TempDir(), StopTimeout: time.Second,
		PaperAPI: f.srv.URL + "/paper", AdoptiumAPI: f.srv.URL + "/adoptium", MojangVersions: f.srv.URL + "/manifest", MojangProfiles: f.srv.URL + "/profiles/",
		GeyserAPI: f.srv.URL + "/geyser", HangarAPI: f.srv.URL + "/hangar", XboxLookup: f.srv.URL + "/xbox/", ModrinthAPI: f.srv.URL + "/modrinth"})
	if err := m.Load(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.StopAll)
	return m
}

func TestJavaServer(t *testing.T) {
	f := newFakeJavaWorld(t)
	m := newJavaManager(t, f)
	st, err := m.CreateJava("Java Fun", 1024, "Steve_1")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m.Get(st.ID)
	waitFor(t, s, "running", state(StateRunning))
	st = s.Status()
	if st.Type != TypeJava || st.Version != "1.21.4" || st.Build != 5 || st.MemoryMB != 1024 {
		t.Errorf("status: %+v", st)
	}
	props := readFile(t, s.propertiesPath())
	for _, want := range []string{"server-port=" + fmt.Sprint(st.Port), "white-list=true", "online-mode=true", "motd=Java Fun", "level-name=world"} {
		if !strings.Contains(props, want) {
			t.Errorf("server.properties lacks %q:\n%s", want, props)
		}
	}
	if !strings.Contains(readFile(t, filepath.Join(s.dir(), "eula.txt")), "eula=true") {
		t.Error("eula")
	}
	// The adder was looked up and put on the lists before the first start.
	if wl := readFile(t, s.whitelistPath()); !strings.Contains(wl, `"name": "Steve_1"`) || !strings.Contains(wl, "01234567-89ab-cdef-0123-456789abcdef") {
		t.Errorf("whitelist: %s", wl)
	}
	if !strings.Contains(readFile(t, s.opsPath()), "Steve_1") {
		t.Error("not an operator")
	}

	// Running: changes go by command.
	if err := s.AllowlistAdd("Alex", ""); err != nil {
		t.Fatal(err)
	}
	waitConsole(t, s, "got: whitelist add Alex")
	if err := s.Kick("Alex", "bye now"); err != nil {
		t.Fatal(err)
	}
	waitConsole(t, s, "got: kick Alex bye now")
	if err := s.AllowlistAdd("bad name!", ""); err == nil {
		t.Error("bad name accepted")
	}
	// Joins, leaves and turned-away players.
	s.Command("join Steve_1")
	waitConsole(t, s, "Steve_1 joined the game")
	if p := s.Status().Players; len(p) != 1 || p[0].Name != "Steve_1" {
		t.Errorf("players: %+v", p)
	}
	s.Command("reject Griefer")
	waitConsole(t, s, "not white-listed")
	time.Sleep(200 * time.Millisecond)
	if v, _ := s.Players(); len(v.Attempts) != 1 || v.Attempts[0].Name != "Griefer" {
		t.Errorf("attempts: %+v", v.Attempts)
	}
	s.Command("leave Steve_1")
	waitConsole(t, s, "left the game")

	// A backup while it runs: saving is paused, everything copied, resumed.
	os.MkdirAll(filepath.Join(s.dir(), "worlds", "world", "region"), 0o755)
	os.WriteFile(filepath.Join(s.dir(), "worlds", "world", "level.dat"), []byte("java world"), 0o644)
	os.MkdirAll(filepath.Join(s.dir(), "plugins"), 0o755)
	os.WriteFile(filepath.Join(s.dir(), "plugins", "x.yml"), []byte("x"), 0o644)
	if err := s.StartBackup(); err != nil {
		t.Fatal(err)
	}
	waitConsole(t, s, "Backup saved")
	if consoleHas(s, "Saved the game") {
		t.Error("save chatter in the console")
	}
	b := s.Backups().Backups[0]
	c := tarContents(t, filepath.Join(m.backupsDir(s.ID()), b.Name))
	if c["worlds/world/level.dat"] != "java world" || c["plugins/x.yml"] != "x" || c["ops.json"] == "" {
		t.Errorf("backup has %v", keys(c))
	}

	// Stopped, with Mojang unreachable: the change waits for the next start.
	s.Stop()
	waitFor(t, s, "stopped", state(StateStopped))
	if err := s.AllowlistAdd("Nobody", ""); err == nil {
		t.Error("an account that doesn't exist was added")
	}
	f.mu.Lock()
	f.profiles = false
	f.mu.Unlock()
	if _, err := s.OpAdd("Later"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Players(); len(v.Operators) != 2 || !hasPending(v.Operators, "Later") {
		t.Errorf("ops: %+v", v.Operators)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	waitConsole(t, s, "got: op Later")

	// An update to a newer build: backed up first, Java isn't fetched again.
	f.mu.Lock()
	f.build = 7
	f.mu.Unlock()
	if err := s.Update(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, s, "updated", func(st Status) bool { return st.Build == 7 && st.State == StateRunning })
	if f.jreFetch != 1 {
		t.Errorf("Java downloaded %d times", f.jreFetch)
	}
	if !strings.Contains(readFile(t, s.binary()), "build 7") {
		t.Error("jar not replaced")
	}
	// Java servers have their own settings, and no Bedrock add-ons.
	v, err := s.Settings()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, g := range v.Groups {
		for _, fl := range g.Fields {
			found = found || fl.Key == "hardcore"
		}
	}
	if !found {
		t.Error("no Java settings")
	}
	if _, err := s.SaveSettings(map[string]string{"allow-cheats": "true"}); err == nil {
		t.Error("a Bedrock setting saved on a Java server")
	}
	if _, err := s.Packs(); err == nil {
		t.Error("add-ons on a Java server")
	}
	// Console support is on by default, so it's in the console list.
	found = false
	for _, j := range m.Joinable() {
		found = found || (j.Name == "Java Fun" && j.Running)
	}
	if !found {
		t.Error("Java server with console support isn't in the console list")
	}
}

func hasPending(l []ListedName, name string) bool {
	for _, x := range l {
		if x.Name == name && x.Pending {
			return true
		}
	}
	return false
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestLatestPaperSkipsPreReleases(t *testing.T) {
	f := newFakeJavaWorld(t)
	m := newJavaManager(t, f)
	r, err := m.latestPaper(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != "1.21.4" || r.Build != 5 || r.SHA256 == "" {
		t.Errorf("release %+v", r)
	}
	if !javaNewer(PaperRelease{Version: "1.21.4", Build: 6}, "1.21.4", 5) || javaNewer(PaperRelease{Version: "1.21.4", Build: 5}, "1.21.4", 5) ||
		!javaNewer(PaperRelease{Version: "26.1", Build: 1}, "1.21.4", 99) {
		t.Error("javaNewer")
	}
	if m.javaMajorFor(t.Context(), "1.21.4") != 21 {
		t.Error("java version from Mojang")
	}
	f.srv.Close() // unreachable: guess from the version
	if m.javaMajorFor(t.Context(), "26.1.2") != 25 || m.javaMajorFor(t.Context(), "1.20.4") != 17 {
		t.Error("java version guess")
	}
}

func TestJavaLinesCantBeFakedInChat(t *testing.T) {
	for line, want := range map[string]string{
		"[12:00:05 INFO]: Steve joined the game":                                                          "join Steve",
		"[12:00:05] [Server thread/INFO]: Steve joined the game":                                          "join Steve",
		"[12:00:05 INFO]: Steve (formerly known as Bob) joined the game":                                  "join Steve",
		"[12:00:05 INFO]: <Bob> ]: Alice left the game":                                                   "",
		"[12:00:05 INFO]: <Bob> Alice joined the game":                                                    "",
		"[12:00:05 INFO]: Disconnecting Griefer (/1.2.3.4:5): You are not white-listed on this server!":   "reject Griefer",
		"[12:00:05 INFO]: Griefer (/1.2.3.4:5) lost connection: You are not white-listed on this server!": "reject Griefer",
		"[12:00:05 INFO]: <Bob> Griefer (/x) You are not white-listed":                                    "",
		"[12:00:05 INFO]: .Kid_One joined the game":                                                       "join .Kid_One",
		"[12:00:05 INFO]: .Jürgen#12 left the game":                                                       "leave .Jürgen#12",
		"[12:00:05 INFO]: Disconnecting .Stranger (/1.2.3.4:5): You are not white-listed on this server!": "reject .Stranger",
		// The server's own lines, which plugins can't change.
		"[12:00:05 INFO]: Steve[/192.168.1.20:50000] logged in with entity id 214 at ([world]0.5, 64.0, 0.5)": "join Steve",
		"[12:00:05 INFO]: .Kid_One[/127.0.0.1:41234] logged in with entity id 9 at ([world]1.0, 70.0, 2.0)":   "join .Kid_One",
		"[12:00:05 INFO]: Steve lost connection: Disconnected":                                                "leave Steve",
		"[12:00:05 INFO]: <Bob> Steve lost connection: Disconnected":                                          "",
		"[12:00:05 INFO]: <Bob> Steve[/1.2.3.4:5] logged in with entity id 1 at":                              "",
		// Paper for Minecraft 26
		"[11:00:19 INFO]: System chat: LarveyHong joined the game":                                                  "join LarveyHong",
		"[11:02:40 INFO]: System chat: LarveyHong left the game":                                                    "leave LarveyHong",
		"[11:37:05 INFO]: System chat: .Larvs1774 joined the game":                                                  "join .Larvs1774",
		"[11:31:13 INFO]: Disconnecting MrAxolotl_ (/192.168.86.75:40906): You are not whitelisted on this server!": "reject MrAxolotl_",
		"[12:00:05 INFO]: <Bob> System chat: Alice joined the game":                                                 "",
		"[12:00:05 INFO]: System chat: <Bob> Alice joined the game":                                                 "",
	} {
		got := ""
		if m := reJavaJoin.FindStringSubmatch(line); m != nil {
			got = "join " + m[1]
		} else if m := reJavaLogin.FindStringSubmatch(line); m != nil {
			got = "join " + m[1]
		} else if m := reJavaNotAllow.FindStringSubmatch(line); m != nil {
			got = "reject " + m[1]
		} else if m := reJavaLeave.FindStringSubmatch(line); m != nil {
			got = "leave " + m[1]
		} else if m := reJavaLost.FindStringSubmatch(line); m != nil {
			got = "leave " + m[1]
		}
		if got != want {
			t.Errorf("%q: got %q, want %q", line, got, want)
		}
	}
	for line, want := range map[string]bool{
		"[12:00:05 INFO]: Saved the game":                    true,
		"[12:00:05 INFO]: System chat: Saved the game":       true, // Paper for Minecraft 26
		"[12:00:05 INFO]: <Bob> Saved the game":              false,
		"[12:00:05 INFO]: <Bob> System chat: Saved the game": false,
		"[12:00:05 INFO]: System chat: <Bob> Saved the game": false,
	} {
		if reJavaSaved.MatchString(line) != want {
			t.Errorf("%q: save line match should be %v", line, want)
		}
	}
}
