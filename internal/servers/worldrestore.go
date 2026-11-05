package servers

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Restoring one world from a backup: the rest of the server (its other
// worlds, settings, allowlist) stays as it is. The world being replaced is
// backed up first, so it can be undone, and the server only stops if it's
// playing that world. A Java world comes with its _nether and _the_end
// folders when Paper keeps them apart.

// javaDimensions are the folders Paper keeps a world's other dimensions in.
var javaDimensions = []string{"_nether", "_the_end"}

// backupWorldSet lists the folders in a backup's worlds/.
func (s *Server) backupWorldSet(name string) (map[string]bool, error) {
	if s.isHytale() {
		return nil, errors.New("Hytale backups are restored whole")
	}
	if s.isJava() && s.isVelocity() {
		return nil, errors.New("a lobby has no worlds")
	}
	p, err := s.BackupPath(name)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("couldn't read the backup: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	set := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("couldn't read the backup: %w", err)
		}
		n := strings.TrimPrefix(path.Clean(hdr.Name), "./")
		rest, ok := strings.CutPrefix(n, "worlds/")
		if !ok {
			continue
		}
		folder, _, _ := strings.Cut(rest, "/")
		if folder != "" && worldFolderRule(folder) == nil {
			set[folder] = true
		}
	}
	return set, nil
}

// worldGroup is a world's folders in a set: the world, and on Java its
// other dimensions'.
func worldGroup(set map[string]bool, world string, java bool) []string {
	out := []string{world}
	if java {
		for _, d := range javaDimensions {
			if set[world+d] {
				out = append(out, world+d)
			}
		}
	}
	return out
}

// BackupWorlds lists the worlds in a backup (a Java world's other
// dimensions are part of it, not listed apart).
func (s *Server) BackupWorlds(name string) ([]string, error) {
	set, err := s.backupWorldSet(name)
	if err != nil {
		return nil, err
	}
	java := s.isJava()
	var out []string
	for f := range set {
		if java {
			dim := false
			for _, d := range javaDimensions {
				if base, ok := strings.CutSuffix(f, d); ok && set[base] {
					dim = true
				}
			}
			if dim {
				continue
			}
		}
		out = append(out, f)
	}
	sort.Strings(out)
	return out, nil
}

// RestoreWorld puts one world back as it is in a backup. It returns
// straight away; the console says how it went.
func (s *Server) RestoreWorld(name, world string) error {
	set, err := s.backupWorldSet(name)
	if err != nil {
		return err
	}
	worlds, _ := s.BackupWorlds(name)
	found := false
	for _, w := range worlds {
		found = found || w == world
	}
	if !found {
		return errors.New("that backup doesn't have that world")
	}
	group := worldGroup(set, world, s.isJava())
	if err := s.EditBlocked(); err != nil {
		return err
	}
	if !s.m.beginWork() {
		return errShuttingDown
	}
	s.filesMu.Lock()
	current, _, _ := s.currentWorld()
	s.filesMu.Unlock()
	s.mu.Lock()
	if s.state == StateImporting || s.busy {
		s.mu.Unlock()
		s.m.work.Done()
		if s.state == StateImporting {
			return ErrImporting
		}
		return errBusy
	}
	s.busy = true
	wasRunning := s.cmd != nil || s.restart != nil
	playing := wasRunning && current == world
	s.mu.Unlock()

	go func() {
		defer s.m.work.Done()
		s.backupMu.Lock()
		defer s.backupMu.Unlock()
		if playing {
			s.say("Stopping the server to put back the world it's playing…")
			if done := s.stop(true); done != nil {
				<-done
			}
			s.mu.Lock()
			s.state = StateRestoring
			s.message = "Putting back " + world + "…"
			s.mu.Unlock()
		}
		err := s.restoreWorld(name, group)
		s.mu.Lock()
		s.busy = false
		if err != nil {
			s.backupErr = "Putting back " + world + " failed: " + err.Error()
			s.addLineLocked("[Panel] " + s.backupErr)
			s.m.log.Error("world restore failed", "id", s.meta.ID, "backup", name, "world", world, "error", err)
		} else {
			s.backupErr = ""
			s.playedSinceBackup = true
			s.addLineLocked("[Panel] Put back the world " + world + " from " + name + ". The rest of the server is as it was.")
			s.m.log.Info("world restored", "id", s.meta.ID, "backup", name, "world", world)
		}
		startAgain := false
		if playing {
			s.message = ""
			s.state = StateStopped
			startAgain = s.meta.AutoStart
		}
		s.mu.Unlock()
		if startAgain {
			_ = s.Start()
		}
	}()
	return nil
}

// restoreWorld swaps a world's folders for the backup's. Caller holds
// backupMu, and has stopped the server if it plays that world.
func (s *Server) restoreWorld(name string, group []string) error {
	ctx := s.m.ctx
	sd := s.m.serversDir()
	staging := filepath.Join(sd, ".restore-wnew-"+s.meta.ID)
	old := filepath.Join(sd, ".restore-world-"+s.meta.ID)
	_ = os.RemoveAll(staging)
	_ = os.RemoveAll(old)
	defer os.RemoveAll(staging)

	p, err := s.BackupPath(name)
	if err != nil {
		return err
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	s.say("Unpacking " + group[0] + " from " + name + "…")
	keep := func(n string) bool {
		for _, g := range group {
			if n == "worlds/"+g || strings.HasPrefix(n, "worlds/"+g+"/") {
				return true
			}
		}
		return false
	}
	err = extractTarGzOnly(ctx, f, staging, keep)
	f.Close()
	if err != nil {
		return fmt.Errorf("couldn't read the backup: %w", err)
	}

	// Back up the world as it is now, so this can be undone.
	backupDir := s.m.backupsDir(s.meta.ID)
	snap, err := os.MkdirTemp(backupDir, ".snapshot-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(snap)
	had := false
	for _, g := range group {
		if fi, err := os.Stat(s.worldDir(g)); err == nil && fi.IsDir() {
			had = true
			if err := copyTree(ctx, s.worldDir(g), filepath.Join(snap, "worlds", g), 0, nil); err != nil {
				return fmt.Errorf("couldn't back up %s first, so nothing was changed: %w", g, err)
			}
		}
	}
	if had {
		safety, err := archiveSnapshot(ctx, snap, backupDir, s.meta.ID, "before-restore")
		if err != nil {
			return fmt.Errorf("couldn't back up %s first, so nothing was changed: %w", group[0], err)
		}
		s.forgetLastBackup()
		s.m.backupMade()
		s.say("Saved " + group[0] + " as it was in " + filepath.Base(safety) + ", in case you want it back.")
	}

	if err := os.MkdirAll(filepath.Join(s.dir(), "worlds"), 0o755); err != nil {
		return err
	}
	var items []string
	for _, g := range group {
		if _, err := os.Stat(filepath.Join(staging, "worlds", g)); err == nil {
			items = append(items, g)
		}
	}
	if len(items) == 0 {
		return errors.New("the backup's world is empty")
	}
	s.filesMu.Lock()
	err = swapIn(filepath.Join(staging, "worlds"), filepath.Join(s.dir(), "worlds"), old, items)
	s.filesMu.Unlock()
	if err != nil {
		return err
	}
	_ = os.RemoveAll(old)
	pruneBackups(backupDir, s.meta.ID, "before-restore", restoreBackups)
	return nil
}
