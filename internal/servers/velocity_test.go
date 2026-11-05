package servers

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSetYAML(t *testing.T) {
	paper := `# Paper's settings
_version: 29
proxies:
  bungee-cord:
    online-mode: true
  proxy-protocol: false
  velocity:
    enabled: false # off
    online-mode: false
    secret: ''
scoreboards:
  save-empty-scoreboard-teams: true
`
	got := setYAML(paper, []string{"proxies", "velocity", "enabled"}, "true")
	got = setYAML(got, []string{"proxies", "velocity", "secret"}, "'abc'")
	want := strings.Replace(strings.Replace(paper, "enabled: false # off", "enabled: true # off", 1), "secret: ''", "secret: 'abc'", 1)
	if got != want {
		t.Errorf("in place:\n%s", got)
	}
	// Missing keys are added in their place.
	got = setYAML("proxies:\n  proxy-protocol: false\nother: 1\n", []string{"proxies", "velocity", "enabled"}, "true")
	if got != "proxies:\n  proxy-protocol: false\n  velocity:\n    enabled: true\nother: 1\n" {
		t.Errorf("added under a parent:\n%s", got)
	}
	if got := setYAML("", []string{"proxies", "velocity", "enabled"}, "true"); got != "proxies:\n  velocity:\n    enabled: true\n" {
		t.Errorf("a new file:\n%s", got)
	}
	// Same key name elsewhere isn't touched.
	got = setYAML("other:\n  velocity:\n    enabled: false\n", []string{"proxies", "velocity", "enabled"}, "true")
	if !strings.Contains(got, "other:\n  velocity:\n    enabled: false\n") || !strings.Contains(got, "proxies:\n  velocity:\n    enabled: true") {
		t.Errorf("another block:\n%s", got)
	}
}

func TestLobby(t *testing.T) {
	f := newFakeJavaWorld(t)
	m := newJavaManager(t, f)
	mk := func(name string) *Server {
		st, err := m.CreateJava(name, 1024)
		if err != nil {
			t.Fatal(err)
		}
		s, _ := m.Get(st.ID)
		waitFor(t, s, name+" running", state(StateRunning))
		return s
	}
	a, b := mk("Alpha"), mk("Bravo")
	st, err := m.CreateJavaOf("Lobby", 1024, JavaChoice{Loader: LoaderVelocity}, "Steve_1")
	if err != nil {
		t.Fatal(err)
	}
	lobby, _ := m.Get(st.ID)
	waitFor(t, lobby, "lobby running", state(StateRunning))
	if st := lobby.Status(); st.Loader != LoaderVelocity || st.Version != "3.4.0-SNAPSHOT" || st.KeepVersion {
		t.Errorf("lobby: %+v", st)
	}
	if readFile(t, filepath.Join(lobby.dir(), "velocity.jar")) != "velocity proxy" || !fileExists(filepath.Join(lobby.dir(), "plugins", "Geyser-Velocity.jar")) {
		t.Error("Velocity and its console support")
	}
	if got := lastArgs(t, lobby); !strings.HasSuffix(got, "-jar velocity.jar") || !strings.Contains(got, "-DgeyserUdpPort=") {
		t.Errorf("started with %q", got)
	}
	if fileExists(filepath.Join(lobby.dir(), "server.properties")) {
		t.Error("a lobby has no server.properties")
	}

	if err := m.SetLobbyServers(lobby.ID(), []string{b.ID(), a.ID()}); err != nil {
		t.Fatal(err)
	}
	toml := readFile(t, filepath.Join(lobby.dir(), velocityConfigFile))
	for _, want := range []string{`bind = "0.0.0.0:` + strconv.Itoa(lobby.Status().Port) + `"`, b.ID() + ` = "127.0.0.1:` + strconv.Itoa(b.Status().Port) + `"`,
		`try = ["` + b.ID() + `", "` + a.ID() + `"]`, `player-info-forwarding-mode = "modern"`} {
		if !strings.Contains(toml, want) {
			t.Errorf("velocity.toml has no %s:\n%s", want, toml)
		}
	}
	secret := strings.TrimSpace(readFile(t, filepath.Join(lobby.dir(), velocitySecretFile)))
	for _, s := range []*Server{a, b} {
		if st := s.Status(); st.Proxy != lobby.ID() || st.Consoles {
			t.Errorf("%s: %+v", s.ID(), st)
		}
		if p := readFile(t, s.propertiesPath()); !strings.Contains(p, "online-mode=false") {
			t.Errorf("%s: online-mode stays on:\n%s", s.ID(), p)
		}
		if y := readFile(t, filepath.Join(s.dir(), paperGlobalFile)); !strings.Contains(y, "enabled: true") || !strings.Contains(y, "secret: '"+secret+"'") {
			t.Errorf("%s: paper-global.yml:\n%s", s.ID(), y)
		}
	}
	if err := a.SetConsoles(true); err == nil {
		t.Error("consoles join a linked server through the lobby")
	}
	// Console players still go on its allowlist (they come through the lobby).
	if _, err := a.consoleChange(".Kid_One", "allow"); err != nil {
		t.Errorf("a console player on a linked server's allowlist: %v", err)
	}
	if wl := readFile(t, filepath.Join(a.dir(), "whitelist.json")); !strings.Contains(wl, "00000000-0000-0000-0009-01eed05d1002") {
		t.Errorf("allowlist: %s", wl)
	}
	v, _ := lobby.Settings()
	if v.Lobby == nil || len(v.Lobby.Servers) != 2 || v.Lobby.Servers[0].ID != b.ID() || !v.Lobby.Servers[0].Linked {
		t.Errorf("settings: %+v", v.Lobby)
	}
	if _, err := lobby.SaveSettings(map[string]string{"motd": "x"}); err == nil {
		t.Error("a lobby has no server.properties settings")
	}

	// Taking one out puts it back as it was.
	if err := m.SetLobbyServers(lobby.ID(), []string{a.ID()}); err != nil {
		t.Fatal(err)
	}
	if st := b.Status(); st.Proxy != "" || !st.Consoles {
		t.Errorf("bravo after leaving: %+v", st)
	}
	if p := readFile(t, b.propertiesPath()); !strings.Contains(p, "online-mode=true") {
		t.Errorf("bravo's online-mode:\n%s", p)
	}
	if y := readFile(t, filepath.Join(b.dir(), paperGlobalFile)); !strings.Contains(y, "enabled: false") {
		t.Errorf("bravo's paper-global.yml:\n%s", y)
	}
	if err := m.SetLobbyServers(lobby.ID(), []string{lobby.ID()}); err == nil {
		t.Error("a lobby can't be behind itself")
	}

	// Stopping a lobby uses Velocity's command.
	if err := lobby.Stop(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, lobby, "lobby stopped", state(StateStopped))
	waitConsole(t, lobby, "Shutting down the proxy")

	// Deleting the lobby lets its servers take players again.
	if err := m.DeleteServer(lobby.ID()); err != nil {
		t.Fatal(err)
	}
	if st := a.Status(); st.Proxy != "" {
		t.Errorf("alpha after the lobby went: %+v", st)
	}
}
