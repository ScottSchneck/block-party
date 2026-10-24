// Package servers installs, runs and updates game servers. Phase 1 supports
// the official Minecraft Bedrock Dedicated Server.
//
// Each server lives in its own folder under <data>/servers/<id>, with the
// panel's details about it in .blockheads.json in that folder. Backups go to
// <data>/backups/<id>.
package servers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ScottSchneck/block-party/internal/notify"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Options configures a Manager.
type Options struct {
	DataDir     string        // usually /data
	DownloadAPI string        // Mojang's download list; DefaultDownloadAPI if empty
	HTTPClient  *http.Client  // for downloads; a default if nil
	Version     string        // panel version, for the download User-Agent
	StopTimeout time.Duration // how long a server gets to save and stop; 30s if zero
	Log         *slog.Logger
	ImportDir   string // other panels' server folders, mounted read-only; "" if none
	// Where Java servers come from (defaults if empty; tests change them).
	PaperAPI       string
	AdoptiumAPI    string
	MojangVersions string
	MojangProfiles string
	// Where console support for Java servers comes from.
	GeyserAPI  string
	HangarAPI  string
	XboxLookup string
	// Where Java plugins come from (Hangar is HangarAPI).
	ModrinthAPI string
	// Hytale's sign-in, account and session services.
	HytaleOAuth    string
	HytaleAccounts string
	HytaleSessions string
	// Notify, when set, is told about things the owner may want to hear
	// about (crashes, join attempts, failed backups…). It must not block.
	Notify func(notify.Event)
}

// Manager keeps track of all servers.
type Manager struct {
	opts Options
	log  *slog.Logger

	// ctx is cancelled when the panel shuts down; installs and updates in
	// progress stop, and no server starts. work counts them.
	ctx    context.Context
	cancel context.CancelFunc
	workMu sync.Mutex // orders beginWork against StopAll's cancel
	work   sync.WaitGroup
	saves  sync.WaitGroup // background file writes (server details, joins); StopAll waits for them

	mu      sync.Mutex
	servers map[string]*Server

	people     *people
	settingsMu sync.Mutex
	imports    importState
	latest     versionCache
	paper      paperCache
	geyserMC   geyserVersionCache
	plugins    pluginCache
	hostAddrs  func() HostAddresses
	// pluginUpdates: the newest release of each Java plugin, for update notes.
	pluginUpdates pluginUpdateCache
	hytale        hytaleAuth
	hytaleLatest  hytaleCache
	javaMu        sync.Mutex // one Java runtime install at a time
	history       *playHistory
	playtime      *playtime
}

// Meta is what the panel remembers about a server between restarts.
type Meta struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Type    string    `json:"type"` // "bedrock"
	Version string    `json:"version,omitempty"`
	Preview bool      `json:"preview,omitempty"`
	Port    int       `json:"port"`
	PortV6  int       `json:"portV6"`
	Created time.Time `json:"created"`
	// AutoStart is true while the owner wants the server running: it's set
	// by Start and cleared by Stop, so servers that were running come back
	// after the container restarts.
	AutoStart bool `json:"autoStart"`
	// PendingOps are gamertags to make operators when they first join
	// (Bedrock needs their Xbox ID, which it only learns then).
	PendingOps []string `json:"pendingOps,omitempty"`
	// ImportedFrom is the folder (inside the import folder) the server was
	// copied from, if it was imported.
	ImportedFrom string `json:"importedFrom,omitempty"`
	// Backups is when the server is backed up; nil means DefaultBackupPlan.
	Backups *BackupPlan `json:"backups,omitempty"`
	// LastScheduledBackup is when a scheduled backup was last made or
	// skipped.
	LastScheduledBackup time.Time `json:"lastScheduledBackup,omitempty"`
	// Outside is true while the server is open to friends outside the
	// house (it also needs outside access on in the panel).
	Outside bool `json:"outside,omitempty"`
	// HoldVersion keeps the server on its Bedrock version: overnight
	// updates skip it.
	HoldVersion bool `json:"holdVersion,omitempty"`
	// Java servers: the Paper build, the Java version it runs on, and how
	// much memory it may use (MB; 0 means the default).
	Build     int `json:"build,omitempty"`
	JavaMajor int `json:"javaMajor,omitempty"`
	MemoryMB  int `json:"memoryMB,omitempty"`
	// JavaPending are allowlist and operator changes to make by command
	// the next time a Java server starts (its accounts couldn't be looked
	// up while it was stopped).
	JavaPending []JavaPending `json:"javaPending,omitempty"`
	// NoConsoles turns console players (Geyser and Floodgate) off on a
	// Java server; they're on by default. Plugins holds the SHA-256 and
	// version of each console plugin installed ("<sha256> 2.11.3 (build
	// 9)"), by key.
	NoConsoles bool              `json:"noConsoles,omitempty"`
	Plugins    map[string]string `json:"plugins,omitempty"`
	// HytaleNames are the Hytale players the server has seen (account ID →
	// name), since Hytale's lists keep account IDs.
	HytaleNames map[string]string `json:"hytaleNames,omitempty"`
	// LastNightly is the night (its date) overnight upkeep last acted on
	// the server, so a panel restart doesn't do it twice.
	LastNightly string `json:"lastNightly,omitempty"`
	// Hours are when the server is open (nil or off: always). OpenLate
	// keeps it open past them until then, for one night. ClosedForHours is
	// set while it's stopped for the night, to start again when it opens.
	Hours          *Hours    `json:"hours,omitempty"`
	OpenLate       time.Time `json:"openLate,omitempty"`
	ClosedForHours bool      `json:"closedForHours,omitempty"`
}

// The name is from before the panel was called Block Party; it stays so
// existing servers keep working.
const metaFile = ".blockheads.json"

// New returns a Manager. Call Load to pick up existing servers.
func New(opts Options) *Manager {
	if opts.DownloadAPI == "" {
		opts.DownloadAPI = DefaultDownloadAPI
	}
	if opts.StopTimeout == 0 {
		opts.StopTimeout = 30 * time.Second
	}
	for _, d := range []struct {
		v   *string
		def string
	}{
		{&opts.PaperAPI, DefaultPaperAPI}, {&opts.AdoptiumAPI, DefaultAdoptiumAPI},
		{&opts.MojangVersions, DefaultMojangVersion}, {&opts.MojangProfiles, DefaultMojangProfile},
		{&opts.GeyserAPI, DefaultGeyserAPI}, {&opts.HangarAPI, DefaultHangarAPI}, {&opts.XboxLookup, DefaultXboxLookup},
		{&opts.ModrinthAPI, DefaultModrinthAPI},
		{&opts.HytaleOAuth, DefaultHytaleOAuth}, {&opts.HytaleAccounts, DefaultHytaleAccounts}, {&opts.HytaleSessions, DefaultHytaleSessions},
	} {
		if *d.v == "" {
			*d.v = d.def
		}
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{opts: opts, log: opts.Log.With("src", "servers"), servers: map[string]*Server{}, ctx: ctx, cancel: cancel, people: newPeople(opts.DataDir),
		history: newPlayHistory(opts.DataDir)}
	m.playtime = newPlaytime(m)
	return m
}

func (m *Manager) serversDir() string { return filepath.Join(m.opts.DataDir, "servers") }

func (m *Manager) backupsDir(id string) string {
	return filepath.Join(m.opts.DataDir, "backups", id)
}

// Load reads the servers already set up in the data folder.
func (m *Manager) Load() error {
	if err := os.MkdirAll(m.serversDir(), 0o755); err != nil {
		return fmt.Errorf("can't create %s: %w", m.serversDir(), err)
	}
	entries, err := os.ReadDir(m.serversDir())
	if err != nil {
		return err
	}
	// Java downloads that were cut short.
	if left, _ := filepath.Glob(filepath.Join(m.runtimesDir(), ".*")); len(left) > 0 {
		for _, l := range left {
			_ = os.RemoveAll(l)
		}
	}
	// A restore that was interrupted: put back what was moved aside.
	for _, e := range entries {
		if id, ok := strings.CutPrefix(e.Name(), ".restore-old-"); ok {
			if recoverRestore(filepath.Join(m.serversDir(), e.Name()), filepath.Join(m.serversDir(), id)) {
				m.log.Warn("a restore was interrupted, so it was undone; the server is as it was before", "id", id)
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".restore-") {
			_ = os.RemoveAll(filepath.Join(m.serversDir(), e.Name()))
			continue
		}
		if strings.HasPrefix(e.Name(), ".staging-") || strings.HasPrefix(e.Name(), ".download-") || strings.HasPrefix(e.Name(), ".import-") || strings.HasPrefix(e.Name(), ".upload-") || strings.HasPrefix(e.Name(), ".deleted-") {
			// Left over from an install that was interrupted.
			_ = os.RemoveAll(filepath.Join(m.serversDir(), e.Name()))
			continue
		}
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(m.serversDir(), e.Name(), metaFile))
		if err != nil {
			continue // not one of ours
		}
		var meta Meta
		if err := json.Unmarshal(b, &meta); err != nil || meta.ID != e.Name() {
			m.log.Warn("skipping a server folder with unreadable details", "folder", e.Name(), "error", err)
			continue
		}
		s := newServer(m, meta)
		if _, err := os.Stat(s.binary()); err != nil {
			s.state = StateError
			s.message = "The server files are missing. Use Update to download them again."
		} else if b, err := os.ReadFile(s.incompleteMarker()); err == nil {
			s.state = StateError
			s.message = strings.TrimSpace(string(b))
		}
		// World downloads cut short by a restart.
		if left, _ := filepath.Glob(filepath.Join(m.backupsDir(meta.ID), ".export-*")); len(left) > 0 {
			for _, l := range left {
				_ = os.RemoveAll(l)
			}
		}
		m.servers[meta.ID] = s
		m.log.Info("server loaded", "id", meta.ID, "name", meta.Name, "version", meta.Version, "port", meta.Port)
	}
	return nil
}

// StartAutoStart starts the servers that were running when the panel last
// stopped.
func (m *Manager) StartAutoStart() {
	for _, s := range m.all() {
		s.mu.Lock()
		want := s.meta.AutoStart && s.state == StateStopped // not after an error, such as a half-applied update
		if st := s.hoursStatusLocked(time.Now()); want && st != nil && !st.Open {
			// Outside its hours: it starts when it opens.
			want = false
			if !s.meta.ClosedForHours {
				s.meta.ClosedForHours = true
				s.saveMetaLater()
			}
		}
		s.mu.Unlock()
		if want && s.isHytale() {
			// It asks Hytale for a session first: don't hold up the others.
			if m.beginWork() {
				go func() {
					defer m.work.Done()
					if err := s.Start(); err != nil {
						m.log.Warn("could not start a server", "id", s.ID(), "error", err)
					}
				}()
			}
			continue
		}
		if want {
			if err := s.Start(); err != nil {
				m.log.Warn("could not start a server", "id", s.ID(), "error", err)
			}
		}
	}
}

// StopAll stops every running server and waits for them, up to the stop
// timeout plus a little. It's used when the container is shutting down, so it
// doesn't change which servers start next time. Installs and updates in
// progress are cancelled; an update that hasn't started copying files leaves
// the server as it was.
func (m *Manager) StopAll() {
	m.workMu.Lock()
	m.cancel() // no new installs, updates or starts from here on
	m.workMu.Unlock()
	deadline := time.After(m.opts.StopTimeout + 15*time.Second)

	// Stop what's running now, and wait for installs and updates at the same
	// time: they notice the cancellation and stop at the next file.
	var wg sync.WaitGroup
	for _, s := range m.all() {
		if done := s.stop(false); done != nil {
			wg.Add(1)
			go func() { defer wg.Done(); <-done }()
		}
	}
	wg.Add(1)
	go func() { defer wg.Done(); m.work.Wait() }()
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-deadline:
		m.log.Warn("some servers or updates didn't finish in time")
	}
	// An update that was waiting for its server to stop may have been the
	// only thing between that server and the stop; make sure nothing is left.
	for _, s := range m.all() {
		if done := s.stop(false); done != nil {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
		}
	}
	// Servers that just stopped may still be writing their details.
	savesDone := make(chan struct{})
	go func() { m.saves.Wait(); close(savesDone) }()
	select {
	case <-savesDone:
	case <-time.After(5 * time.Second):
	}
}

// beginWork registers an install or update, unless the panel is shutting
// down. The caller must call m.work.Done when it finishes.
func (m *Manager) beginWork() bool {
	m.workMu.Lock()
	defer m.workMu.Unlock()
	if m.ctx.Err() != nil {
		return false
	}
	m.work.Add(1)
	return true
}

func (m *Manager) all() []*Server {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Server, 0, len(m.servers))
	names := map[*Server]string{}
	for _, s := range m.servers {
		out = append(out, s)
		names[s] = strings.ToLower(s.displayName())
	}
	sort.Slice(out, func(i, j int) bool { return names[out[i]] < names[out[j]] })
	return out
}

// List returns every server's status, sorted by name.
func (m *Manager) List() []Status {
	var out []Status
	for _, s := range m.all() {
		st := s.Status()
		if t := s.lastBackup(); !t.IsZero() {
			st.LastBackup = &t
		}
		out = append(out, st)
	}
	return out
}

// Get finds a server by ID.
func (m *Manager) Get(id string) (*Server, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.servers[id]
	return s, ok
}

// Joinable is one of the panel's servers, for the console menu.
type Joinable struct {
	Name    string
	Port    int
	Running bool
	// NetherNetPort is where consoles joining over NetherNet go, when it
	// isn't Port (a Java server's Geyser: see NetherNetPort).
	NetherNetPort int
}

// Joinable returns the panel's servers for the console menu. Only running
// ones are offered, but all are returned so the menu can hide entries from
// servers.json that the panel now runs (for example after an import).
func (m *Manager) Joinable() []Joinable {
	var out []Joinable
	for _, s := range m.all() {
		st := s.Status()
		running := st.State == StateRunning
		if st.Type == TypeHytale {
			continue // not a Minecraft server
		}
		if st.Type == TypeJava {
			if !st.Consoles {
				continue // Java players only
			}
			running = st.ConsolesLive
		}
		j := Joinable{Name: st.Name, Port: st.Port, Running: running}
		if st.Type == TypeJava {
			j.NetherNetPort = GeyserNetherNetPort(st.Port)
		}
		out = append(out, j)
	}
	return out
}

var errNameInvalid = errors.New("give the server a name of 1 to 40 characters")

// Create sets up a new Bedrock server: it downloads the current version and
// starts it. It returns straight away; follow progress in the console.
//
// The owner's gamertag (a panel setting) and any gamertags passed in extra
// are put on the new server's allowlist and made operators.
func (m *Manager) Create(name string, preview bool, extra ...string) (Status, error) {
	return m.create(TypeBedrock, name, preview, 0, extra)
}

// CreateJava sets up a new Java (Paper) server, downloading Paper and the
// Java it needs. names (Java usernames) are put on its allowlist and made
// operators.
func (m *Manager) CreateJava(name string, memoryMB int, names ...string) (Status, error) {
	return m.create(TypeJava, name, false, memoryMB, names)
}

// CreateHytale sets up a new Hytale server, downloading it (with the
// panel's Hytale sign-in) and the Java it needs.
func (m *Manager) CreateHytale(name string, memoryMB int) (Status, error) {
	if !m.HytaleSignedIn() {
		return Status{}, ErrHytaleSignIn
	}
	return m.create(TypeHytale, name, false, memoryMB, nil)
}

func (m *Manager) create(typ, name string, preview bool, memoryMB int, extra []string) (Status, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 40 || strings.ContainsAny(name, "\r\n\t") {
		return Status{}, errNameInvalid
	}
	if typ != TypeBedrock && memoryMB != 0 && (memoryMB < 512 || memoryMB > 32768) {
		return Status{}, errors.New("pick between 512 MB and 32 GB of memory")
	}
	var ops []string
	for _, gt := range extra {
		var err error
		if typ == TypeJava {
			gt, err = cleanJavaName(gt)
		} else {
			gt, err = cleanGamertag(gt)
		}
		if err != nil {
			return Status{}, err
		}
		ops = append(ops, gt)
	}
	m.mu.Lock()
	for _, s := range m.servers {
		if strings.EqualFold(s.displayName(), name) {
			m.mu.Unlock()
			return Status{}, fmt.Errorf("there's already a server called %q", name)
		}
	}
	id := m.newID(name)
	port, err := m.freePort()
	if err != nil {
		m.mu.Unlock()
		return Status{}, err
	}
	meta := Meta{ID: id, Name: name, Type: typ, Preview: preview, Port: port, PortV6: port + 1, Created: time.Now().UTC(), AutoStart: true, MemoryMB: memoryMB}
	s := newServer(m, meta)
	s.state = StateInstalling
	s.busy = true
	m.servers[id] = s
	m.mu.Unlock()

	if !m.beginWork() {
		m.remove(id)
		return Status{}, errShuttingDown
	}
	if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		m.work.Done()
		m.remove(id)
		return Status{}, err
	}
	if err := s.saveMeta(); err != nil {
		m.work.Done()
		m.remove(id)
		return Status{}, err
	}
	m.log.Info("creating a server", "id", id, "name", name, "port", port)
	go s.installAndStart(ops)
	return s.Status(), nil
}

func (m *Manager) remove(id string) {
	m.mu.Lock()
	delete(m.servers, id)
	m.mu.Unlock()
}

var slugChars = regexp.MustCompile(`[^a-z0-9]+`)

// newID makes a folder-safe ID from a name. Caller holds m.mu.
func (m *Manager) newID(name string) string {
	base := strings.Trim(slugChars.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(base) > 30 {
		base = strings.Trim(base[:30], "-")
	}
	if base == "" {
		base = "server"
	}
	id := base
	for i := 2; ; i++ {
		_, taken := m.servers[id]
		_, err := os.Stat(filepath.Join(m.serversDir(), id))
		// A deleted server's backups keep its name, so it can come back.
		_, berr := os.Stat(m.backupsDir(id))
		if !taken && os.IsNotExist(err) && os.IsNotExist(berr) {
			return id
		}
		id = fmt.Sprintf("%s-%d", base, i)
	}
}

// Game ports for panel servers: 19132 and 19133 belong to the console server
// list; each server takes a pair (IPv4, IPv6) up to 19199, the range the
// container image exposes. That's 33 servers.
const (
	firstPort = 19134
	lastPort  = 19198
)

// freePort picks the next free port pair. Caller holds m.mu.
func (m *Manager) freePort() (int, error) {
	used := map[int]bool{}
	for _, s := range m.servers {
		used[s.meta.Port] = true
		used[s.meta.PortV6] = true
	}
	for p := firstPort; p <= lastPort; p += 2 {
		if !used[p] && !used[p+1] {
			return p, nil
		}
	}
	return 0, errors.New("no free ports left: the panel runs up to 33 servers")
}

// BedrockUDPPorts is the UDP port range a Bedrock server uses for NetherNet
// gameplay (one port per player connected at once), so servers in the same
// container never compete for ports and each range can be forwarded for
// friends outside.
func BedrockUDPPorts(port int) (first, last int) {
	slot := (port - firstPort) / 2
	first = 49152 + slot*50
	return first, first + bedrockUDPPortCount - 1
}

// bedrockUDPPortCount is how many players can be connected to one Bedrock
// server at once over NetherNet.
const bedrockUDPPortCount = 20

// GeyserNetherNetPort is the port a Java server's Geyser takes NetherNet on,
// both TCP (signaling) and UDP (gameplay): the next one up, since the Java
// server itself listens on TCP on its own port (each server has two).
func GeyserNetherNetPort(port int) int { return port + 1 }

// HostAddresses are this container's addresses, for game servers that tell
// consoles where to connect: its address at home, and the home's internet
// address while friends outside can join ("" if not).
type HostAddresses struct {
	Home     string
	Internet string
}

// SetHostAddresses tells the manager where to find the container's
// addresses (set once at start, before servers run).
func (m *Manager) SetHostAddresses(f func() HostAddresses) { m.hostAddrs = f }

func (m *Manager) hostAddresses() HostAddresses {
	if m.hostAddrs == nil {
		return HostAddresses{}
	}
	return m.hostAddrs()
}
