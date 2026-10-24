package servers

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ScottSchneck/block-party/internal/notify"
)

// Hytale servers. The server files (HytaleServer.jar and Assets.zip, a few
// GB) come from Hytale's own download service with the owner's Hytale
// sign-in (see hytale_auth.go), checked against its SHA-256. They run on
// Java 25, which is downloaded like a Java server's. Players connect over
// UDP (QUIC) on the server's port.
//
// The server keeps its worlds in universe/ and its settings in config.json,
// whitelist.json and permissions.json, which it writes back while it runs:
// the panel changes those files only while the server is stopped, and by
// command while it runs.

// TypeHytale is Meta.Type for Hytale servers.
const TypeHytale = "hytale"

const (
	hytaleJar         = "HytaleServer.jar"
	hytaleAOT         = "HytaleServer.aot"
	hytaleAssets      = "Assets.zip"
	hytaleJavaMajor   = 25
	hytaleMemoryMB    = 4096
	hytaleWorldFolder = "universe"
	hytalePatchline   = "release"
	maxHytaleDownload = 16 << 30
)

// hytaleSettingsFiles are a Hytale server's settings, kept in backups.
var hytaleSettingsFiles = []string{"config.json", "permissions.json", "whitelist.json", "bans.json"}

// hytaleDirs are a Hytale server's folders kept in backups.
var hytaleDirs = []string{hytaleWorldFolder, "mods"}

func (s *Server) isHytale() bool  { return s.meta.Type == TypeHytale }
func (s *Server) isBedrock() bool { return s.meta.Type == TypeBedrock || s.meta.Type == "" }

// ---- downloads ----

// HytaleRelease is a version of the Hytale server.
type HytaleRelease struct {
	Version  string
	Download string // path at the download service
	SHA256   string
}

// latestHytale asks Hytale's download service for the newest release.
func (m *Manager) latestHytale(ctx context.Context) (HytaleRelease, error) {
	access, err := m.hytaleToken(ctx, hytaleDownloaderClient)
	if err != nil {
		return HytaleRelease{}, err
	}
	var signed struct {
		URL string `json:"url"`
	}
	if err := m.hytaleGet(ctx, m.opts.HytaleAccounts+"/game-assets/version/"+hytalePatchline+".json", access, &signed); err != nil {
		return HytaleRelease{}, fmt.Errorf("couldn't ask Hytale for its newest server: %w", err)
	}
	if signed.URL == "" {
		return HytaleRelease{}, errors.New("Hytale's download service gave an odd answer")
	}
	var v struct {
		Version     string `json:"version"`
		DownloadURL string `json:"download_url"`
		SHA256      string `json:"sha256"`
	}
	if err := m.getJSON(ctx, signed.URL, &v); err != nil {
		return HytaleRelease{}, fmt.Errorf("couldn't read Hytale's release list: %w", err)
	}
	if v.Version == "" || v.DownloadURL == "" || len(v.SHA256) != 64 {
		return HytaleRelease{}, errors.New("Hytale's release list gave an odd answer")
	}
	return HytaleRelease{Version: v.Version, Download: v.DownloadURL, SHA256: strings.ToLower(v.SHA256)}, nil
}

// hytaleNewer reports whether version a is newer than b (versions look like
// 2026.01.13-1a2b3c4).
func hytaleNewer(a, b string) bool {
	if a == "" || a == b {
		return false
	}
	if b == "" {
		return true
	}
	da, _, _ := strings.Cut(a, "-")
	db, _, _ := strings.Cut(b, "-")
	if da != db {
		return da > db
	}
	return a != b // same day, another build: the service's newest is newer
}

type hytaleCache struct {
	mu      sync.Mutex
	release HytaleRelease
	at      time.Time
	busy    bool
}

// LatestHytale returns the newest Hytale release, checking at most once an
// hour (only while signed in), without waiting.
func (m *Manager) LatestHytale() HytaleRelease {
	c := &m.hytaleLatest
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.busy && time.Since(c.at) > versionCheckEvery {
		c.busy = true
		go func() {
			ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
			defer cancel()
			r, err := m.latestHytale(ctx)
			c.mu.Lock()
			defer c.mu.Unlock()
			c.busy, c.at = false, time.Now()
			if err == nil {
				c.release = r
			}
		}()
	}
	return c.release
}

// installHytale downloads the Hytale server (and Java 25) into the server
// folder.
func (s *Server) installHytale(update bool) error {
	ctx, cancel := context.WithTimeout(s.m.ctx, 2*time.Hour)
	defer cancel()
	s.mu.Lock()
	current := s.meta.Version
	s.mu.Unlock()
	if !s.m.HytaleSignedIn() {
		return ErrHytaleSignIn
	}
	s.say("Asking Hytale for the current server…")
	r, err := s.m.latestHytale(ctx)
	if err != nil {
		return err
	}
	if _, err := s.m.ensureJava(ctx, hytaleJavaMajor, s.say); err != nil {
		return err
	}
	s.mu.Lock()
	s.meta.JavaMajor = hytaleJavaMajor
	s.mu.Unlock()
	have := fileExists(filepath.Join(s.dir(), hytaleJar)) && fileExists(filepath.Join(s.dir(), hytaleAssets))
	if update && have && !hytaleNewer(r.Version, current) {
		_ = s.saveMeta()
		s.say("Already on the current version (" + r.Version + ").")
		return nil
	}
	access, err := s.m.hytaleToken(ctx, hytaleDownloaderClient)
	if err != nil {
		return err
	}
	var signed struct {
		URL string `json:"url"`
	}
	if err := s.m.hytaleGet(ctx, s.m.opts.HytaleAccounts+"/game-assets/"+strings.TrimPrefix(r.Download, "/"), access, &signed); err != nil || signed.URL == "" {
		if err == nil {
			err = errors.New("no download address")
		}
		return fmt.Errorf("couldn't get the Hytale download: %w", err)
	}
	s.say("Downloading Hytale server " + r.Version + " (a few GB; this takes a while)…")
	if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		return err
	}
	zipFile, err := s.m.downloadLimited(ctx, signed.URL, s.m.serversDir(), r.SHA256, s.say, s.m.javaUserAgent(), maxHytaleDownload)
	if err != nil {
		return err
	}
	defer os.Remove(zipFile)
	s.say("Unpacking the Hytale server…")
	staging, err := os.MkdirTemp(s.m.serversDir(), ".staging-"+s.meta.ID+"-") // cleaned up at start if left behind
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := extractHytale(ctx, zipFile, staging); err != nil {
		return err
	}
	// Assets first, the jar last: a failure partway is reported as such.
	moved := 0
	for _, f := range []string{hytaleAssets, hytaleAOT, hytaleJar} {
		src := filepath.Join(staging, f)
		if !fileExists(src) {
			_ = os.Remove(filepath.Join(s.dir(), f)) // an AOT cache of an older version is no use
			continue
		}
		if err := os.Rename(src, filepath.Join(s.dir(), f)); err != nil {
			if moved > 0 && update {
				return &partialUpdateError{err: fmt.Errorf("couldn't put the new Hytale server in place: %w", err),
					hint: "Use Update to download the server again (the backup holds the worlds, which weren't changed)."}
			}
			return err
		}
		moved++
	}
	s.mu.Lock()
	s.meta.Version = r.Version
	s.mu.Unlock()
	if err := s.saveMeta(); err != nil {
		return err
	}
	_ = os.Remove(s.incompleteMarker())
	s.say("Hytale server " + r.Version + " is installed.")
	return nil
}

// extractHytale takes the server and its assets out of Hytale's download
// (Server/HytaleServer.jar, Server/HytaleServer.aot and Assets.zip; the
// rest, such as a client, is left out).
func extractHytale(ctx context.Context, zipPath, dir string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("the Hytale download isn't a valid zip: %w", err)
	}
	defer r.Close()
	found := map[string]bool{}
	for _, f := range r.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := filepath.Base(filepath.ToSlash(f.Name))
		dirName := filepath.ToSlash(filepath.Dir(filepath.ToSlash(f.Name)))
		ok := (name == hytaleAssets && (dirName == "." || dirName == "Server")) ||
			((name == hytaleJar || name == hytaleAOT) && (dirName == "Server" || dirName == "."))
		if !ok || found[name] || f.FileInfo().IsDir() {
			continue
		}
		found[name] = true
		in, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			in.Close()
			return err
		}
		_, err = io.Copy(out, io.LimitReader(in, maxHytaleDownload))
		in.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	if !found[hytaleJar] || !found[hytaleAssets] {
		return errors.New("the Hytale download doesn't have the server in it")
	}
	return nil
}

// ---- running ----

// hytaleCommand returns the command line and environment that start a
// Hytale server. Caller holds s.mu.
func (s *Server) hytaleCommand(sess hytaleSession) (string, []string, []string, error) {
	bin := s.m.javaBinary(hytaleJavaMajor)
	if bin == "" {
		return "", nil, nil, fmt.Errorf("Java %d isn't installed; use Update to install it", hytaleJavaMajor)
	}
	mem := s.meta.MemoryMB
	if mem <= 0 {
		mem = hytaleMemoryMB
	}
	args := []string{fmt.Sprintf("-Xms%dM", mem/2), fmt.Sprintf("-Xmx%dM", mem), "-XX:+UseG1GC",
		"-Dfile.encoding=UTF-8", "-Dterminal.jline=false", "-Dterminal.ansi=false"}
	if fileExists(filepath.Join(s.dir(), hytaleAOT)) {
		args = append(args, "-XX:AOTCache="+hytaleAOT)
	}
	args = append(args, "-jar", hytaleJar, "--assets", hytaleAssets, "--bind", "0.0.0.0:"+strconv.Itoa(s.meta.Port))
	env := append(os.Environ(), "HYTALE_DISABLE_UPDATES=1") // the panel updates it
	if sess.Session != "" {
		env = append(env, "HYTALE_SERVER_SESSION_TOKEN="+sess.Session, "HYTALE_SERVER_IDENTITY_TOKEN="+sess.Identity)
		args = append(args, "--owner-uuid", sess.Owner)
	}
	return bin, args, env, nil
}

// prepareHytaleStart gets a game session for a Hytale server that's about to
// start (it takes a moment, so it's done before the start, without locks).
// A session taken just before (by a restart, before stopping) is used.
func (s *Server) prepareHytaleStart() error {
	if !s.isHytale() {
		return nil
	}
	s.mu.Lock()
	fresh := s.hytaleSess.Session != "" && time.Since(s.hytaleSessAt) < 10*time.Minute // sessions last an hour
	idle := s.cmd == nil && !s.busy && !s.coldBackup
	s.mu.Unlock()
	if fresh || !idle {
		return nil // got one just now, or it won't start anyway
	}
	ctx, cancel := context.WithTimeout(s.m.ctx, 20*time.Second)
	defer cancel()
	sess, err := s.m.newHytaleSession(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hytaleSess, s.hytaleSessErr, s.hytaleSessAt = sess, err, time.Now()
	return err
}

// prepareHytaleRestart gets a session before a running Hytale server is
// stopped to restart, so a restart while Hytale's services are down leaves
// it running instead of stopped.
func (s *Server) prepareHytaleRestart() error {
	if !s.isHytale() {
		return nil
	}
	ctx, cancel := context.WithTimeout(s.m.ctx, 20*time.Second)
	defer cancel()
	sess, err := s.m.newHytaleSession(ctx)
	if err != nil {
		return fmt.Errorf("couldn't get a Hytale session for the restart, so the server keeps running: %w", err)
	}
	s.mu.Lock()
	s.hytaleSess, s.hytaleSessErr, s.hytaleSessAt = sess, nil, time.Now()
	s.mu.Unlock()
	return nil
}

// Hytale's log lines start with "[2026/01/13 12:00:00   INFO]" and the
// part of the server that wrote them ("[HytaleServer]"); chat comes from
// "[Hytale]" as "name: text", so it can't pass for these.
var (
	hytalePrefix   = `^\[[^\]]*\]\s+`
	reHytaleBooted = regexp.MustCompile(hytalePrefix + `\[HytaleServer\]\s+Hytale Server Booted!`)
	reHytaleJoin   = regexp.MustCompile(hytalePrefix + `\[HandshakeHandler\]\s+Mutual authentication complete for\s+(\w{1,32})\s+\(([0-9a-fA-F-]{36})\)`)
	reHytaleLeave  = regexp.MustCompile(hytalePrefix + `(?:\[Universe\|P\]\s+Removing player\s+'(\w{1,32})'\s+\(([0-9a-fA-F-]{36})\)|\[Hytale\]\s+\{Setup\(null.*?\),\s*(\w{1,32}),\s*([0-9a-fA-F-]{36}),\s*SECURE\}\s+was closed\.)`)
	reHytaleNoAuth = regexp.MustCompile(`No server tokens configured|Server starting unauthenticated|Token validation failed`)
	reANSI         = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")
)

// onHytaleLineLocked handles a line from a Hytale server. Caller holds s.mu.
func (s *Server) onHytaleLineLocked(line string) {
	switch {
	case reHytaleBooted.MatchString(line):
		if s.state == StateStarting {
			s.state = StateRunning
			s.message = ""
			s.m.log.Info("server running", "id", s.meta.ID)
			s.applyHytalePendingLocked()
		}
	case reHytaleJoin.MatchString(line):
		mm := reHytaleJoin.FindStringSubmatch(line)
		name, uuid := mm[1], strings.ToLower(mm[2])
		s.players[name] = Player{Name: name, Since: time.Now()}
		s.playedSinceBackup = true
		s.m.playtime.kick()
		if s.meta.HytaleNames == nil {
			s.meta.HytaleNames = map[string]string{}
		}
		if s.meta.HytaleNames[uuid] != name {
			s.meta.HytaleNames[uuid] = name
			s.saveMetaLater()
		}
		s.notifyLocked(notify.PlayerJoined, name+" is playing on "+s.meta.Name, name+" joined "+s.meta.Name+".",
			"play/"+s.meta.ID+"/"+strings.ToLower(name))
		s.applyHytaleOpOnJoinLocked(name)
	case reHytaleLeave.MatchString(line):
		mm := reHytaleLeave.FindStringSubmatch(line)
		name := mm[1]
		if name == "" {
			name = mm[3]
		}
		s.playerLeftLocked(name)
	case reHytaleNoAuth.MatchString(line):
		s.message = "Players can't join: the server isn't signed in to Hytale. The owner signs in again under Add server, Hytale, then restarts it."
	}
}

// ---- players ----

var errHytaleNotYet = errors.New("kicking players and sending messages aren't available for Hytale servers yet; use the Console tab")

var hytaleNamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{3,16}$`)

// ErrBadHytaleName is returned for names that can't be a Hytale username.
var ErrBadHytaleName = errors.New("that doesn't look like a Hytale username (3 to 16 letters, numbers or _)")

func (s *Server) whitelistFile() string   { return filepath.Join(s.dir(), "whitelist.json") }
func (s *Server) permissionsFile() string { return filepath.Join(s.dir(), "permissions.json") }

// hytaleWhitelist is whitelist.json: {"enabled": true, "list": [uuids]}.
type hytaleWhitelist struct {
	Enabled bool     `json:"enabled"`
	List    []string `json:"list"`
}

func readJSONObject(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("%s isn't valid: %w", filepath.Base(path), err)
	}
	if v == nil {
		v = map[string]any{}
	}
	return v, nil
}

func writeJSONObject(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".tmp", append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func (s *Server) readHytaleWhitelist() (hytaleWhitelist, map[string]any, error) {
	raw, err := readJSONObject(s.whitelistFile())
	if err != nil {
		return hytaleWhitelist{}, nil, err
	}
	var w hytaleWhitelist
	w.Enabled, _ = raw["enabled"].(bool)
	if list, ok := raw["list"].([]any); ok {
		for _, x := range list {
			if id, ok := x.(string); ok {
				w.List = append(w.List, strings.ToLower(id))
			}
		}
	}
	return w, raw, nil
}

func (s *Server) writeHytaleWhitelist(w hytaleWhitelist, raw map[string]any) error {
	if raw == nil {
		raw = map[string]any{}
	}
	list := w.List
	if list == nil {
		list = []string{}
	}
	raw["enabled"], raw["list"] = w.Enabled, list
	return writeJSONObject(s.whitelistFile(), raw)
}

// hytaleOps returns the players in the OP group of permissions.json.
func (s *Server) hytaleOps() ([]string, error) {
	raw, err := readJSONObject(s.permissionsFile())
	if err != nil {
		return nil, err
	}
	var out []string
	users, _ := raw["users"].(map[string]any)
	for id, u := range users {
		entry, _ := u.(map[string]any)
		groups, _ := entry["groups"].([]any)
		for _, g := range groups {
			if gs, _ := g.(string); strings.EqualFold(gs, "OP") {
				out = append(out, strings.ToLower(id))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// setHytaleOp puts a player in or takes them out of the OP group.
func (s *Server) setHytaleOp(uuid string, op bool) error {
	raw, err := readJSONObject(s.permissionsFile())
	if err != nil {
		return err
	}
	users, _ := raw["users"].(map[string]any)
	if users == nil {
		users = map[string]any{}
	}
	var key string
	for id := range users {
		if strings.EqualFold(id, uuid) {
			key = id
		}
	}
	if key == "" {
		key = uuid
	}
	entry, _ := users[key].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
	}
	groups, _ := entry["groups"].([]any)
	kept := []any{}
	for _, g := range groups {
		if gs, _ := g.(string); !strings.EqualFold(gs, "OP") {
			kept = append(kept, g)
		}
	}
	if op {
		kept = append(kept, "OP")
	}
	entry["groups"] = kept
	users[key] = entry
	raw["users"] = users
	if _, ok := raw["groups"]; !ok {
		raw["groups"] = map[string]any{"Default": []any{}, "OP": []any{"*"}}
	}
	return writeJSONObject(s.permissionsFile(), raw)
}

// hytaleName is a player's name, if the server has seen them.
func (s *Server) hytaleNameLocked(uuid string) string {
	if n := s.meta.HytaleNames[strings.ToLower(uuid)]; n != "" {
		return n
	}
	return uuid
}

// hytaleUUIDLocked finds a player the server has seen, by name.
func (s *Server) hytaleUUIDLocked(name string) string {
	for id, n := range s.meta.HytaleNames {
		if strings.EqualFold(n, name) {
			return id
		}
	}
	return ""
}

// hytalePlayers is the Players page of a Hytale server.
func (s *Server) hytalePlayers() (PlayersView, error) {
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	v := PlayersView{Hytale: true, Online: []Player{}, Allowlist: []ListedName{}, Operators: []ListedName{}, Attempts: []JoinAttempt{}}
	w, _, err := s.readHytaleWhitelist()
	if err != nil {
		return v, err
	}
	ops, err := s.hytaleOps()
	if err != nil {
		return v, err
	}
	st := s.Status()
	v.Online, v.Running, v.AllowlistEnabled = st.Players, st.State == StateRunning, w.Enabled
	s.mu.Lock()
	for _, id := range w.List {
		v.Allowlist = append(v.Allowlist, ListedName{Name: s.hytaleNameLocked(id)})
	}
	for _, id := range ops {
		v.Operators = append(v.Operators, ListedName{Name: s.hytaleNameLocked(id)})
	}
	for _, p := range s.meta.JavaPending {
		switch p.Action {
		case "allow":
			v.Allowlist = append(v.Allowlist, ListedName{Name: p.Name, Pending: true})
		case "op":
			v.Operators = append(v.Operators, ListedName{Name: p.Name, Pending: true})
		case "allowlist-on", "allowlist-off":
			v.AllowlistEnabled = p.Action == "allowlist-on"
		}
	}
	s.mu.Unlock()
	for _, l := range [][]ListedName{v.Allowlist, v.Operators} {
		sort.Slice(l, func(i, j int) bool { return strings.ToLower(l[i].Name) < strings.ToLower(l[j].Name) })
	}
	return v, nil
}

// hytaleCommandFor is the server command that makes a change.
func hytaleCommandFor(name, action string) string {
	switch action {
	case "allow":
		return "/whitelist add " + name
	case "unallow":
		return "/whitelist remove " + name
	case "op":
		return "/op add " + name
	case "deop":
		return "/op remove " + name
	case "allowlist-on":
		return "/whitelist enable"
	case "allowlist-off":
		return "/whitelist disable"
	}
	return ""
}

// hytaleChange makes an allowlist or operator change: by command while the
// server runs; in the files while it's stopped, for players it has seen;
// otherwise at the next start.
func (s *Server) hytaleChange(name, action string) (pending bool, err error) {
	name = strings.TrimSpace(name)
	if reUUID.MatchString(name) && (action == "unallow" || action == "deop") {
		return false, s.hytaleRemoveByID(strings.ToLower(name), action)
	}
	if !hytaleNamePattern.MatchString(name) {
		return false, ErrBadHytaleName
	}
	s.dropJavaPending(name, action)
	if s.javaCommandIfRunning(hytaleCommandFor(name, action)) {
		time.Sleep(300 * time.Millisecond) // let it save before the page reloads
		return false, nil
	}
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	file := s.permissionsFile()
	if action == "allow" || action == "unallow" {
		file = s.whitelistFile()
	}
	s.mu.Lock()
	uuid := s.hytaleUUIDLocked(name)
	// A file the server hasn't written yet is left to it (by command).
	if s.cmd != nil || uuid == "" || !fileExists(file) {
		s.meta.JavaPending = append(s.meta.JavaPending, JavaPending{Name: name, Action: action})
		s.mu.Unlock()
		return true, s.saveMeta()
	}
	s.mu.Unlock()
	switch action {
	case "allow", "unallow":
		w, raw, err := s.readHytaleWhitelist()
		if err != nil {
			return false, err
		}
		kept := w.List[:0]
		for _, id := range w.List {
			if id != uuid {
				kept = append(kept, id)
			}
		}
		if action == "allow" {
			kept = append(kept, uuid)
		}
		w.List = kept
		return false, s.writeHytaleWhitelist(w, raw)
	default:
		return false, s.setHytaleOp(uuid, action == "op")
	}
}

var reUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// hytaleRemoveByID takes a player the panel only knows by account ID off a
// list (while the server is stopped: the server's commands take names).
func (s *Server) hytaleRemoveByID(uuid, action string) error {
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	s.mu.Lock()
	up := s.cmd != nil
	s.mu.Unlock()
	if up {
		return errors.New("the server doesn't know this player's name; stop it to take them off")
	}
	if action == "deop" {
		if !fileExists(s.permissionsFile()) {
			return nil
		}
		return s.setHytaleOp(uuid, false)
	}
	w, raw, err := s.readHytaleWhitelist()
	if err != nil {
		return err
	}
	kept := w.List[:0]
	for _, id := range w.List {
		if id != uuid {
			kept = append(kept, id)
		}
	}
	w.List = kept
	return s.writeHytaleWhitelist(w, raw)
}

// setHytaleAllowlist turns the allowlist on or off.
func (s *Server) setHytaleAllowlist(on bool) error {
	action := "allowlist-off"
	if on {
		action = "allowlist-on"
	}
	// Under filesMu throughout, like SetOutside, so the server can't be
	// opened to friends outside in between.
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	s.mu.Lock()
	if !on && s.meta.Outside {
		s.mu.Unlock()
		return ErrOutsideOpen
	}
	s.mu.Unlock()
	s.dropJavaPending("", "allowlist-on")
	if s.javaCommandIfRunning(hytaleCommandFor("", action)) {
		return nil
	}
	s.mu.Lock()
	if s.cmd != nil || !fileExists(s.whitelistFile()) {
		s.meta.JavaPending = append(s.meta.JavaPending, JavaPending{Action: action})
		s.mu.Unlock()
		return s.saveMeta()
	}
	s.mu.Unlock()
	w, raw, err := s.readHytaleWhitelist()
	if err != nil {
		return err
	}
	w.Enabled = on
	return s.writeHytaleWhitelist(w, raw)
}

// applyHytalePendingLocked sends the queued changes once the server is up.
// Operator changes for players it hasn't seen wait until they join (the
// server only knows players who have played). Caller holds s.mu.
func (s *Server) applyHytalePendingLocked() {
	if len(s.meta.JavaPending) == 0 {
		return
	}
	var kept []JavaPending
	for _, p := range s.meta.JavaPending {
		cmd := hytaleCommandFor(p.Name, p.Action)
		if cmd == "" {
			continue
		}
		if p.Action == "op" && s.hytaleUUIDLocked(p.Name) == "" {
			kept = append(kept, p)
			continue
		}
		s.addLineLocked("> " + cmd)
		s.sendLocked(cmd)
	}
	s.meta.JavaPending = kept
	s.saveMetaLater()
}

// applyHytaleOpOnJoinLocked makes a player who just joined an operator if
// that was waiting for them. Caller holds s.mu.
func (s *Server) applyHytaleOpOnJoinLocked(name string) {
	kept := s.meta.JavaPending[:0]
	changed := false
	for _, p := range s.meta.JavaPending {
		if p.Action == "op" && strings.EqualFold(p.Name, name) {
			cmd := hytaleCommandFor(name, "op")
			s.addLineLocked("> " + cmd)
			s.sendLocked(cmd)
			changed = true
			continue
		}
		kept = append(kept, p)
	}
	s.meta.JavaPending = kept
	if changed {
		s.saveMetaLater()
	}
}

// hytaleAllowlistOn reads whitelist.json's "enabled". Caller holds filesMu.
func (s *Server) hytaleAllowlistOn() (bool, error) {
	w, _, err := s.readHytaleWhitelist()
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.meta.JavaPending {
		switch p.Action {
		case "allowlist-on":
			w.Enabled = true
		case "allowlist-off":
			w.Enabled = false
		}
	}
	return w.Enabled, nil
}

// ---- settings (config.json) ----

var hytaleSchema = []schemaGroup{
	{"Server", []fieldDef{
		{Field: Field{Key: "ServerName", Label: "Server name", Type: "text", Max: 64,
			Help: "Shown to players in Hytale's server list."}, def: "Hytale Server",
			validate: func(v string) error { return textRule(v, "") }},
		{Field: Field{Key: "MOTD", Label: "Message of the day", Type: "text", Max: 128,
			Help: "Shown to players when they join."}, def: "",
			validate: func(v string) error {
				if v == "" {
					return nil
				}
				return textRule(v, "")
			}},
		{Field: Field{Key: "Password", Label: "Server password", Type: "text", Max: 64,
			Help: "Players type this to join. Leave it empty for none (the allowlist still applies)."}, def: "",
			validate: func(v string) error {
				if v == "" {
					return nil
				}
				return textRule(v, "")
			}},
		{Field: Field{Key: "MaxPlayers", Label: "Maximum players", Type: "int", Min: 1, Max: 200,
			Help: "How many people can play at once."}, def: "100"},
	}},
	{"Game", []fieldDef{
		{Field: Field{Key: "Defaults.GameMode", Label: "Game mode", Type: "select",
			Options: opts("Adventure", "Adventure", "Creative", "Creative"),
			Help:    "The game mode players get when they join."}, def: "Adventure"},
		{Field: Field{Key: "MaxViewRadius", Label: "View distance (chunks)", Type: "int", Min: 4, Max: 32,
			Help: "How far players can see. Hytale suggests 12 at most; higher uses much more memory."}, def: "32"},
	}},
	{"Set by the panel", []fieldDef{
		{Field: Field{Key: "port", Label: "Port", Type: "readonly",
			Help: "Given out by the panel so servers never clash (UDP)."}, managed: true},
	}},
}

func (s *Server) hytaleConfigPath() string { return filepath.Join(s.dir(), "config.json") }

// jsonPath reads a dotted key ("Defaults.GameMode") from a JSON object.
func jsonPath(v map[string]any, key string) (any, bool) {
	parts := strings.Split(key, ".")
	cur := any(v)
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func setJSONPath(v map[string]any, key string, val any) {
	parts := strings.Split(key, ".")
	cur := v
	for _, p := range parts[:len(parts)-1] {
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
	cur[parts[len(parts)-1]] = val
}

func jsonString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case nil:
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// hytaleSettings is the Settings tab of a Hytale server.
func (s *Server) hytaleSettings() (SettingsView, error) {
	s.filesMu.Lock()
	cfg, err := readJSONObject(s.hytaleConfigPath())
	s.filesMu.Unlock()
	if err != nil {
		return SettingsView{}, err
	}
	st := s.Status()
	v := SettingsView{Name: st.Name, Running: st.State == StateRunning || st.State == StateStarting}
	for _, g := range hytaleSchema {
		group := Group{Title: g.title}
		for _, f := range g.fields {
			field := f.Field
			if f.Key == "port" {
				field.Value, field.InFile = strconv.Itoa(st.Port), true
			} else if val, ok := jsonPath(cfg, f.Key); ok {
				field.Value, field.InFile = jsonString(val), true
			} else {
				field.Value = f.def
			}
			group.Fields = append(group.Fields, field)
		}
		v.Groups = append(v.Groups, group)
	}
	return v, nil
}

var errHytaleRunning = errors.New("Hytale saves its settings while it runs, so stop the server to change them")

// saveHytaleSettings writes changed settings to config.json (only while the
// server is stopped). Unknown settings in the file are kept.
func (s *Server) saveHytaleSettings(values map[string]string) ([]SettingsChange, error) {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		f, ok := findIn(hytaleSchema, k)
		if !ok || f.managed || f.Type == "readonly" {
			return nil, fmt.Errorf("%q can't be changed on the Settings tab", k)
		}
		switch f.Type {
		case "text":
			values[k] = strings.TrimSpace(values[k])
		case "int":
			if n, err := strconv.Atoi(strings.TrimSpace(values[k])); err == nil {
				values[k] = strconv.Itoa(n)
			}
		}
		if err := f.check(values[k]); err != nil {
			return nil, fmt.Errorf("%s %s", f.Label, err)
		}
	}
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	s.mu.Lock()
	up := s.cmd != nil
	s.mu.Unlock()
	if up {
		return nil, errHytaleRunning
	}
	cfg, err := readJSONObject(s.hytaleConfigPath())
	if err != nil {
		return nil, err
	}
	var changes []SettingsChange
	for _, k := range keys {
		f, _ := findIn(hytaleSchema, k)
		old := f.def
		if v, ok := jsonPath(cfg, k); ok {
			old = jsonString(v)
		}
		if old == values[k] {
			continue
		}
		var val any = values[k]
		if f.Type == "int" {
			n, _ := strconv.Atoi(values[k])
			val = n
		}
		setJSONPath(cfg, k, val)
		changes = append(changes, SettingsChange{Key: k, Label: f.Label, From: old, To: values[k]})
	}
	if len(changes) == 0 {
		return nil, nil
	}
	if err := writeJSONObject(s.hytaleConfigPath(), cfg); err != nil {
		return nil, fmt.Errorf("couldn't save config.json: %w", err)
	}
	for _, c := range changes {
		s.say(fmt.Sprintf("Setting changed: %s: %s → %s", c.Label, c.From, c.To))
	}
	return changes, nil
}

// saveHytaleRawConfig replaces config.json from the advanced editor (only
// while the server is stopped).
func (s *Server) saveHytaleRawConfig(text string) error {
	var v map[string]any
	if err := json.Unmarshal([]byte(text), &v); err != nil || v == nil {
		return fmt.Errorf("that isn't valid JSON: %v", err)
	}
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	s.mu.Lock()
	up := s.cmd != nil
	s.mu.Unlock()
	if up {
		return errHytaleRunning
	}
	if err := os.WriteFile(s.hytaleConfigPath()+".tmp", []byte(text), 0o644); err != nil {
		return err
	}
	if err := os.Rename(s.hytaleConfigPath()+".tmp", s.hytaleConfigPath()); err != nil {
		return err
	}
	s.say("config.json was edited in the advanced editor.")
	return nil
}

// ---- backups while running ----

// hytaleHoldSave asks a running Hytale server to save its worlds before
// they're copied. Hytale can't pause saving, so this is a moment's flush;
// a backup made while nobody plays is the most reliable.
func (s *Server) hytaleHoldSave(ctx context.Context, held *exec.Cmd) error {
	s.mu.Lock()
	ok := s.cmd != nil && s.cmd == held && s.sendLocked("/world save --all")
	s.mu.Unlock()
	if !ok {
		return errors.New("the server isn't taking commands")
	}
	select {
	case <-time.After(hytaleSaveWait):
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// hytaleSaveWait is how long a save is given before copying; tests shorten
// it.
var hytaleSaveWait = 5 * time.Second
