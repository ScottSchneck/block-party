package servers

import (
	"github.com/ScottSchneck/block-party/internal/notify"

	"bufio"
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
	"strings"
	"sync"
	"syscall"
	"time"
)

// State is what a server is doing.
type State string

const (
	StateInstalling State = "installing"
	StateImporting  State = "importing"
	StateUpdating   State = "updating"
	StateRestoring  State = "restoring"
	StateStopped    State = "stopped"
	StateStarting   State = "starting"
	StateRunning    State = "running"
	StateStopping   State = "stopping"
	StateCrashed    State = "crashed" // gave up restarting
	StateError      State = "error"   // can't start until fixed, see Message
)

// Player is someone on a server.
type Player struct {
	Name  string    `json:"name"`
	XUID  string    `json:"xuid,omitempty"`
	Since time.Time `json:"since"`
}

// Status is a snapshot of a server for the web page.
type Status struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	Version   string   `json:"version"`
	Port      int      `json:"port"`
	State     State    `json:"state"`
	Message   string   `json:"message,omitempty"`
	Players   []Player `json:"players"`
	AutoStart bool     `json:"autoStart"`
	Preview   bool     `json:"preview,omitempty"`
	// Waiting is how many players tried to join but aren't on the allowlist.
	Waiting int `json:"waiting"`
	// LastBackup is when the newest backup was made (only filled in by List).
	LastBackup *time.Time `json:"lastBackup,omitempty"`
	// HoldVersion: overnight updates skip it.
	HoldVersion bool `json:"holdVersion,omitempty"`
	// Outside is true while it's open to friends outside the house.
	Outside bool `json:"outside,omitempty"`
	// Java servers: the Paper build, memory (MB), and a newer release when
	// there is one ("1.21.5 (Paper build 12)").
	Build    int    `json:"build,omitempty"`
	MemoryMB int    `json:"memoryMB,omitempty"`
	UpdateTo string `json:"updateTo,omitempty"`
	// Consoles: console players are turned on (Java servers). ConsolesLive:
	// the running server has Geyser loaded, so consoles can join now.
	Consoles     bool `json:"consoles,omitempty"`
	ConsolesLive bool `json:"consolesLive,omitempty"`
	// ConsolesUpdate: a newer Geyser is out (Update installs it).
	ConsolesUpdate bool `json:"consolesUpdate,omitempty"`
	// ConsolesWaiting is the server's Minecraft version when Geyser doesn't
	// support it yet, so consoles can't join until a newer Geyser is out.
	ConsolesWaiting string `json:"consolesWaiting,omitempty"`
	// Hours: when it's open, if it has hours.
	Hours *HoursStatus `json:"hours,omitempty"`
}

const (
	consoleLines  = 2000
	crashWindow   = 10 * time.Minute
	maxCrashes    = 4 // in crashWindow before giving up
	updateBackups = 5
	commandQueue  = 64
)

// crashBackoff is the first wait before restarting a crashed server; tests
// shorten it.
var crashBackoff = 5 * time.Second

// Server is one game server and, when running, its process.
//
// Nothing that can block (pipe writes, file copies, downloads) happens while
// mu is held: the web page reads Status under mu every couple of seconds, and
// a stuck game server must never freeze the panel.
type Server struct {
	m *Manager

	mu       sync.Mutex
	meta     Meta
	state    State
	message  string // what's happening now, e.g. "Restarting after a crash"
	notice   string // stays until the next successful update, e.g. "Update failed: …"
	players  map[string]Player
	busy     bool // installing or updating
	console  []string
	subs     map[chan string]struct{}
	cmd      *exec.Cmd
	commands chan string // lines for the running process's stdin; closed when it exits
	exited   chan struct{}
	stopping bool // the current process was asked to stop
	crashes  []time.Time
	restart  *time.Timer

	saveMu sync.Mutex // orders writes of .blockheads.json

	filesMu  sync.Mutex // orders edits of allowlist.json, permissions.json and server.properties from the Players page
	attempts []JoinAttempt

	// Backups. backupMu is held while a backup, restore or update runs.
	backupMu          sync.Mutex
	backupActive      bool // a backup or restore is running
	coldBackup        bool // backing up a stopped server; Start waits
	hotBackup         bool // "save hold" is on; its replies stay out of the console
	saveReady         bool
	saveList          chan saveResult
	hideSaveUntil     time.Time // after "save resume", its reply stays out of the console too
	retryScheduledAt  time.Time // after a failed scheduled backup
	backupErr         string
	playedSinceBackup bool
	// playing is the world folder the running process loaded (level-name
	// when it started), which may differ from server.properties after a
	// change that hasn't been applied by a restart.
	playing           string
	javaSaved         chan struct{} // a Java server finished "save-all" during a backup
	deleted           bool          // removed by DeleteServer; nothing more happens to it
	startedAt         time.Time
	autoUpdating      bool          // the update running was started overnight (for its notification)
	backupFailNoted   bool          // a failed scheduled backup was notified; reset by a good one
	lbTime, lbChecked time.Time     // cached newest backup time (see lastBackup)
	pluginsMu         sync.Mutex    // one console plugin install at a time
	consolesLive      bool          // the running process was started with Geyser and Floodgate
	hytaleSess        hytaleSession // for the next start of a Hytale server
	hytaleSessErr     error
	hytaleSessAt      time.Time
	updatedConsoles   bool      // the update running installed new console plugins (for its notification)
	pluginsChanged    time.Time // when a Java plugin was last added, updated, removed or turned on or off
	jarSums           sync.Map  // plugin jar path -> jarSum (SHA-256 while unchanged)
}

func newServer(m *Manager, meta Meta) *Server {
	return &Server{m: m, meta: meta, state: StateStopped, players: map[string]Player{}, subs: map[chan string]struct{}{}, playedSinceBackup: true}
}

// ID returns the server's ID (it never changes).
func (s *Server) ID() string { return s.meta.ID }

// displayName returns the server's name in the panel. The lock order is
// Manager.mu before Server.mu, so this may be called with m.mu held.
func (s *Server) displayName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta.Name
}

func (s *Server) dir() string { return filepath.Join(s.m.serversDir(), s.meta.ID) }
func (s *Server) binary() string {
	if s.isHytale() {
		return filepath.Join(s.dir(), hytaleJar)
	}
	if s.isJava() {
		return filepath.Join(s.dir(), paperJar)
	}
	return filepath.Join(s.dir(), "bedrock_server")
}

// incompleteMarker exists while an update is known to have been applied only
// partly. The server won't start until it's gone: a successful update removes
// it, or the owner deletes it after restoring the backup.
func (s *Server) incompleteMarker() string { return filepath.Join(s.dir(), ".update-incomplete") }

func (s *Server) isBusy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy
}

// Status returns a snapshot.
func (s *Server) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{
		ID: s.meta.ID, Name: s.meta.Name, Type: s.meta.Type, Version: s.meta.Version,
		Port: s.meta.Port, State: s.state, Message: s.message, AutoStart: s.meta.AutoStart,
		Players: []Player{}, Preview: s.meta.Preview, Waiting: len(s.attempts),
		Outside: s.meta.Outside, HoldVersion: s.meta.HoldVersion, Hours: s.hoursStatusLocked(time.Now()),
	}
	if s.isJava() {
		st.Build, st.MemoryMB = s.meta.Build, s.meta.MemoryMB
		if st.MemoryMB == 0 {
			st.MemoryMB = defaultMemoryMB
		}
		st.Consoles = s.consolesOnLocked()
		if r := s.m.LatestPaper(st.Consoles); st.Version != "" && javaNewer(r, st.Version, st.Build) {
			st.UpdateTo = r.label()
		}
		st.ConsolesUpdate = s.consolePluginsBehindLocked()
		if supported, known := s.m.GeyserSupports(st.Version); st.Consoles && st.Version != "" && known && !supported {
			st.ConsolesWaiting = st.Version
		}
		st.ConsolesLive = st.Consoles && s.consolesLive && s.cmd != nil && s.state == StateRunning
	}
	if s.isHytale() {
		st.MemoryMB = s.meta.MemoryMB
		if st.MemoryMB == 0 {
			st.MemoryMB = hytaleMemoryMB
		}
		if r := s.m.LatestHytale(); st.Version != "" && hytaleNewer(r.Version, st.Version) {
			st.UpdateTo = r.Version
		}
	}
	for _, p := range s.players {
		st.Players = append(st.Players, p)
	}
	sort.Slice(st.Players, func(i, j int) bool { return st.Players[i].Since.Before(st.Players[j].Since) })
	if st.Message == "" {
		st.Message = s.notice
	}
	if st.Message == "" && s.backupActive {
		st.Message = "Backing up…"
	}
	return st
}

// saveMeta writes the server's details. Writes are serialised and each one
// takes its snapshot while holding saveMu, so the last write always carries
// the latest details. It must not be called with mu held; use saveMetaLater.
func (s *Server) saveMeta() error { return s.writeMeta(s.dir()) }

// writeMeta writes the server's details into dir.
func (s *Server) writeMeta(dir string) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	if s.deleted {
		s.mu.Unlock()
		return errDeleted // its folder is gone (or belongs to a server brought back)
	}
	b, _ := json.MarshalIndent(s.meta, "", "  ")
	s.mu.Unlock()
	path := filepath.Join(dir, metaFile)
	if err := os.WriteFile(path+".tmp", b, 0o644); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// saveMetaLater saves the details from a new goroutine, for callers that
// hold mu.
func (s *Server) saveMetaLater() {
	s.m.saves.Add(1)
	go func() {
		defer s.m.saves.Done()
		if err := s.saveMeta(); err != nil {
			s.m.log.Warn("could not save server details", "id", s.meta.ID, "error", err)
		}
	}()
}

// ---- console ----

// say adds a panel message to the console.
func (s *Server) say(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addLineLocked("[Panel] " + msg)
}

func (s *Server) addLineLocked(line string) {
	s.console = append(s.console, line)
	if len(s.console) > consoleLines {
		s.console = append([]string(nil), s.console[len(s.console)-consoleLines:]...)
	}
	for ch := range s.subs {
		select {
		case ch <- line:
		default: // a slow viewer misses lines rather than holding up the server
		}
	}
}

// Subscribe returns the console so far and a channel of new lines. Call the
// returned function to stop.
func (s *Server) Subscribe() ([]string, <-chan string, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan string, 256)
	s.subs[ch] = struct{}{}
	backlog := append([]string(nil), s.console...)
	return backlog, ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}

// ---- log events ----

var (
	rePlayerJoin  = regexp.MustCompile(`Player connected: (.+?), xuid: ?(\d*)`)
	rePlayerLeave = regexp.MustCompile(`Player disconnected: (.+?), xuid: ?(\d*)`)
	reVersion     = regexp.MustCompile(`\bVersion:? ([0-9]+(?:\.[0-9]+){2,3})`)
)

// onLineLocked handles one line of server output. Caller holds s.mu.
func (s *Server) onLineLocked(line string) {
	if s.isHytale() {
		line = reANSI.ReplaceAllString(line, "")
		s.addLineLocked(line)
		s.onHytaleLineLocked(line)
		return
	}
	// Colour codes and a stray carriage return would stop the lines from
	// matching (and look odd in the console).
	line = strings.TrimRight(reANSI.ReplaceAllString(line, ""), "\r")
	if s.isJava() {
		if s.onJavaSaveLineLocked(line) {
			return
		}
		s.addLineLocked(line)
		s.onJavaLineLocked(line)
		return
	}
	if s.onSaveLineLocked(line) {
		return
	}
	s.addLineLocked(line)
	switch {
	case strings.Contains(line, "Server started."):
		if s.state == StateStarting {
			s.state = StateRunning
			s.message = ""
			s.m.log.Info("server running", "id", s.meta.ID)
		}
	case rePlayerJoin.MatchString(line):
		mm := rePlayerJoin.FindStringSubmatch(line)
		s.players[mm[1]] = Player{Name: mm[1], XUID: mm[2], Since: time.Now()}
		s.playedSinceBackup = true
		s.m.playtime.kick() // a kid whose time is up is moved off at once
		s.notifyLocked(notify.PlayerJoined, mm[1]+" is playing on "+s.meta.Name, mm[1]+" joined "+s.meta.Name+".",
			"play/"+s.meta.ID+"/"+strings.ToLower(mm[1]))
		s.m.saves.Add(1)
		go func(name, xuid string) {
			defer s.m.saves.Done()
			s.onJoin(name, xuid)
		}(mm[1], mm[2])
	case rePlayerLeave.MatchString(line):
		mm := rePlayerLeave.FindStringSubmatch(line)
		s.playerLeftLocked(mm[1])
	case reVersion.MatchString(line) && s.state == StateStarting:
		if v := reVersion.FindStringSubmatch(line)[1]; v != s.meta.Version {
			s.meta.Version = v
			s.saveMetaLater()
		}
	}
}

// ---- start and stop ----

var (
	errBusy         = errors.New("the server is busy installing, updating or restoring; try again when it's done")
	errShuttingDown = errors.New("the panel is shutting down")
	errRestoring    = errors.New("a backup is being restored; wait for it to finish")
	// ErrImporting is returned for changes to a server that's still being copied.
	ErrImporting = errors.New("the server is still being imported; wait for the copy to finish")
)

// Start starts the server and marks it to start again after a panel restart.
func (s *Server) Start() error {
	if s.m.ctx.Err() != nil {
		return errShuttingDown
	}
	s.applyConsoleFileChanges()
	serr := s.prepareHytaleStart()
	s.mu.Lock()
	defer s.mu.Unlock()
	if serr != nil && !errors.Is(serr, ErrHytaleSignIn) && s.cmd == nil && !s.busy && s.restart == nil {
		// Hytale's sign-in service can't be reached (the network may still
		// be coming up): keep trying, as after a crash.
		s.waitForHytaleLocked(serr)
		if !s.meta.AutoStart {
			s.meta.AutoStart = true
			s.saveMetaLater()
		}
		return nil
	}
	if s.busy {
		return errBusy
	}
	if s.coldBackup {
		return errors.New("a backup is being made; try again in a moment")
	}
	if s.cmd != nil {
		return errors.New("the server is already running")
	}
	if s.restart != nil {
		s.restart.Stop()
		s.restart = nil
	}
	s.crashes = nil
	if !s.meta.AutoStart {
		s.meta.AutoStart = true
		s.saveMetaLater()
	}
	return s.startLocked()
}

// failLocked puts the server in the error state with a message.
func (s *Server) failLocked(msg string) error {
	s.state = StateError
	s.message = msg
	s.addLineLocked("[Panel] " + msg)
	return errors.New(msg)
}

func (s *Server) startLocked() error {
	bin := s.binary()
	if _, err := os.Stat(bin); err != nil {
		return s.failLocked("The server files are missing. Use Update to download them again.")
	}
	if b, err := os.ReadFile(s.incompleteMarker()); err == nil {
		return s.failLocked(strings.TrimSpace(string(b)))
	}
	var cmd *exec.Cmd
	if s.isHytale() {
		sess, serr := s.hytaleSess, s.hytaleSessErr
		s.hytaleSess, s.hytaleSessErr = hytaleSession{}, nil
		if serr != nil {
			return s.failLocked("Couldn't sign the server in to Hytale: " + serr.Error())
		}
		if sess.Session == "" {
			return s.failLocked("The server has no Hytale session yet; press Start again.")
		}
		java, args, env, err := s.hytaleCommand(sess)
		if err != nil {
			return s.failLocked(err.Error())
		}
		cmd = exec.Command(java, args...)
		cmd.Env = env
	} else if s.isJava() {
		if err := s.applyJavaProperties(false); err != nil {
			s.addLineLocked("[Panel] Couldn't update server.properties: " + err.Error())
		}
		s.consolesLive = s.consolesOnLocked() && s.consolesInstalled()
		if s.consolesOnLocked() && !s.consolesLive {
			s.addLineLocked("[Panel] Console support isn't installed yet, so only Java players can join. Use Update now to install it.")
		}
		java, args, err := s.javaCommand()
		if err != nil {
			return s.failLocked(err.Error())
		}
		cmd = exec.Command(java, args...)
		cmd.Env = os.Environ()
	} else {
		// The execute bit gets lost when files are copied around by hand, which
		// is the classic "Permission denied: ./bedrock_server". Set it every time.
		_ = os.Chmod(bin, 0o755)
		if err := s.applyPanelProperties(false); err != nil {
			s.addLineLocked("[Panel] Couldn't update server.properties: " + err.Error())
		}
		cmd = exec.Command(bin)
		cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH=.")
	}
	cmd.Dir = s.dir()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return s.failLocked("Couldn't start the server: " + err.Error())
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		stdin.Close()
		return s.failLocked("Couldn't start the server: " + err.Error())
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	s.addLineLocked(fmt.Sprintf("[Panel] Starting %s on port %d…", s.meta.Name, s.meta.Port))
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		stdin.Close() // exec closes its own pipe ends; ours too, to be sure
		return s.failLocked("Couldn't start the server: " + err.Error())
	}
	pw.Close()
	commands := make(chan string, commandQueue)
	s.cmd, s.commands, s.exited, s.stopping = cmd, commands, make(chan struct{}), false
	s.startedAt = time.Now()
	s.playing = ""
	if p, err := readProperties(s.propertiesPath()); err == nil {
		s.playing, _ = p.get("level-name")
	}
	if strings.TrimSpace(s.playing) == "" {
		s.playing = "Bedrock level"
		if s.isJava() {
			s.playing = javaWorldDefault
		}
		if s.isHytale() {
			s.playing = hytaleWorldFolder
		}
	}
	s.state, s.message = StateStarting, ""
	s.players = map[string]Player{}
	s.m.log.Info("server starting", "id", s.meta.ID, "pid", cmd.Process.Pid)

	// One writer per process, so a server that stops reading its input
	// can only ever block this goroutine.
	go func() {
		defer stdin.Close()
		for line := range commands {
			if _, err := io.WriteString(stdin, line+"\n"); err != nil {
				for range commands { // drain until the process exits
				}
				return
			}
		}
	}()

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		sc := bufio.NewScanner(pr)
		// Big worlds make long "save query" file lists.
		sc.Buffer(make([]byte, 64*1024), 16<<20)
		for sc.Scan() {
			s.mu.Lock()
			s.onLineLocked(sc.Text())
			s.mu.Unlock()
		}
		pr.Close()
	}()
	go func() {
		err := cmd.Wait()
		select {
		case <-readDone:
		case <-time.After(3 * time.Second):
		}
		s.onExit(cmd, err)
	}()
	return nil
}

// sendLocked queues a line for the process without blocking. Caller holds
// s.mu and has checked s.cmd != nil.
func (s *Server) sendLocked(line string) bool {
	select {
	case s.commands <- line:
		return true
	default:
		return false
	}
}

func (s *Server) onExit(cmd *exec.Cmd, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != cmd {
		return
	}
	requested := s.stopping
	close(s.exited)
	close(s.commands)
	s.cmd, s.commands, s.exited = nil, nil, nil
	s.everyoneLeftLocked()
	cleanExit := cmd.ProcessState != nil && cmd.ProcessState.Success()
	if requested || cleanExit {
		// A clean exit we didn't ask for is the server being stopped from
		// inside (a script or an operator), not a crash.
		s.state = StateStopped
		s.addLineLocked("[Panel] Server stopped.")
		s.m.log.Info("server stopped", "id", s.meta.ID)
		return
	}

	code := "unknown"
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.String()
	}
	now := time.Now()
	recent := s.crashes[:0]
	for _, t := range s.crashes {
		if now.Sub(t) < crashWindow {
			recent = append(recent, t)
		}
	}
	s.crashes = append(recent, now)
	s.m.log.Warn("server stopped unexpectedly", "id", s.meta.ID, "exit", code, "crashesRecently", len(s.crashes))
	if len(s.crashes) >= maxCrashes || s.m.ctx.Err() != nil {
		s.state = StateCrashed
		s.message = fmt.Sprintf("Stopped after crashing %d times in %d minutes. Check the console, then press Start.", len(s.crashes), int(crashWindow.Minutes()))
		s.addLineLocked("[Panel] " + s.message)
		if s.m.ctx.Err() == nil {
			s.notifyLocked(notify.Crash, s.meta.Name+" keeps crashing", s.message, "crashstop/"+s.meta.ID)
		}
		return
	}
	wait := time.Duration(len(s.crashes)*len(s.crashes)) * crashBackoff // 5s, 20s, 45s
	s.notifyLocked(notify.Crash, s.meta.Name+" crashed",
		fmt.Sprintf("%s stopped unexpectedly (%s). The panel is starting it again.", s.meta.Name, code), "crash/"+s.meta.ID)
	s.state = StateStarting
	s.message = "Restarting after a crash"
	s.addLineLocked(fmt.Sprintf("[Panel] The server stopped unexpectedly (%s). Restarting in %s…", code, wait))
	s.restart = time.AfterFunc(wait, s.crashRestart)
}

// crashRestart starts the server again after a crash. A Hytale server that
// can't get a session (Hytale's services are down) tries again every minute.
func (s *Server) crashRestart() {
	err := s.prepareHytaleStart()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.restart == nil || s.cmd != nil || s.busy || s.m.ctx.Err() != nil {
		return
	}
	if (err != nil && !errors.Is(err, ErrHytaleSignIn)) || (s.isHytale() && s.coldBackup) {
		if err == nil {
			err = errors.New("a backup is being made")
		}
		s.waitForHytaleLocked(err)
		return
	}
	s.restart = nil
	_ = s.startLocked()
}

// waitForHytaleLocked starts the server once a Hytale session can be had,
// trying every hytaleRetryEvery. Caller holds s.mu.
func (s *Server) waitForHytaleLocked(err error) {
	s.state = StateStarting
	s.message = "Starting (waiting for Hytale's sign-in service)"
	s.addLineLocked("[Panel] Couldn't get a Hytale session (" + err.Error() + "); trying again in a minute.")
	s.hytaleSessErr = nil
	s.restart = time.AfterFunc(hytaleRetryEvery, s.crashRestart)
}

// hytaleRetryEvery is how often a Hytale server waiting for a session tries
// again; tests shorten it.
var hytaleRetryEvery = time.Minute

// Stop asks the server to save and stop, and marks it to stay stopped after a
// panel restart.
func (s *Server) Stop() error {
	s.mu.Lock()
	if s.state == StateImporting {
		s.mu.Unlock()
		return ErrImporting
	}
	if s.meta.AutoStart {
		s.meta.AutoStart = false
		s.saveMetaLater()
	}
	s.mu.Unlock()
	s.stop(true)
	return nil
}

// EditBlocked says why the server's files can't be changed right now (it's
// being imported or restored), or returns nil.
func (s *Server) EditBlocked() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleted {
		return errDeleted
	}
	switch s.state {
	case StateImporting:
		return ErrImporting
	case StateRestoring:
		return errRestoring
	}
	return nil
}

// Importing reports whether the server is still being copied in.
func (s *Server) Importing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == StateImporting
}

// stop stops the process. It returns a channel closed when the process has
// exited, or nil if nothing was running.
func (s *Server) stop(user bool) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.restart != nil {
		s.restart.Stop()
		s.restart = nil
		if s.cmd == nil {
			s.state = StateStopped
			s.message = ""
		}
	}
	if s.cmd == nil {
		return nil
	}
	exited := s.exited
	if s.stopping {
		return exited
	}
	s.stopping = true
	s.state = StateStopping
	if user {
		s.addLineLocked("[Panel] Stopping (saving the world first)…")
	} else {
		s.addLineLocked("[Panel] The panel is shutting down; stopping the server…")
	}
	// Start the fallback first, so a server that won't take the command is
	// still stopped.
	pid := s.cmd.Process.Pid
	timeout := s.m.opts.StopTimeout
	go func() {
		select {
		case <-exited:
			return
		case <-time.After(timeout):
		}
		s.say("The server didn't stop in time; forcing it.")
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}()
	s.sendLocked("stop")
	return exited
}

// Restart stops the server if it's running, then starts it.
func (s *Server) Restart() error {
	if err := s.prepareHytaleRestart(); err != nil {
		s.mu.Lock()
		up := s.cmd != nil
		s.mu.Unlock()
		if up {
			s.say(err.Error())
			return err
		}
	}
	if done := s.stop(true); done != nil {
		<-done
	}
	return s.Start()
}

// Command sends a line to the server console, like "say hello". "stop" is
// handled like the Stop button.
func (s *Server) Command(line string) error {
	line = strings.TrimSpace(line)
	if line == "" || strings.ContainsAny(line, "\r\n") {
		return errors.New("type one command")
	}
	if strings.EqualFold(strings.TrimPrefix(line, "/"), "stop") {
		s.say("> " + line)
		return s.Stop()
	}
	if err := s.outsideCommand(line); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil {
		return errors.New("the server isn't running")
	}
	s.addLineLocked("> " + line)
	if !s.sendLocked(line) {
		return errors.New("the server isn't taking commands right now; it may be stuck")
	}
	return nil
}

// ---- install and update ----

func (s *Server) installAndStart(extra []string) {
	defer s.m.work.Done()
	err := s.install(false)
	s.mu.Lock()
	s.busy = false
	if err != nil {
		s.failLocked("Install failed: " + err.Error())
		s.mu.Unlock()
		s.m.log.Error("install failed", "id", s.meta.ID, "error", err)
		return
	}
	s.state = StateStopped
	s.mu.Unlock()
	ops := extra
	if owner := s.m.Settings().OwnerGamertag; owner != "" && s.isBedrock() {
		ops = append([]string{owner}, extra...) // an Xbox gamertag
	} else if owner != "" && s.ConsolesOn() && s.consolesInstalled() {
		if _, javaName, err := consoleIDs(owner); err == nil {
			ops = append(ops, javaName) // the owner on a console
		}
	}
	if s.isHytale() {
		// The owner's Hytale profile: on the allowlist (which is turned on)
		// and an operator, as the server first starts.
		a := s.m.HytaleAccount()
		uuid := s.m.hytaleOwnerUUID()
		if a.Username != "" && uuid != "" {
			s.mu.Lock()
			if s.meta.HytaleNames == nil {
				s.meta.HytaleNames = map[string]string{}
			}
			s.meta.HytaleNames[strings.ToLower(uuid)] = a.Username
			s.mu.Unlock()
			ops = []string{a.Username}
		}
		if err := s.SetAllowlistEnabled(true); err != nil {
			s.say("Couldn't turn the allowlist on: " + err.Error())
		}
	}
	for i, name := range ops {
		if i > 0 && strings.EqualFold(name, ops[0]) {
			continue
		}
		if err := s.AllowlistAdd(name, ""); err != nil {
			s.say("Couldn't add " + name + " to the allowlist: " + err.Error())
		}
		if _, err := s.OpAdd(name); err != nil {
			s.say("Couldn't make " + name + " an operator: " + err.Error())
		}
	}
	if err := s.Start(); err != nil {
		s.m.log.Warn("could not start a new server", "id", s.meta.ID, "error", err)
	}
}

// Update backs the server up, downloads the current version and starts it
// again if it was running and nobody pressed Stop meanwhile. It returns
// straight away; follow the console.
func (s *Server) Update() error { return s.startUpdate(false) }

// UpdateOvernight is Update started by overnight upkeep (its result is
// sent as a notification).
func (s *Server) UpdateOvernight() error { return s.startUpdate(true) }

func (s *Server) startUpdate(auto bool) error {
	if !s.m.beginWork() {
		return errShuttingDown
	}
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		s.m.work.Done()
		return errBusy
	}
	s.busy = true
	s.autoUpdating = auto
	s.updatedConsoles = false
	versionBefore, buildBefore := s.meta.Version, s.meta.Build
	wasRunning := s.cmd != nil || s.restart != nil
	s.mu.Unlock()

	go func() {
		defer s.m.work.Done()
		s.backupMu.Lock() // let a backup in progress finish
		defer s.backupMu.Unlock()
		if done := s.stop(true); done != nil {
			<-done
		}
		s.mu.Lock()
		s.state = StateUpdating
		s.message = ""
		s.mu.Unlock()

		err := s.update()
		s.mu.Lock()
		s.busy = false
		var partial *partialUpdateError
		switch {
		case errors.As(err, &partial):
			msg := "Update failed partway: " + partial.Error() + " Restore the backup " + partial.backup + " before starting the server."
			if partial.hint != "" {
				msg = "Update failed partway: " + partial.Error() + " " + partial.hint
			}
			_ = os.WriteFile(s.incompleteMarker(), []byte(msg+"\n"), 0o644)
			s.failLocked(msg)
			if s.autoUpdating {
				s.notifyLocked(notify.Updated, "Overnight update of "+s.meta.Name+" failed", msg, "")
			}
			s.autoUpdating = false
			s.mu.Unlock()
			s.m.log.Error("update failed partway", "id", s.meta.ID, "error", err)
			return
		case err != nil:
			s.notice = "Update failed: " + err.Error() + " Nothing was changed."
			s.addLineLocked("[Panel] " + s.notice)
			s.m.log.Error("update failed", "id", s.meta.ID, "error", err)
			if s.autoUpdating {
				s.notifyLocked(notify.Updated, "Overnight update of "+s.meta.Name+" failed", s.notice, "")
			}
		default:
			s.notice = ""
			if s.autoUpdating {
				what := "Bedrock " + s.meta.Version
				if s.isHytale() {
					what = "Hytale " + s.meta.Version
				}
				if s.isJava() {
					what = fmt.Sprintf("Minecraft Java %s (Paper build %d)", s.meta.Version, s.meta.Build)
				}
				text := s.meta.Name + " was updated overnight to " + what + "."
				if s.isJava() && versionBefore == s.meta.Version && buildBefore == s.meta.Build && s.updatedConsoles {
					text = s.meta.Name + "'s console support (Geyser) was updated overnight, so consoles on the newest Minecraft can keep joining."
				}
				s.notifyLocked(notify.Updated, s.meta.Name+" was updated", text, "")
			}
		}
		s.autoUpdating = false
		if _, statErr := os.Stat(s.binary()); statErr != nil {
			s.state = StateError
		} else {
			s.state = StateStopped
		}
		startAgain := wasRunning && s.meta.AutoStart && s.state == StateStopped
		s.mu.Unlock()
		if startAgain {
			_ = s.Start()
		}
	}()
	return nil
}

// partialUpdateError means an update changed some files but not all.
type partialUpdateError struct {
	err    error
	backup string
	hint   string // what to do instead of restoring the backup
}

func (e *partialUpdateError) Error() string { return e.err.Error() }
func (e *partialUpdateError) Unwrap() error { return e.err }

func (s *Server) update() error {
	backupPath := ""
	if _, err := os.Stat(s.binary()); err == nil {
		s.say("Backing up before the update…")
		var path string
		var err error
		if s.isHytale() {
			// Hytale's server is a few GB, and is downloaded again anyway:
			// the backup holds the worlds and settings.
			path, err = writeWorldBackup(s.m.ctx, s.dir(), s.m.backupsDir(s.meta.ID), s.meta.ID, "before-update")
			if err == nil {
				pruneBackups(s.m.backupsDir(s.meta.ID), s.meta.ID, "before-update", updateBackups)
			}
		} else {
			path, err = backup(s.m.ctx, s.dir(), s.m.backupsDir(s.meta.ID), s.meta.ID, "before-update", updateBackups)
		}
		if err == nil {
			s.m.backupMade()
		}
		if err != nil {
			return fmt.Errorf("backup failed, so nothing was changed: %w", err)
		}
		s.forgetLastBackup()
		backupPath = path
		s.say("Backup saved: " + filepath.Base(path))
	}
	err := s.install(true)
	var partial *partialUpdateError
	if errors.As(err, &partial) {
		partial.backup = backupPath
	}
	return err
}

// install downloads the current version into the server folder. On an
// update, settings, the allowlist, permissions and worlds are kept.
func (s *Server) install(update bool) error {
	if s.isHytale() {
		return s.installHytale(update)
	}
	if s.isJava() {
		return s.installJava(update)
	}
	ctx, cancel := context.WithTimeout(s.m.ctx, 30*time.Minute)
	defer cancel()
	s.mu.Lock()
	preview, current := s.meta.Preview, s.meta.Version
	s.mu.Unlock()

	s.say("Asking Mojang for the current Bedrock server…")
	url, version, err := s.m.latestBedrock(ctx, preview)
	if err != nil {
		return err
	}
	_, statErr := os.Stat(s.binary())
	if update && statErr == nil && version != "" && version == current {
		s.say("Already on the current version (" + version + ").")
		return nil
	}
	s.say("Installing Bedrock " + version + ". By downloading it you accept the Minecraft EULA: https://www.minecraft.net/eula")
	zipPath, err := s.m.download(ctx, url, s.m.serversDir(), s.say)
	if err != nil {
		return err
	}
	defer os.Remove(zipPath)
	staging, err := os.MkdirTemp(s.m.serversDir(), ".staging-"+s.meta.ID+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	s.say("Unpacking…")
	if err := extractZip(ctx, zipPath, staging); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(staging, "bedrock_server")); err != nil {
		return errors.New("the download doesn't contain bedrock_server")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := mergeInto(ctx, staging, s.dir(), update); err != nil {
		var partial *partialUpdateError
		if errors.As(err, &partial) {
			return err
		}
		return fmt.Errorf("couldn't copy the new files into place: %w", err)
	}
	_ = os.Chmod(s.binary(), 0o755)

	s.mu.Lock()
	s.meta.Version = version
	s.mu.Unlock()
	if err := s.saveMeta(); err != nil {
		return err
	}
	s.mu.Lock()
	err = s.applyPanelProperties(!update)
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("couldn't set up server.properties: %w", err)
	}
	_ = os.Remove(s.incompleteMarker())
	s.say("Bedrock " + version + " is installed.")
	return nil
}

// applyOwnProperties sets the settings the panel manages, for the server's
// edition. Caller holds s.mu.
func (s *Server) applyOwnProperties() error {
	if s.isHytale() {
		return nil // its port is given on the command line
	}
	if s.isJava() {
		return s.applyJavaProperties(false)
	}
	return s.applyPanelProperties(false)
}

// applyPanelProperties sets the settings the panel manages. Caller holds
// s.mu. With fresh, it also names the server after the panel name.
func (s *Server) applyPanelProperties(fresh bool) error {
	path := filepath.Join(s.dir(), "server.properties")
	p, err := readProperties(path)
	if err != nil {
		return err
	}
	before := strings.Join(p.lines, "\n")
	if fresh {
		p.set("server-name", s.meta.Name)
	}
	p.set("server-port", fmt.Sprint(s.meta.Port))
	p.set("server-portv6", fmt.Sprint(s.meta.PortV6))
	// The console server list answers LAN searches on port 19132 for the
	// whole container; a game server doing the same would clash with it.
	p.set("enable-lan-visibility", "false")
	// NetherNet gameplay ports: a fixed range per server (Mojang's file has
	// the line commented out, so ports would otherwise be random), and,
	// while friends outside can join, the same range at the home's internet
	// address, so their consoles are told where to connect.
	first, last := BedrockUDPPorts(s.meta.Port)
	ports := fmt.Sprintf("%d-%d", first, last)
	if ip := s.m.hostAddresses().Internet; ip != "" {
		if strings.Contains(ip, ":") {
			ip = "[" + ip + "]"
		}
		ports += fmt.Sprintf(",%s:%d-%d:%d-%d", ip, first, last, first, last)
	}
	p.set("server-udp-ports", ports)
	if strings.Join(p.lines, "\n") == before {
		return nil
	}
	return p.write(path)
}
