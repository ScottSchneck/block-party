// Package offsite copies the panel's backups somewhere else: another disk or
// share, or a cloud service (Google Drive, Dropbox, OneDrive, iCloud Drive,
// Backblaze B2 or S3-compatible storage), using rclone.
//
// Backups live in <data>/backups/<server id>/. Off-site they keep that
// layout under a folder of the owner's choosing ("Block Party"), with a
// server.json beside them describing the server, so a panel that lost its
// data can bring its servers back from the off-site copies alone.
//
// Each pass lists what's off-site, copies up the newest backups of each
// server that aren't there yet (up to Keep), and removes older ones beyond
// Keep. Passes run after each new backup, every few hours, and when asked.
// Credentials are kept in <data>/offsite/rclone.conf (only the panel can
// read it); the choices in offsite.json.
package offsite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kinds of destination.
const (
	KindFolder   = "folder"   // another share or disk, mounted into the container
	KindDrive    = "drive"    // Google Drive
	KindDropbox  = "dropbox"  // Dropbox
	KindOneDrive = "onedrive" // Microsoft OneDrive
	KindICloud   = "icloud"   // iCloud Drive
	KindB2       = "b2"       // Backblaze B2
	KindS3       = "s3"       // Amazon S3 or compatible (Cloudflare R2, Wasabi, MinIO…)
)

// Kinds lists the destinations, in the order the page offers them.
var Kinds = []KindInfo{
	{KindDrive, "Google Drive", "oauth"},
	{KindICloud, "iCloud Drive", "icloud"},
	{KindOneDrive, "OneDrive", "oauth"},
	{KindDropbox, "Dropbox", "oauth"},
	{KindB2, "Backblaze B2", "keys"},
	{KindS3, "Amazon S3 or compatible", "keys"},
	{KindFolder, "Another disk or share", "folder"},
}

// KindInfo describes a kind of destination for the page.
type KindInfo struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	// How is how it's connected: oauth (sign in on the provider's site and
	// paste the address back), icloud (Apple ID, password and a code),
	// keys (an access key and a bucket) or folder (a path).
	How string `json:"how"`
}

func kindInfo(kind string) (KindInfo, bool) {
	for _, k := range Kinds {
		if k.Kind == kind {
			return k, true
		}
	}
	return KindInfo{}, false
}

// rcloneType is rclone's name for each kind.
var rcloneType = map[string]string{
	KindFolder: "local", KindDrive: "drive", KindDropbox: "dropbox", KindOneDrive: "onedrive",
	KindICloud: "iclouddrive", KindB2: "b2", KindS3: "s3",
}

// Settings are the owner's choices.
type Settings struct {
	On   bool   `json:"on"`
	Kind string `json:"kind,omitempty"`
	// Folder is the folder backups go in at the destination.
	Folder string `json:"folder"`
	// Path is the folder's location for KindFolder (inside the container,
	// such as /offsite), and the bucket for B2 and S3.
	Path string `json:"path,omitempty"`
	// Keep is how many backups of each server are kept off-site.
	Keep int `json:"keep"`
	// Encrypt: backups are encrypted with the owner's passphrase before
	// they leave (names included).
	Encrypt bool `json:"encrypt,omitempty"`
	// Account is who it's connected as, when known (an Apple ID, a key ID).
	Account   string    `json:"account,omitempty"`
	Connected time.Time `json:"connected,omitempty"`
}

// DefaultFolder and DefaultKeep are used until the owner chooses.
const (
	DefaultFolder = "Block Party"
	DefaultKeep   = 3
	maxKeep       = 50
)

// LocalBackup is one of the panel's backups.
type LocalBackup struct {
	Server string // server ID
	Name   string // file name
	Path   string
	Time   time.Time
	Size   int64
}

// Options connects the manager to the panel.
type Options struct {
	DataDir string
	// Rclone is the rclone program ("rclone" by default).
	Rclone string
	// Backups lists the panel's backups, newest first within each server.
	Backups func() []LocalBackup
	// Note is a server's description (server.json), or nil if unknown.
	Note func(server string) []byte
	// Live reports whether a server with that ID is in the panel.
	Live func(server string) bool
	// Failed is told when copying fails (once, until it works again).
	Failed func(title, text string)
	Log    *slog.Logger
	// Every is how often a pass runs without a new backup; RetryAfter how
	// soon one runs again after a failure.
	Every      time.Duration
	RetryAfter time.Duration
}

// Manager keeps the off-site copies.
type Manager struct {
	opts Options
	dir  string
	log  *slog.Logger

	mu       sync.Mutex
	settings Settings
	status   Status
	failed   bool // told about a failure, not yet about it working again

	passMu sync.Mutex // one pass (or connect, or download) at a time
	kick   chan struct{}

	authMu sync.Mutex
	auth   *authFlow
	ask    *question // an iCloud question waiting for an answer

	pendMu sync.Mutex
	pend   *pending // the connection being set up

	versionOnce sync.Once
	version     string
}

// Status is how the copies are doing.
type Status struct {
	Running   bool      `json:"running"`
	Copying   string    `json:"copying,omitempty"` // the file being copied now
	LastPass  time.Time `json:"lastPass,omitempty"`
	LastOK    time.Time `json:"lastOK,omitempty"`
	LastError string    `json:"lastError,omitempty"`
	// Copied and Removed are counts from the last pass.
	Copied  int `json:"copied"`
	Removed int `json:"removed"`
}

// New returns a Manager. Call Run to keep copies up to date.
func New(opts Options) *Manager {
	if opts.Rclone == "" {
		opts.Rclone = "rclone"
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Every == 0 {
		opts.Every = 6 * time.Hour
	}
	if opts.RetryAfter == 0 {
		opts.RetryAfter = 15 * time.Minute
	}
	m := &Manager{opts: opts, dir: filepath.Join(opts.DataDir, "offsite"), log: opts.Log.With("src", "offsite"), kick: make(chan struct{}, 1)}
	m.settings = Settings{Folder: DefaultFolder, Keep: DefaultKeep}
	if b, err := os.ReadFile(filepath.Join(m.dir, "offsite.json")); err == nil {
		_ = json.Unmarshal(b, &m.settings)
	}
	if m.settings.Keep <= 0 {
		m.settings.Keep = DefaultKeep
	}
	return m
}

func (m *Manager) confPath() string { return filepath.Join(m.dir, "rclone.conf") }

// Settings returns the owner's choices.
func (m *Manager) Settings() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings
}

// Status returns how the copies are doing.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *Manager) saveSettings(s Settings) error {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := filepath.Join(m.dir, "offsite.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(m.dir, "offsite.json")); err != nil {
		return err
	}
	m.mu.Lock()
	m.settings = s
	m.mu.Unlock()
	return nil
}

// ---- rclone ----

var errNoRclone = errors.New("rclone isn't installed in this container (update Block Party)")

// rclone runs rclone with the panel's config file and returns its output.
// An error carries rclone's own message.
func (m *Manager) rclone(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	return m.rcloneConf(ctx, m.confPath(), stdin, args...)
}

// rcloneConf is rclone with another config file (a connection being set up).
func (m *Manager) rcloneConf(ctx context.Context, conf string, stdin []byte, args ...string) ([]byte, error) {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return nil, err
	}
	full := append([]string{"--config", conf, "--ask-password=false"}, args...)
	cmd := exec.CommandContext(ctx, m.opts.Rclone, full...)
	cmd.Env = append(os.Environ(), "RCLONE_CONFIG="+conf)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if errors.Is(err, exec.ErrNotFound) {
		return nil, errNoRclone
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return out.Bytes(), errors.New(rcloneError(errb.String(), err))
	}
	return out.Bytes(), nil
}

var reLogPrefix = regexp.MustCompile(`^\d{4}/\d\d/\d\d \d\d:\d\d:\d\d (?:ERROR|NOTICE|CRITICAL|Failed to [^:]*:)\s*:?\s*`)

// rcloneError picks the most useful line from rclone's errors.
func rcloneError(stderr string, err error) string {
	var best string
	for _, l := range strings.Split(strings.TrimSpace(stderr), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.Contains(l, "DEBUG") || strings.HasPrefix(l, "Usage:") {
			continue
		}
		best = reLogPrefix.ReplaceAllString(l, "")
	}
	if best == "" {
		return "rclone: " + err.Error()
	}
	if len(best) > 300 {
		best = best[:300] + "…"
	}
	return best
}

// base is the destination's top folder, as rclone names it.
func (s Settings) base() string {
	folder := strings.Trim(s.Folder, "/")
	switch s.Kind {
	case KindFolder, KindB2, KindS3:
		return "offsite:" + path.Join(strings.TrimSuffix(s.Path, "/"), folder)
	}
	return "offsite:" + folder
}

// target is where backups go: the top folder, or its encrypted view.
func (s Settings) target() string {
	if s.Encrypt {
		return "offsite-crypt:"
	}
	return s.base()
}

func join(target, p string) string {
	if strings.HasSuffix(target, ":") {
		return target + p
	}
	return target + "/" + p
}

// ---- passes ----

// Run keeps the copies up to date until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	wait := time.Minute // let the panel settle after starting
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-m.kick:
		}
		wait = m.opts.Every
		if !m.Settings().On {
			continue
		}
		if err := m.Pass(ctx); err != nil && ctx.Err() == nil {
			wait = m.opts.RetryAfter
		}
	}
}

// Soon asks for a pass now (after a new backup, or from the page).
func (m *Manager) Soon() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// remoteFile is a file off-site.
type remoteFile struct {
	Path    string    `json:"Path"`
	Size    int64     `json:"Size"`
	ModTime time.Time `json:"ModTime"`
	IsDir   bool      `json:"IsDir"`
}

func (m *Manager) list(ctx context.Context, s Settings) ([]remoteFile, error) {
	out, err := m.rclone(ctx, nil, "lsjson", s.target(), "--recursive", "--files-only", "--max-depth", "2", "--no-mimetype")
	if err != nil {
		if strings.Contains(err.Error(), "directory not found") {
			return nil, nil // nothing copied yet
		}
		return nil, err
	}
	var files []remoteFile
	if err := json.Unmarshal(out, &files); err != nil {
		return nil, fmt.Errorf("couldn't read rclone's list: %w", err)
	}
	return files, nil
}

// reBackup matches a backup's file name and finds its time.
var reBackup = regexp.MustCompile(`^(.+)-(\d{8}-\d{6})-[a-z-]+\.tar\.gz$`)

// Pass copies up what's missing and removes what's beyond Keep.
func (m *Manager) Pass(ctx context.Context) (err error) {
	m.passMu.Lock()
	defer m.passMu.Unlock()
	s := m.Settings()
	if !s.On {
		return nil
	}
	m.setStatus(func(st *Status) { st.Running, st.Copying = true, "" })
	copied, removed := 0, 0
	defer func() {
		m.setStatus(func(st *Status) {
			st.Running, st.Copying, st.LastPass = false, "", time.Now()
			st.Copied, st.Removed = copied, removed
			if err != nil {
				st.LastError = err.Error()
			} else {
				st.LastError, st.LastOK = "", time.Now()
			}
		})
		m.noteResult(err)
	}()

	files, err := m.list(ctx, s)
	if err != nil {
		return err
	}
	remote := map[string]map[string]remoteFile{} // server -> file name -> file
	for _, f := range files {
		dir, name := path.Split(f.Path)
		dir = strings.Trim(dir, "/")
		if dir == "" || strings.Contains(dir, "/") {
			continue
		}
		if remote[dir] == nil {
			remote[dir] = map[string]remoteFile{}
		}
		remote[dir][name] = f
	}

	// Newest Keep backups of each server, oldest of those first, so an
	// interrupted pass still leaves the newest copies first time round.
	byServer := map[string][]LocalBackup{}
	if m.opts.Backups != nil {
		for _, b := range m.opts.Backups() {
			byServer[b.Server] = append(byServer[b.Server], b)
		}
	}
	servers := make([]string, 0, len(byServer))
	for id := range byServer {
		servers = append(servers, id)
	}
	sort.Strings(servers)
	for _, id := range servers {
		list := byServer[id]
		sort.Slice(list, func(i, j int) bool { return list[i].Time.After(list[j].Time) })
		if len(list) > s.Keep {
			list = list[:s.Keep]
		}
		newFiles := false
		for i := len(list) - 1; i >= 0; i-- {
			b := list[i]
			if f, ok := remote[id][b.Name]; ok && f.Size == b.Size {
				continue
			}
			m.setStatus(func(st *Status) { st.Copying = b.Name })
			m.log.Info("copying a backup off-site", "server", id, "file", b.Name)
			if _, err := m.rclone(ctx, nil, "copyto", b.Path, join(s.target(), id+"/"+b.Name)); err != nil {
				return fmt.Errorf("couldn't copy %s: %w", b.Name, err)
			}
			copied++
			newFiles = true
			if remote[id] == nil {
				remote[id] = map[string]remoteFile{}
			}
			remote[id][b.Name] = remoteFile{Path: id + "/" + b.Name, Size: b.Size, ModTime: b.Time}
		}
		// The server's description, so it can be brought back from here.
		if _, ok := remote[id]["server.json"]; (newFiles || !ok) && m.opts.Note != nil {
			if note := m.opts.Note(id); note != nil {
				if _, err := m.rclone(ctx, note, "rcat", join(s.target(), id+"/server.json")); err != nil {
					return fmt.Errorf("couldn't copy %s's details: %w", id, err)
				}
			}
		}
	}

	// Remove what's beyond Keep, oldest first.
	for id, fs := range remote {
		var names []string
		for name := range fs {
			if reBackup.MatchString(name) {
				names = append(names, name)
			}
		}
		sort.Slice(names, func(i, j int) bool { return backupTime(names[i]) > backupTime(names[j]) })
		for _, name := range names[min(len(names), s.Keep):] {
			m.log.Info("removing an older off-site backup", "server", id, "file", name)
			if _, err := m.rclone(ctx, nil, "deletefile", join(s.target(), id+"/"+name)); err != nil {
				return fmt.Errorf("couldn't remove the older copy %s: %w", name, err)
			}
			removed++
		}
	}
	return nil
}

func backupTime(name string) string {
	if mm := reBackup.FindStringSubmatch(name); mm != nil {
		return mm[2]
	}
	return ""
}

func (m *Manager) setStatus(f func(*Status)) {
	m.mu.Lock()
	f(&m.status)
	m.mu.Unlock()
}

// noteResult tells the owner about a failure once, and that it works again.
func (m *Manager) noteResult(err error) {
	m.mu.Lock()
	first := err != nil && !m.failed
	m.failed = err != nil
	m.mu.Unlock()
	if err != nil {
		m.log.Warn("off-site copy failed", "error", err)
	}
	if first && m.opts.Failed != nil {
		m.opts.Failed("Off-site backup copy failed", err.Error()+" The panel tries again every 15 minutes and won't tell you again until it works.")
	}
}

// ---- what's off-site ----

// Copy is a backup off-site.
type Copy struct {
	Server string    `json:"server"`
	Name   string    `json:"name"`
	Size   int64     `json:"size"`
	Time   time.Time `json:"time"`
	Local  bool      `json:"local"` // the panel has it too
}

// ServerCopies are one server's copies off-site.
type ServerCopies struct {
	Server string `json:"server"`
	Name   string `json:"name"` // from its server.json, when there
	Copies []Copy `json:"copies"`
	Live   bool   `json:"live"`
}

// List returns what's off-site, by server.
func (m *Manager) List(ctx context.Context) ([]ServerCopies, error) {
	s := m.Settings()
	if s.Kind == "" {
		return nil, errors.New("off-site backups aren't set up")
	}
	m.passMu.Lock()
	files, err := m.list(ctx, s)
	m.passMu.Unlock()
	if err != nil {
		return nil, err
	}
	local := map[string]bool{}
	if m.opts.Backups != nil {
		for _, b := range m.opts.Backups() {
			local[b.Server+"/"+b.Name] = true
		}
	}
	byID := map[string]*ServerCopies{}
	notes := map[string]bool{}
	for _, f := range files {
		dir, name := path.Split(f.Path)
		dir = strings.Trim(dir, "/")
		if dir == "" || strings.Contains(dir, "/") {
			continue
		}
		sc := byID[dir]
		if sc == nil {
			sc = &ServerCopies{Server: dir, Name: dir, Copies: []Copy{}}
			if m.opts.Live != nil {
				sc.Live = m.opts.Live(dir)
			}
			byID[dir] = sc
		}
		if name == "server.json" {
			notes[dir] = true
			continue
		}
		if !reBackup.MatchString(name) {
			continue
		}
		t := f.ModTime
		if bt, err := time.ParseInLocation("20060102-150405", backupTime(name), time.Local); err == nil {
			t = bt
		}
		sc.Copies = append(sc.Copies, Copy{Server: dir, Name: name, Size: f.Size, Time: t, Local: local[dir+"/"+name]})
	}
	out := []ServerCopies{}
	for id, sc := range byID {
		if notes[id] {
			if b, err := m.rcloneLocked(ctx, "cat", join(s.target(), id+"/server.json")); err == nil {
				var n struct {
					Meta struct {
						Name string `json:"name"`
					} `json:"meta"`
				}
				if json.Unmarshal(b, &n) == nil && n.Meta.Name != "" {
					sc.Name = n.Meta.Name
				}
			}
		}
		sort.Slice(sc.Copies, func(i, j int) bool { return sc.Copies[i].Time.After(sc.Copies[j].Time) })
		if len(sc.Copies) > 0 {
			out = append(out, *sc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

func (m *Manager) rcloneLocked(ctx context.Context, args ...string) ([]byte, error) {
	m.passMu.Lock()
	defer m.passMu.Unlock()
	return m.rclone(ctx, nil, args...)
}

// ErrNotACopy is returned for a name that can't be an off-site backup.
var ErrNotACopy = errors.New("that isn't one of the off-site backups")

var reSafeID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Download copies a backup back into the panel's backups. A server that
// isn't in the panel any more gets a note from its server.json, so it can be
// brought back from the Backups page.
func (m *Manager) Download(ctx context.Context, server, name string) error {
	if !reSafeID.MatchString(server) || filepath.Base(name) != name || !reBackup.MatchString(name) {
		return ErrNotACopy
	}
	if mm := reBackup.FindStringSubmatch(name); mm[1] != server {
		return ErrNotACopy
	}
	s := m.Settings()
	if s.Kind == "" {
		return errors.New("off-site backups aren't set up")
	}
	m.passMu.Lock()
	defer m.passMu.Unlock()
	dir := filepath.Join(m.opts.DataDir, "backups", server)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(dir, name)
	if _, err := os.Stat(dest); err == nil {
		return errors.New("the panel already has that backup")
	}
	partial := dest + ".download"
	if _, err := m.rclone(ctx, nil, "copyto", join(s.target(), server+"/"+name), partial); err != nil {
		_ = os.Remove(partial)
		return fmt.Errorf("couldn't download it: %w", err)
	}
	if err := os.Rename(partial, dest); err != nil {
		return err
	}
	if m.opts.Live != nil && m.opts.Live(server) {
		return nil
	}
	// A server that isn't here: write (or point) its note at this backup.
	notePath := filepath.Join(dir, "deleted-server.json")
	var note map[string]any
	if b, err := os.ReadFile(notePath); err == nil {
		_ = json.Unmarshal(b, &note)
	}
	if note == nil {
		b, err := m.rclone(ctx, nil, "cat", join(s.target(), server+"/server.json"))
		if err != nil || json.Unmarshal(b, &note) != nil {
			return nil // it's downloaded; without details it can't be brought back
		}
		note["deleted"] = time.Now().UTC()
	}
	note["final"] = name
	b, _ := json.MarshalIndent(note, "", "  ")
	return os.WriteFile(notePath, b, 0o644)
}
