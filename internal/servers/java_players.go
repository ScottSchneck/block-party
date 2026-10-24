package servers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ScottSchneck/block-party/internal/notify"
)

// Players on Java servers. Java servers keep their allowlist in
// whitelist.json and operators in ops.json, by Minecraft account (UUID).
//
// While the server runs, the panel uses its own commands ("whitelist add",
// "op"), which look the account up and save the files. While it's stopped,
// the panel looks the account up itself (Mojang's profile service) and edits
// the files; if that isn't possible (no internet), the change waits and is
// made by command the next time the server starts.

// javaNamePattern is a Java Edition username.
var javaNamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{3,16}$`)

// ErrBadJavaName is returned for names that can't be a Java username.
var ErrBadJavaName = errors.New("that doesn't look like a Minecraft Java username (3 to 16 letters, numbers or _)")

func cleanJavaName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if !javaNamePattern.MatchString(name) {
		return "", ErrBadJavaName
	}
	return name, nil
}

type javaListEntry struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

type javaOpEntry struct {
	UUID                string `json:"uuid"`
	Name                string `json:"name"`
	Level               int    `json:"level"`
	BypassesPlayerLimit bool   `json:"bypassesPlayerLimit"`
}

// JavaPending is a change to make the next time a Java server starts.
type JavaPending struct {
	Name   string `json:"name"`
	Action string `json:"action"` // "allow", "unallow", "op", "deop"
}

func (s *Server) whitelistPath() string { return filepath.Join(s.dir(), "whitelist.json") }
func (s *Server) opsPath() string       { return filepath.Join(s.dir(), "ops.json") }

// running reports whether the server's process is up and taking commands.
func (s *Server) runningLocked() bool { return s.cmd != nil && !s.stopping && s.state == StateRunning }

// javaLookup finds a Java account's UUID with Mojang's profile service.
func (m *Manager) javaLookup(ctx context.Context, name string) (string, string, error) {
	var p struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.opts.MojangProfiles+name, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", m.javaUserAgent())
	resp, err := m.http().Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return "", "", fmt.Errorf("there's no Minecraft Java account called %s", name)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("Mojang answered %s", resp.Status)
	}
	if err := decodeJSON(resp, &p); err != nil {
		return "", "", err
	}
	id := strings.ReplaceAll(p.ID, "-", "")
	if len(id) != 32 {
		return "", "", errors.New("Mojang gave an odd answer")
	}
	return id[0:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:], p.Name, nil
}

// javaPlayers is the Players page of a Java server.
func (s *Server) javaPlayers() (PlayersView, error) {
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	v := PlayersView{Java: true, Consoles: s.ConsolesOn(), AllowlistEnabled: s.allowlistEnabled(), Online: []Player{}, Allowlist: []ListedName{}, Operators: []ListedName{}, Attempts: []JoinAttempt{}}
	allow, err := readJSONList[javaListEntry](s.whitelistPath())
	if err != nil {
		return v, err
	}
	for _, a := range allow {
		v.Allowlist = append(v.Allowlist, ListedName{Name: a.Name, Console: isConsoleName(a.Name)})
	}
	ops, err := readJSONList[javaOpEntry](s.opsPath())
	if err != nil {
		return v, err
	}
	for _, o := range ops {
		v.Operators = append(v.Operators, ListedName{Name: o.Name, Console: isConsoleName(o.Name)})
	}
	st := s.Status()
	v.Online = st.Players
	v.Running = st.State == StateRunning
	s.mu.Lock()
	for _, p := range s.meta.JavaPending {
		switch p.Action {
		case "allow":
			v.Allowlist = append(v.Allowlist, ListedName{Name: p.Name, Pending: true, Console: isConsoleName(p.Name)})
		case "op":
			v.Operators = append(v.Operators, ListedName{Name: p.Name, Pending: true, Console: isConsoleName(p.Name)})
		}
	}
	v.Attempts = append(v.Attempts, s.attempts...)
	s.mu.Unlock()
	for _, l := range [][]ListedName{v.Allowlist, v.Operators} {
		sort.Slice(l, func(i, j int) bool { return strings.ToLower(l[i].Name) < strings.ToLower(l[j].Name) })
	}
	sort.Slice(v.Attempts, func(i, j int) bool { return v.Attempts[i].At.After(v.Attempts[j].At) })
	return v, nil
}

// javaCommandIfRunning sends a command if the server is running.
func (s *Server) javaCommandIfRunning(line string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.runningLocked() {
		return false
	}
	s.addLineLocked("> " + line)
	return s.sendLocked(line)
}

// javaChange makes an allowlist or operator change: by command while the
// server runs, otherwise in the files (or later, if the account can't be
// looked up now).
func (s *Server) javaChange(name, action string) (pending bool, err error) {
	if s.isHytale() {
		return s.hytaleChange(name, action)
	}
	if isConsoleName(name) {
		return s.consoleChange(name, action)
	}
	name, err = cleanJavaName(name)
	if err != nil {
		return false, err
	}
	command := map[string]string{"allow": "whitelist add ", "unallow": "whitelist remove ", "op": "op ", "deop": "deop "}[action] + name
	s.dropJavaPending(name, action)
	if s.javaCommandIfRunning(command) {
		time.Sleep(300 * time.Millisecond) // let it save the file before the page reloads
		return false, nil
	}
	var uuid, real string
	if action == "allow" || action == "op" {
		ctx, cancel := context.WithTimeout(s.m.ctx, 10*time.Second)
		var lerr error
		uuid, real, lerr = s.m.javaLookup(ctx, name)
		cancel()
		if lerr != nil && strings.Contains(lerr.Error(), "no Minecraft Java account") {
			return false, lerr
		}
		if lerr != nil {
			uuid = "" // can't look them up now: by command at the next start
		}
	}
	// Edit the files only while no process is up: one that's starting
	// may already have read them (and would write its own copy back).
	s.mu.Lock()
	up := s.cmd != nil
	if up && s.runningLocked() {
		s.mu.Unlock()
		if s.javaCommandIfRunning(command) { // it finished starting meanwhile
			time.Sleep(300 * time.Millisecond)
			return false, nil
		}
		s.mu.Lock()
		up = s.cmd != nil
	}
	if up || ((action == "allow" || action == "op") && uuid == "") {
		s.meta.JavaPending = append(s.meta.JavaPending, JavaPending{Name: name, Action: action})
		s.mu.Unlock()
		return true, s.saveMeta()
	}
	s.mu.Unlock()
	if real == "" {
		real = name
	}
	return false, s.editJavaLists(real, uuid, action)
}

// dropJavaPending removes a queued change for a name (any queued change that
// the new one replaces).
func (s *Server) dropJavaPending(name, action string) {
	opposite := map[string]string{"allow": "unallow", "unallow": "allow", "op": "deop", "deop": "op",
		"allowlist-on": "allowlist-off", "allowlist-off": "allowlist-on"}[action]
	s.mu.Lock()
	kept := s.meta.JavaPending[:0]
	changed := false
	for _, p := range s.meta.JavaPending {
		if strings.EqualFold(p.Name, name) && (p.Action == action || p.Action == opposite) {
			changed = true
			continue
		}
		kept = append(kept, p)
	}
	s.meta.JavaPending = kept
	s.mu.Unlock()
	if changed {
		_ = s.saveMeta()
	}
}

func (s *Server) editJavaLists(name, uuid, action string) error {
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	switch action {
	case "allow", "unallow":
		list, err := readJSONList[javaListEntry](s.whitelistPath())
		if err != nil {
			return err
		}
		kept := list[:0]
		for _, e := range list {
			if !strings.EqualFold(e.Name, name) && (uuid == "" || e.UUID != uuid) {
				kept = append(kept, e)
			}
		}
		if action == "allow" {
			kept = append(kept, javaListEntry{UUID: uuid, Name: name})
		}
		return writeJSONList(s.whitelistPath(), kept)
	default:
		list, err := readJSONList[javaOpEntry](s.opsPath())
		if err != nil {
			return err
		}
		kept := list[:0]
		for _, e := range list {
			if !strings.EqualFold(e.Name, name) && (uuid == "" || e.UUID != uuid) {
				kept = append(kept, e)
			}
		}
		if action == "op" {
			kept = append(kept, javaOpEntry{UUID: uuid, Name: name, Level: 4})
		}
		return writeJSONList(s.opsPath(), kept)
	}
}

// applyJavaPendingLocked sends the queued changes once the server is up.
// Caller holds s.mu.
func (s *Server) applyJavaPendingLocked() {
	if len(s.meta.JavaPending) == 0 {
		return
	}
	// Console removals first: their reload reads the edited allowlist
	// before any other change makes the server save its own copy.
	pending := append([]JavaPending(nil), s.meta.JavaPending...)
	sort.SliceStable(pending, func(i, j int) bool {
		first := func(p JavaPending) bool { return isConsoleName(p.Name) && p.Action == "unallow" }
		return first(pending[i]) && !first(pending[j])
	})
	var kept []JavaPending
	for _, p := range pending {
		if isConsoleName(p.Name) {
			if s.applyConsolePendingLocked(p) {
				kept = append(kept, p)
			}
			continue
		}
		cmd := map[string]string{"allow": "whitelist add ", "unallow": "whitelist remove ", "op": "op ", "deop": "deop "}[p.Action] + p.Name
		s.addLineLocked("> " + cmd)
		s.sendLocked(cmd)
	}
	s.meta.JavaPending = kept
	s.saveMetaLater()
}

// ---- log lines ----

var (
	// The server's own lines start with "[12:00:00 INFO]: " (or
	// "[12:00:00] [Server thread/INFO]: "); matching from there means chat
	// can't pretend to be one ("<Bob> Alice left the game").
	javaPrefix    = `^(?:\[[^\]]*\]\s*)+:\s`
	reJavaDone    = regexp.MustCompile(javaPrefix + `Done \([0-9.,]+s\)! For help`)
	reJavaVersion = regexp.MustCompile(javaPrefix + `Starting minecraft server version ([0-9]+(?:\.[0-9]+){1,3})`)
	// Names: Java usernames, and console players' ".Gamer_Tag" (gamertags
	// can have letters of any language, and #).
	javaPlayerName = `([\p{L}\p{N}_.#]{2,17})`
	// Paper for Minecraft 26 logs join and leave messages as
	// "System chat: Steve joined the game".
	javaSystemChat = `(?:System chat: )?`
	reJavaJoin     = regexp.MustCompile(javaPrefix + javaSystemChat + javaPlayerName + `(?: \(formerly known as [\p{L}\p{N}_.#]+\))? joined the game$`)
	reJavaLeave    = regexp.MustCompile(javaPrefix + javaSystemChat + javaPlayerName + ` left the game$`)
	// The server's own lines for the same, which plugins can't change (they
	// often change or hide "joined the game" and "left the game"):
	// "Steve[/1.2.3.4:5] logged in with entity id 7 at (...)" and
	// "Steve lost connection: Disconnected".
	reJavaLogin    = regexp.MustCompile(javaPrefix + javaPlayerName + `\[/[^\]]*\] logged in with entity id `)
	reJavaLost     = regexp.MustCompile(javaPrefix + javaPlayerName + ` lost connection: `)
	reJavaNotAllow = regexp.MustCompile(javaPrefix + `(?:Disconnecting )?(?:com\.mojang\.authlib\.GameProfile.*?name=)?` + javaPlayerName + `(?: \(/|,| ).*(?:not white-?listed|not on the white-?list)`)
	// Paper for Minecraft 26 logs it as "System chat: Saved the game".
	reJavaSaved   = regexp.MustCompile(javaPrefix + javaSystemChat + `Saved the game\s*$`)
	reJavaServer  = regexp.MustCompile(javaPrefix)
	javaSaveLines = []string{"Automatic saving is now disabled", "Saving the game (this may take a moment!)", "Saved the game", "Automatic saving is now enabled", "Saving is already turned"}
)

// onJavaLineLocked handles a line from a Java server. Caller holds s.mu.
func (s *Server) onJavaLineLocked(line string) {
	switch {
	case reJavaDone.MatchString(line):
		if s.state == StateStarting {
			s.state = StateRunning
			s.message = ""
			s.m.log.Info("server running", "id", s.meta.ID)
			s.applyJavaPendingLocked()
		}
	case reJavaJoin.MatchString(line) || reJavaLogin.MatchString(line):
		m := reJavaJoin.FindStringSubmatch(line)
		if m == nil {
			m = reJavaLogin.FindStringSubmatch(line)
		}
		name := m[1]
		if _, ok := s.players[name]; ok {
			return // both lines come for one join
		}
		s.players[name] = Player{Name: name, Since: time.Now()}
		s.playedSinceBackup = true
		s.m.playtime.kick() // a kid whose time is up is moved off at once
		if isConsoleName(name) {
			s.onConsoleJoinLocked(name)
		}
		s.notifyLocked(notify.PlayerJoined, name+" is playing on "+s.meta.Name, name+" joined "+s.meta.Name+".",
			"play/"+s.meta.ID+"/"+strings.ToLower(name))
		kept := s.attempts[:0]
		for _, a := range s.attempts {
			if !strings.EqualFold(a.Name, name) {
				kept = append(kept, a)
			}
		}
		s.attempts = kept
	case reJavaVersion.MatchString(line) && s.state == StateStarting:
		if v := reJavaVersion.FindStringSubmatch(line)[1]; v != s.meta.Version {
			s.meta.Version, s.meta.Build = v, 0 // after a restore: the build is learned at the next update
			s.saveMetaLater()
		}
	case reJavaLeave.MatchString(line):
		s.playerLeftLocked(reJavaLeave.FindStringSubmatch(line)[1])
	case reJavaLost.MatchString(line):
		s.playerLeftLocked(reJavaLost.FindStringSubmatch(line)[1])
	case reJavaNotAllow.MatchString(line):
		name := reJavaNotAllow.FindStringSubmatch(line)[1]
		// With account checks on (online-mode), the name is real.
		s.m.saves.Add(1)
		go func() {
			defer s.m.saves.Done()
			s.noteJoinAttempt(name, "", s.checksSignIn())
		}()
	}
}

// ---- backups while running ----

// javaHoldSave stops a Java server's saving and has it save everything, so
// the files can be copied.
func (s *Server) javaHoldSave(ctx context.Context, held *exec.Cmd) error {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	if s.cmd == nil || s.cmd != held {
		s.mu.Unlock()
		return errors.New("the server stopped")
	}
	s.hotBackup = true
	s.javaSaved = ch
	ok := s.sendLocked("save-off") && s.sendLocked("save-all flush")
	s.mu.Unlock()
	if !ok {
		return errors.New("the server isn't taking commands")
	}
	select {
	case <-ch:
		return nil
	case <-time.After(holdTimeout):
		return errors.New("the server didn't finish saving within a minute")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// onJavaSaveLineLocked keeps save chatter out of the console during a
// backup and notices when the save is done.
func (s *Server) onJavaSaveLineLocked(line string) bool {
	if !s.hotBackup && !time.Now().Before(s.hideSaveUntil) {
		return false
	}
	if !reJavaServer.MatchString(line) {
		return false // chat, not the server
	}
	for _, c := range javaSaveLines {
		if strings.Contains(line, c) {
			if reJavaSaved.MatchString(line) && s.javaSaved != nil {
				select {
				case s.javaSaved <- struct{}{}:
				default:
				}
				s.javaSaved = nil
			}
			return true
		}
	}
	return false
}

// worldsOf lists what a Java world is made of: the overworld folder, and
// Paper's separate nether and end folders.
func javaWorldFolders(level string) []string {
	return []string{level, level + "_nether", level + "_the_end"}
}

// decodeJSON reads a small JSON answer.
func decodeJSON(resp *http.Response, v any) error {
	return jsonDecodeLimited(resp.Body, v)
}

// javaPlayerFiles are a Java server's player lists and plugin settings,
// kept in backups next to server.properties.
var javaSettingsFiles = []string{"ops.json", "banned-players.json", "banned-ips.json", "eula.txt", "bukkit.yml", "spigot.yml", "commands.yml", "help.yml", "permissions.yml"}

// javaSettingsDirs are folders of settings kept in backups.
var javaSettingsDirs = []string{"config", "plugins"}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
