package servers

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Deleting a server. Only the owner can (the web layer checks). The server
// is stopped, a final backup of its worlds and settings is made, and its
// folder is removed. Its backups stay (with a note of what the server was),
// so it can be brought back, with the Bedrock version downloaded again.

const deletedFile = "deleted-server.json"

// DeletedServer is a server that was deleted but whose backups are kept.
type DeletedServer struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Port     int       `json:"port"`
	Deleted  time.Time `json:"deleted"`
	Backups  int       `json:"backups"`
	Size     int64     `json:"size"`
	Final    string    `json:"final"` // the backup made when it was deleted
	CanBring bool      `json:"canBringBack"`
}

type deletedNote struct {
	Meta    Meta      `json:"meta"`
	Deleted time.Time `json:"deleted"`
	Final   string    `json:"final"`
}

var errDeleted = errors.New("this server was deleted")

// DeleteServer stops a server, makes a final backup and removes it.
func (m *Manager) DeleteServer(id string) error {
	s, ok := m.Get(id)
	if !ok {
		return errors.New("there's no such server")
	}
	if !m.beginWork() {
		return errShuttingDown
	}
	defer m.work.Done()
	s.mu.Lock()
	switch {
	case s.deleted:
		s.mu.Unlock()
		return errDeleted
	case s.state == StateImporting:
		s.mu.Unlock()
		return ErrImporting
	case s.busy:
		s.mu.Unlock()
		return errBusy
	}
	s.busy = true // nothing else starts on it from here
	wasRunning := s.cmd != nil || s.restart != nil
	s.mu.Unlock()
	fail := func(err error) error {
		s.mu.Lock()
		s.busy = false
		s.mu.Unlock()
		if wasRunning {
			_ = s.Start()
		}
		return err
	}

	s.backupMu.Lock() // wait for a backup in progress
	defer s.backupMu.Unlock()
	s.say("Deleting the server: stopping it and making a final backup…")
	if done := s.stop(true); done != nil {
		<-done
	}
	// Nothing changes its files from here (uploads, settings, players), so
	// the final backup has everything; then it's marked deleted, which
	// refuses any change still on its way.
	s.filesMu.Lock()
	backupDir := m.backupsDir(id)
	final, err := writeWorldBackup(m.ctx, s.dir(), backupDir, id, "final")
	if err != nil {
		s.filesMu.Unlock()
		return fail(fmt.Errorf("couldn't make the final backup, so the server wasn't deleted: %w", err))
	}
	s.mu.Lock()
	meta := s.meta
	s.mu.Unlock()
	meta.AutoStart = false
	meta.Outside = false
	b, _ := json.MarshalIndent(deletedNote{Meta: meta, Deleted: time.Now().UTC(), Final: filepath.Base(final)}, "", "  ")
	if err := os.WriteFile(filepath.Join(backupDir, deletedFile), b, 0o644); err != nil {
		s.filesMu.Unlock()
		return fail(err)
	}
	if final != "" {
		m.backupMade()
	}
	s.mu.Lock()
	s.deleted = true
	s.state = StateStopped
	s.mu.Unlock()
	m.mu.Lock()
	delete(m.servers, id)
	m.mu.Unlock()
	// Moved aside first, so a half-removed folder is never loaded as a server.
	gone := filepath.Join(m.serversDir(), ".deleted-"+id)
	_ = os.RemoveAll(gone)
	if err := os.Rename(s.dir(), gone); err != nil {
		m.log.Warn("couldn't move a deleted server's folder aside", "id", id, "error", err)
		_ = os.RemoveAll(s.dir())
	}
	s.filesMu.Unlock()
	_ = os.RemoveAll(gone)
	m.log.Info("server deleted", "id", id, "name", meta.Name, "finalBackup", filepath.Base(final))
	return nil
}

func (m *Manager) readDeleted(id string) (deletedNote, error) {
	var n deletedNote
	b, err := os.ReadFile(filepath.Join(m.backupsDir(id), deletedFile))
	if err != nil {
		return n, errors.New("there's no deleted server with that name")
	}
	if err := json.Unmarshal(b, &n); err != nil || n.Meta.ID != id {
		return n, errors.New("the deleted server's details can't be read")
	}
	return n, nil
}

// DeletedServers lists servers that were deleted, newest first.
func (m *Manager) DeletedServers() []DeletedServer {
	out := []DeletedServer{}
	root := filepath.Join(m.opts.DataDir, "backups")
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		if _, live := m.Get(id); live {
			continue
		}
		n, err := m.readDeleted(id)
		if err != nil {
			continue
		}
		d := DeletedServer{ID: id, Name: n.Meta.Name, Port: n.Meta.Port, Deleted: n.Deleted, Final: n.Final}
		files, _ := os.ReadDir(m.backupsDir(id))
		for _, f := range files {
			if mm := reBackupName.FindStringSubmatch(f.Name()); mm != nil && mm[1] == id {
				d.Backups++
				if info, err := f.Info(); err == nil {
					d.Size += info.Size()
				}
			}
		}
		_, err = os.Stat(filepath.Join(m.backupsDir(id), n.Final))
		d.CanBring = err == nil
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Deleted.After(out[j].Deleted) })
	return out
}

// BringBack re-creates a deleted server from its final backup and downloads
// Bedrock again. It returns straight away; follow the console. The server
// is left stopped.
func (m *Manager) BringBack(id string) (Status, error) {
	n, err := m.readDeleted(id)
	if err != nil {
		return Status{}, err
	}
	archive, err := os.Open(filepath.Join(m.backupsDir(id), n.Final))
	if err != nil {
		return Status{}, errors.New("its final backup is missing, so it can't be brought back")
	}
	defer archive.Close()
	if !m.beginWork() {
		return Status{}, errShuttingDown
	}
	started := false
	defer func() {
		if !started {
			m.work.Done()
		}
	}()

	meta := n.Meta
	m.mu.Lock()
	if _, taken := m.servers[id]; taken {
		m.mu.Unlock()
		return Status{}, errors.New("a server with that name is already here")
	}
	if _, err := os.Lstat(filepath.Join(m.serversDir(), id)); err == nil {
		m.mu.Unlock()
		return Status{}, errors.New("a folder for that server is already there")
	}
	meta.Name = m.freeNameLocked(meta.Name)
	// Keep its port if it's still free, so players' saved servers work.
	if !m.portFreeLocked(meta.Port, "") {
		port, err := m.freePort()
		if err != nil {
			m.mu.Unlock()
			return Status{}, err
		}
		meta.Port, meta.PortV6 = port, port+1
	}
	meta.AutoStart, meta.Outside, meta.Version = false, false, ""
	s := newServer(m, meta)
	s.state = StateInstalling
	s.busy = true
	m.servers[id] = s // holds the name and port while it's unpacked
	m.mu.Unlock()

	// Unpacked next to it and moved into place with its details, so a
	// restart meanwhile never leaves a folder that blocks bringing it back.
	staging, err := os.MkdirTemp(m.serversDir(), ".staging-"+id+"-")
	if err == nil {
		err = extractTarGz(m.ctx, archive, staging)
		if err == nil {
			err = s.writeMeta(staging)
		}
		if err == nil {
			err = os.Rename(staging, s.dir())
		}
		if err != nil {
			_ = os.RemoveAll(staging)
		}
	}
	if err != nil {
		m.remove(id)
		return Status{}, fmt.Errorf("couldn't bring it back: %w", err)
	}
	_ = os.Remove(filepath.Join(m.backupsDir(id), deletedFile))
	m.log.Info("bringing back a deleted server", "id", id, "name", meta.Name, "port", meta.Port)
	started = true
	go func() {
		defer m.work.Done()
		s.say("Brought back from " + n.Final + ". Downloading Bedrock again…")
		err := s.install(true)
		s.mu.Lock()
		s.busy = false
		if err != nil {
			s.failLocked("Install failed: " + err.Error() + " Use Update to try again.")
		} else {
			s.state = StateStopped
			s.addLineLocked("[Panel] Ready. Press Start to play.")
		}
		s.mu.Unlock()
	}()
	return s.Status(), nil
}

// freeNameLocked returns name, or name with a number, so no two servers
// share a name. Caller holds m.mu.
func (m *Manager) freeNameLocked(name string) string {
	taken := func(n string) bool {
		for _, s := range m.servers {
			if strings.EqualFold(s.displayName(), n) {
				return true
			}
		}
		return false
	}
	base := []rune(name)
	for i := 1; ; i++ {
		n := string(base)
		if i > 1 {
			suffix := fmt.Sprintf(" (%d)", i)
			r := base
			if len(r)+len([]rune(suffix)) > 40 {
				r = r[:40-len([]rune(suffix))]
			}
			n = strings.TrimSpace(string(r)) + suffix
		}
		if !taken(n) {
			return n
		}
	}
}

// ForgetDeleted removes a deleted server's backups for good.
func (m *Manager) ForgetDeleted(id string) error {
	m.mu.Lock() // so it can't be brought back meanwhile
	defer m.mu.Unlock()
	if _, err := m.readDeleted(id); err != nil {
		return err
	}
	if _, live := m.servers[id]; live {
		return errors.New("that server is back; delete its backups on its Backups tab")
	}
	return os.RemoveAll(m.backupsDir(id))
}
