package servers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Lobbies: Velocity (PaperMC's proxy) in front of Java servers. Players
// join the lobby's address and land on its first server; /server takes them
// to the others, without leaving the game. Velocity checks accounts and
// passes each player on with a signed note (its "modern forwarding"), so the
// servers behind it turn account checks off and only let players in through
// it: joining one of them directly is refused. Console players join the
// lobby too (Geyser and Floodgate for Velocity); the servers behind it don't
// need their own console support while they're linked.
//
// The panel owns velocity.toml and writes it at each start, from the
// servers linked on the lobby's Settings tab. Paper and Purpur servers can
// be linked (mod loaders need a mod for Velocity).

const (
	LoaderVelocity     = "velocity"
	velocityJar        = "velocity.jar"
	velocityConfigFile = "velocity.toml"
	velocitySecretFile = "forwarding.secret"
	paperGlobalFile    = "config/paper-global.yml"
)

// velocityConsolePlugins: Geyser and Floodgate for Velocity.
var velocityConsolePlugins = []consolePlugin{
	{Key: "geyser", Name: "Geyser-Velocity", File: "Geyser-Velocity.jar", Label: "Geyser", Required: true, Dir: "plugins", Source: "geyser:velocity"},
	{Key: "floodgate", Name: "floodgate", File: "floodgate-velocity.jar", Label: "Floodgate", Required: true, Dir: "plugins", Source: "geyser:velocity"},
}

func (s *Server) isVelocity() bool { return s.Loader() == LoaderVelocity }

// ---- the download ----

var reVelocityVersion = regexp.MustCompile(`^\d+\.\d+\.\d+(-SNAPSHOT)?$`)

func (m *Manager) velocityAPI() string {
	return strings.TrimSuffix(m.opts.PaperAPI, "/paper") + "/velocity"
}

// velocityVersions are Velocity's versions, newest first.
func (m *Manager) velocityVersions(ctx context.Context) ([]string, error) {
	var project struct {
		Versions map[string][]string `json:"versions"`
	}
	if err := m.getJSON(ctx, m.velocityAPI(), &project); err != nil {
		return nil, fmt.Errorf("couldn't ask PaperMC for Velocity's versions: %w", err)
	}
	var out []string
	for _, group := range project.Versions {
		for _, v := range group {
			if reVelocityVersion.MatchString(v) {
				out = append(out, v)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := strings.TrimSuffix(out[i], "-SNAPSHOT"), strings.TrimSuffix(out[j], "-SNAPSHOT")
		if a == b {
			return !strings.HasSuffix(out[i], "-SNAPSHOT") // a release over its snapshots
		}
		return versionNewer(a, b)
	})
	return out, nil
}

// velocityRelease is the newest Velocity build (a stable one if there is
// one; PaperMC marks most of Velocity's builds stable).
func (m *Manager) velocityRelease(ctx context.Context) (PaperRelease, error) {
	versions, err := m.velocityVersions(ctx)
	if err != nil {
		return PaperRelease{}, err
	}
	for i, v := range versions {
		if i >= 3 {
			break
		}
		var builds []paperBuild
		if err := m.getJSON(ctx, m.velocityAPI()+"/versions/"+v+"/builds", &builds); err != nil {
			return PaperRelease{}, fmt.Errorf("couldn't ask PaperMC for Velocity's builds: %w", err)
		}
		best := -1
		for bi, b := range builds {
			if _, ok := b.Downloads["server:default"]; !ok {
				continue
			}
			stable := strings.EqualFold(b.Channel, "STABLE")
			if best < 0 || (stable && !strings.EqualFold(builds[best].Channel, "STABLE")) ||
				(stable == strings.EqualFold(builds[best].Channel, "STABLE") && b.ID > builds[best].ID) {
				best = bi
			}
		}
		if best < 0 {
			continue
		}
		d := builds[best].Downloads["server:default"]
		sum := strings.ToLower(d.Checksums["sha256"])
		if d.URL == "" || len(sum) != 64 {
			continue
		}
		return PaperRelease{Version: v, Build: builds[best].ID, URL: d.URL, SHA256: sum}, nil
	}
	return PaperRelease{}, errors.New("PaperMC has no build of Velocity")
}

// ---- velocity.toml ----

// velocitySecret is the lobby's forwarding secret (made the first time).
func velocitySecret(dir string) (string, error) {
	p := filepath.Join(dir, velocitySecretFile)
	if b, err := os.ReadFile(p); err == nil {
		if s := strings.TrimSpace(string(b)); len(s) >= 16 {
			return s, nil
		}
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	secret := hex.EncodeToString(buf)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return secret, os.WriteFile(p, []byte(secret+"\n"), 0o600)
}

var tomlBareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func tomlString(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", " ", "\r", " ", "\t", " ").Replace(s)
	return `"` + s + `"`
}

// writeVelocityConfig writes the lobby's velocity.toml: its port and name,
// and the servers linked to it (the first is where players land). Don't
// hold s.mu: it asks the linked servers for their ports.
func (s *Server) writeVelocityConfig() error {
	s.mu.Lock()
	port, name, dir := s.meta.Port, s.meta.Name, s.dir()
	ids := append([]string(nil), s.meta.LobbyServers...)
	s.mu.Unlock()
	if _, err := velocitySecret(dir); err != nil {
		return err
	}
	var servers, try []string
	for _, id := range ids {
		b, ok := s.m.Get(id)
		if !ok || !tomlBareKey.MatchString(id) {
			continue
		}
		servers = append(servers, fmt.Sprintf("%s = %s", id, tomlString("127.0.0.1:"+strconv.Itoa(b.Status().Port))))
		try = append(try, tomlString(id))
	}
	// Velocity reads the message of the day as MiniMessage: no tags.
	motd := strings.NewReplacer("<", "", ">", "").Replace(name)
	text := `# Written by Block Party each time the lobby starts. Choose its servers on
# its Settings tab; changes made here are replaced.
config-version = "2.7"
bind = ` + tomlString("0.0.0.0:"+strconv.Itoa(port)) + `
motd = ` + tomlString(motd) + `
show-max-players = 20
online-mode = true
force-key-authentication = true
prevent-client-proxy-connections = false
player-info-forwarding-mode = "modern"
forwarding-secret-file = "` + velocitySecretFile + `"
announce-forge = false
kick-existing-players = false
ping-passthrough = "DISABLED"
enable-player-address-logging = true

[servers]
` + strings.Join(servers, "\n") + `
try = [` + strings.Join(try, ", ") + `]

[forced-hosts]

[advanced]
compression-threshold = 256
compression-level = -1
login-ratelimit = 3000
connection-timeout = 5000
read-timeout = 30000
haproxy-protocol = false
tcp-fast-open = false
bungee-plugin-message-channel = true
show-ping-requests = false
failover-on-unexpected-server-disconnect = true
announce-proxy-commands = true
log-command-executions = false
log-player-connections = true
accepts-transfers = false

[query]
enabled = false
port = ` + strconv.Itoa(port) + `
map = "Velocity"
show-plugins = false
`
	return os.WriteFile(filepath.Join(dir, velocityConfigFile), []byte(text), 0o644)
}

// prepareVelocityStart writes a lobby's config before it starts.
func (s *Server) prepareVelocityStart() {
	if !s.isJava() || !s.isVelocity() {
		return
	}
	if err := s.writeVelocityConfig(); err != nil {
		s.say("Couldn't write velocity.toml: " + err.Error())
	}
}

// ---- the servers behind a lobby ----

// applyProxyLocked sets a server linked to a lobby up for it: account
// checks off (the lobby does them), and Paper's Velocity support on with
// the lobby's secret. Caller holds s.mu.
func (s *Server) applyProxyLocked(p *properties) error {
	if s.meta.Proxy == "" {
		return nil
	}
	p.set("online-mode", "false")
	return setPaperVelocity(s.dir(), true, s.meta.ProxySecret)
}

// setPaperVelocity turns Paper's Velocity support on (with the secret) or
// off in config/paper-global.yml.
func setPaperVelocity(dir string, on bool, secret string) error {
	path := filepath.Join(dir, paperGlobalFile)
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	text := string(b)
	text = setYAML(text, []string{"proxies", "velocity", "enabled"}, strconv.FormatBool(on))
	if on {
		text = setYAML(text, []string{"proxies", "velocity", "online-mode"}, "true")
		text = setYAML(text, []string{"proxies", "velocity", "secret"}, "'"+secret+"'")
	}
	if text == string(b) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(text), 0o600)
}

// setYAML sets a value in a YAML file's text by its path of keys, keeping
// the rest as it is. Keys that aren't there are added, two spaces in per
// level, at the end of the deepest part of the path that is. It knows only
// the block-style maps Paper writes.
func setYAML(text string, path []string, value string) string {
	var lines []string
	if text != "" {
		lines = strings.Split(strings.TrimRight(text, "\n"), "\n")
	}
	type frame struct {
		indent int
		key    string
	}
	var stack []frame
	depth, end, indent := 0, len(lines)-1, -2 // deepest part of the path found, its block's last line, its indent
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "-") {
			continue
		}
		k, v, ok := strings.Cut(t, ":")
		if !ok {
			continue
		}
		in := len(line) - len(strings.TrimLeft(line, " "))
		for len(stack) > 0 && stack[len(stack)-1].indent >= in {
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, frame{in, strings.Trim(strings.TrimSpace(k), `"'`)})
		n := 0 // how much of the path this line's keys follow
		for n < len(stack) && n < len(path) && stack[n].key == path[n] {
			n++
		}
		if n == len(stack) && n == len(path) {
			comment := ""
			if c := strings.Index(v, " #"); c >= 0 {
				comment = v[c:]
			}
			lines[i] = line[:in] + k + ": " + value + comment
			return strings.Join(lines, "\n") + "\n"
		}
		switch {
		case n == len(stack) && n > depth:
			depth, end, indent = n, i, in // a deeper part of the path starts here
		case depth > 0 && n >= depth:
			end = i // still inside it
		}
	}
	var add []string
	for j := depth; j < len(path); j++ {
		pad := strings.Repeat(" ", indent+2+2*(j-depth))
		if depth == 0 {
			pad = strings.Repeat("  ", j)
		}
		if j == len(path)-1 {
			add = append(add, pad+path[j]+": "+value)
		} else {
			add = append(add, pad+path[j]+":")
		}
	}
	if depth == 0 {
		lines = append(lines, add...)
	} else {
		lines = append(lines[:end+1], append(add, lines[end+1:]...)...)
	}
	return strings.Join(lines, "\n") + "\n"
}

// LobbyServer is a Java server that can be (or is) behind a lobby.
type LobbyServer struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Port   int    `json:"port"`
	Linked bool   `json:"linked"`
	// Why it can't be linked ("" if it can).
	Why string `json:"why,omitempty"`
}

// LobbyView is a lobby's servers: the linked ones first, in order (the
// first is where players land), then the others.
type LobbyView struct {
	Servers []LobbyServer `json:"servers"`
}

func (m *Manager) lobbyView(lobby *Server) LobbyView {
	lobby.mu.Lock()
	linked := append([]string(nil), lobby.meta.LobbyServers...)
	lobbyID := lobby.meta.ID
	lobby.mu.Unlock()
	v := LobbyView{Servers: []LobbyServer{}}
	byID := map[string]LobbyServer{}
	var others []LobbyServer
	for _, st := range m.List() {
		if st.ID == lobbyID || st.Type != TypeJava {
			continue
		}
		ls := LobbyServer{ID: st.ID, Name: st.Name, Port: st.Port}
		b, _ := m.Get(st.ID)
		b.mu.Lock()
		proxy := b.meta.Proxy
		b.mu.Unlock()
		switch {
		case proxy == lobbyID:
			ls.Linked = true
		case st.Loader == LoaderVelocity:
			continue
		case proxy != "":
			ls.Why = "behind another lobby"
		case !runsPlugins(st.Loader):
			ls.Why = LoaderName(st.Loader) + " servers need a mod to sit behind a lobby"
		}
		byID[st.ID] = ls
		if !ls.Linked {
			others = append(others, ls)
		}
	}
	for _, id := range linked {
		if ls, ok := byID[id]; ok && ls.Linked {
			v.Servers = append(v.Servers, ls)
		}
	}
	sort.SliceStable(others, func(i, j int) bool { return strings.ToLower(others[i].Name) < strings.ToLower(others[j].Name) })
	v.Servers = append(v.Servers, others...)
	return v
}

// SetLobbyServers chooses the servers behind a lobby, in order (players
// land on the first). Servers taken out go back to taking players
// themselves. The changes apply when each server next starts.
func (m *Manager) SetLobbyServers(lobbyID string, ids []string) error {
	lobby, ok := m.Get(lobbyID)
	if !ok || !lobby.isJava() || !lobby.isVelocity() {
		return errors.New("that isn't a lobby")
	}
	if len(ids) > 20 {
		return errors.New("a lobby takes up to 20 servers")
	}
	seen := map[string]bool{}
	var backends []*Server
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		b, ok := m.Get(id)
		if !ok || id == lobbyID {
			return errors.New("there's no such server")
		}
		st := b.Status()
		b.mu.Lock()
		proxy := b.meta.Proxy
		b.mu.Unlock()
		switch {
		case st.Type != TypeJava || !runsPlugins(st.Loader):
			return fmt.Errorf("%s can't sit behind a lobby: only Paper and Purpur servers can", st.Name)
		case proxy != "" && proxy != lobbyID:
			return fmt.Errorf("%s is behind another lobby already", st.Name)
		case !tomlBareKey.MatchString(id):
			return fmt.Errorf("%s has an ID Velocity can't use", st.Name)
		}
		backends = append(backends, b)
	}
	secret, err := velocitySecret(lobby.dir())
	if err != nil {
		return err
	}
	lobby.mu.Lock()
	old := lobby.meta.LobbyServers
	list := make([]string, 0, len(backends))
	for _, b := range backends {
		list = append(list, b.ID())
	}
	lobby.meta.LobbyServers = list
	lobby.mu.Unlock()
	if err := lobby.saveMeta(); err != nil {
		return err
	}
	for _, id := range old {
		if !seen[id] {
			if b, ok := m.Get(id); ok {
				b.leaveLobby(lobbyID)
			}
		}
	}
	for _, b := range backends {
		if err := b.joinLobby(lobbyID, secret, lobby.Status().Name); err != nil {
			return err
		}
	}
	if err := lobby.writeVelocityConfig(); err != nil {
		return err
	}
	lobby.say(fmt.Sprintf("Servers behind this lobby: %d. Restart the lobby and those servers to use the change.", len(list)))
	return nil
}

// joinLobby links a server to a lobby. Its console support is turned off
// while it's linked (console players join through the lobby).
func (s *Server) joinLobby(lobbyID, secret, lobbyName string) error {
	s.mu.Lock()
	if s.meta.Proxy == lobbyID && s.meta.ProxySecret == secret {
		s.mu.Unlock()
		return nil
	}
	if s.meta.Proxy == "" {
		s.meta.ProxyNoConsoles = s.meta.NoConsoles
		p, err := readProperties(s.propertiesPath())
		if err == nil {
			s.meta.ProxyOnline, _ = p.get("online-mode")
		}
	}
	s.meta.Proxy, s.meta.ProxySecret, s.meta.NoConsoles = lobbyID, secret, true
	err := s.applyJavaProperties(false)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := s.saveMeta(); err != nil {
		return err
	}
	s.applyConsoleFileChanges()
	s.say("This server is now behind the lobby " + lobbyName + ": players (and consoles) join through it. Restart it to use the change.")
	return nil
}

// leaveLobby unlinks a server from its lobby: it takes players itself
// again, with its account checks and console support as they were.
func (s *Server) leaveLobby(lobbyID string) {
	s.mu.Lock()
	if s.meta.Proxy != lobbyID {
		s.mu.Unlock()
		return
	}
	online := s.meta.ProxyOnline
	if online == "" || online == "false" {
		online = "true"
	}
	s.meta.Proxy, s.meta.ProxySecret = "", ""
	s.meta.NoConsoles, s.meta.ProxyNoConsoles, s.meta.ProxyOnline = s.meta.ProxyNoConsoles, false, ""
	if p, err := readProperties(s.propertiesPath()); err == nil {
		p.set("online-mode", online)
		_ = p.write(s.propertiesPath())
	}
	err := setPaperVelocity(s.dir(), false, "")
	s.mu.Unlock()
	if err != nil {
		s.say("Couldn't turn off Velocity support in " + paperGlobalFile + ": " + err.Error())
	}
	_ = s.saveMeta()
	s.say("This server is no longer behind a lobby: players join it directly again. Restart it to use the change (and Update to put console support back, if it was on).")
}

// lobbyName is the name of the lobby a server is behind ("" if none).
func (m *Manager) lobbyName(id string) string {
	if l, ok := m.Get(id); ok {
		return l.Status().Name
	}
	return ""
}

// lobbyDeleted unlinks a deleted lobby's servers, so they can be joined
// again.
func (m *Manager) lobbyDeleted(meta Meta) {
	for _, id := range meta.LobbyServers {
		if b, ok := m.Get(id); ok {
			b.leaveLobby(meta.ID)
		}
	}
}

// onVelocityLineLocked handles a line from a lobby. Caller holds s.mu.
// Players aren't counted here: the servers behind it count them (counting
// both would count play time twice).
func (s *Server) onVelocityLineLocked(line string) {
	if reVelocityDone.MatchString(line) && s.state == StateStarting {
		s.state = StateRunning
		s.message = ""
		s.m.log.Info("server running", "id", s.meta.ID)
	}
}

// behindLobby: the server is linked to a lobby.
func (s *Server) behindLobby() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta.Proxy != ""
}
