package servers

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ScottSchneck/block-party/internal/notify"
)

// Backups
//
// A backup is a .tar.gz in <data>/backups/<id>. Scheduled, manual and
// before-restore backups hold the worlds and the settings files (the files an
// update keeps); before-update backups hold the whole server folder, so
// restoring one also rolls back the update.
//
// A running server is backed up without stopping it, the way Mojang
// documents: "save hold" pauses saving, "save query" lists the files and how
// much of each to copy, and "save resume" carries on.

// BackupPlan is when a server is backed up on its own.
type BackupPlan struct {
	Every string `json:"every"` // off, 1h, 3h, 6h, 12h or daily
	At    string `json:"at"`    // HH:MM, for daily
	Keep  int    `json:"keep"`  // scheduled backups kept
}

// DefaultBackupPlan applies to servers whose plan hasn't been changed.
var DefaultBackupPlan = BackupPlan{Every: "daily", At: "04:00", Keep: 7}

var planIntervals = map[string]time.Duration{
	"1h": time.Hour, "3h": 3 * time.Hour, "6h": 6 * time.Hour, "12h": 12 * time.Hour,
}

var reClock = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)

func (p BackupPlan) validate() error {
	if _, ok := planIntervals[p.Every]; !ok && p.Every != "daily" && p.Every != "off" {
		return errors.New("pick how often to back up")
	}
	if p.Every == "daily" && !reClock.MatchString(p.At) {
		return errors.New("give the time as HH:MM, for example 04:00")
	}
	if p.Keep < 1 || p.Keep > 100 {
		return errors.New("keep between 1 and 100 backups")
	}
	return nil
}

// due reports whether a scheduled backup is due at now, given when the last
// one was made (or skipped).
func (p BackupPlan) due(last, now time.Time) bool {
	if d, ok := planIntervals[p.Every]; ok {
		return now.Sub(last) >= d-30*time.Second
	}
	if p.Every != "daily" {
		return false
	}
	mm := reClock.FindStringSubmatch(p.At)
	if mm == nil {
		return false
	}
	h, _ := strconv.Atoi(mm[1])
	mi, _ := strconv.Atoi(mm[2])
	at := time.Date(now.Year(), now.Month(), now.Day(), h, mi, 0, 0, now.Location())
	if now.Before(at) {
		at = at.AddDate(0, 0, -1)
	}
	return last.Before(at)
}

// Backup is one backup file.
type Backup struct {
	Name string    `json:"name"`
	Kind string    `json:"kind"` // scheduled, manual, before-update, before-restore, before-delete, final
	Time time.Time `json:"time"`
	Size int64     `json:"size"`
	Full bool      `json:"full"` // holds the server files too
}

// BackupsView is the Backups tab.
type BackupsView struct {
	Plan      BackupPlan `json:"plan"`
	Backups   []Backup   `json:"backups"`
	Total     int64      `json:"total"`
	Running   bool       `json:"running"` // a backup or restore is in progress
	LastError string     `json:"lastError,omitempty"`
	TimeZone  string     `json:"timeZone"`
	Now       string     `json:"now"` // the panel's clock, HH:MM
}

const (
	restoreBackups = 3 // before-restore backups kept
	deleteBackups  = 5 // before-delete backups kept (made before a world is deleted)
	holdTimeout    = 60 * time.Second
)

var (
	errBackupRunning = errors.New("a backup or restore is already running for this server")
	errBadBackupName = errors.New("no such backup")
	// The file name: <id>-YYYYMMDD-HHMMSS-<kind>.tar.gz
	reBackupName = regexp.MustCompile(`^(.+)-(\d{8}-\d{6})-(scheduled|manual|before-update|before-restore|before-delete|final)\.tar\.gz$`)
)

// scheduleTick is how often the schedule is checked; tests shorten it.
var scheduleTick = time.Minute

// scheduleRetry is how long after a failed scheduled backup it's tried again.
const scheduleRetry = 15 * time.Minute

// Plan returns the server's backup plan.
func (s *Server) Plan() BackupPlan {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.planLocked()
}

func (s *Server) planLocked() BackupPlan {
	if s.meta.Backups != nil {
		return *s.meta.Backups
	}
	return DefaultBackupPlan
}

// SetPlan changes when the server is backed up.
func (s *Server) SetPlan(p BackupPlan) error {
	p.At = strings.TrimSpace(p.At)
	if p.Every != "daily" && p.At == "" {
		p.At = DefaultBackupPlan.At
	}
	if err := p.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	s.meta.Backups = &p
	s.mu.Unlock()
	return s.saveMeta()
}

// Backups lists the server's backups, newest first.
func (s *Server) Backups() BackupsView {
	s.mu.Lock()
	v := BackupsView{Plan: s.planLocked(), Running: s.backupActive, LastError: s.backupErr}
	s.mu.Unlock()
	v.Backups = []Backup{}
	now := time.Now()
	v.TimeZone = now.Location().String()
	if v.TimeZone == "Local" {
		v.TimeZone, _ = now.Zone()
	}
	v.Now = now.Format("15:04")
	entries, _ := os.ReadDir(s.m.backupsDir(s.meta.ID))
	for _, e := range entries {
		b, ok := s.parseBackup(e)
		if ok {
			v.Backups = append(v.Backups, b)
			v.Total += b.Size
		}
	}
	sort.Slice(v.Backups, func(i, j int) bool { return v.Backups[i].Time.After(v.Backups[j].Time) })
	return v
}

func (s *Server) parseBackup(e fs.DirEntry) (Backup, bool) {
	mm := reBackupName.FindStringSubmatch(e.Name())
	if mm == nil || mm[1] != s.meta.ID || !e.Type().IsRegular() {
		return Backup{}, false
	}
	info, err := e.Info()
	if err != nil {
		return Backup{}, false
	}
	// The file's time rather than the name's, which is in whatever time zone
	// the panel had when it was made.
	return Backup{Name: e.Name(), Kind: mm[3], Time: info.ModTime().UTC(), Size: info.Size(), Full: mm[3] == "before-update"}, true
}

// BackupPath returns the file for one of the server's backups.
func (s *Server) BackupPath(name string) (string, error) {
	if filepath.Base(name) != name {
		return "", errBadBackupName
	}
	mm := reBackupName.FindStringSubmatch(name)
	if mm == nil || mm[1] != s.meta.ID {
		return "", errBadBackupName
	}
	path := filepath.Join(s.m.backupsDir(s.meta.ID), name)
	if st, err := os.Lstat(path); err != nil || !st.Mode().IsRegular() {
		return "", errBadBackupName
	}
	return path, nil
}

// DeleteBackup removes one backup.
func (s *Server) DeleteBackup(name string) error {
	path, err := s.BackupPath(name)
	if err != nil {
		return err
	}
	defer s.forgetLastBackup()
	return os.Remove(path)
}

// StartBackup makes a backup now, in the background. Follow it in Backups.
func (s *Server) StartBackup() error {
	if !s.m.beginWork() {
		return errShuttingDown
	}
	if !s.backupMu.TryLock() {
		s.m.work.Done()
		return errBackupRunning
	}
	if err := s.canBackUp(); err != nil {
		s.backupMu.Unlock()
		s.m.work.Done()
		return err
	}
	s.mu.Lock()
	s.backupActive = true
	s.mu.Unlock()
	go func() {
		defer s.m.work.Done()
		defer s.backupMu.Unlock()
		s.backUp("manual", 0)
	}()
	return nil
}

// canBackUp says why a backup can't be made right now, if it can't.
func (s *Server) canBackUp() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.state == StateImporting:
		return ErrImporting
	case s.busy:
		return errBusy
	case s.cmd != nil && s.state != StateRunning, s.cmd == nil && s.restart != nil:
		return errors.New("the server is starting or stopping; try again in a moment")
	}
	return nil
}

// backUp makes a backup. Caller holds backupMu and has checked canBackUp.
// keep 0 keeps every backup of that kind.
//
// The world is first copied to a snapshot folder next to the backups (the
// LevelDB .ldb files, which never change once written, are hard-linked), and
// only then compressed. So a running server is held for as long as the copy
// takes, not the compression, and a stopped one can start again sooner.
func (s *Server) backUp(kind string, keep int) (path string, err error) {
	s.mu.Lock()
	held := s.cmd
	if held == nil {
		s.coldBackup = true // Start waits: files mustn't change mid-copy
	}
	s.backupActive = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.coldBackup, s.backupActive = false, false
		s.forgetLastBackupLocked()
		if err != nil {
			s.backupErr = "Backup failed: " + err.Error()
			s.playedSinceBackup = true // try again next time
			s.addLineLocked("[Panel] " + s.backupErr)
		} else {
			s.backupErr = ""
			s.addLineLocked("[Panel] Backup saved: " + filepath.Base(path))
		}
		s.mu.Unlock()
		if err != nil {
			s.m.log.Warn("backup failed", "id", s.meta.ID, "kind", kind, "error", err)
		} else {
			s.m.log.Info("backup saved", "id", s.meta.ID, "file", filepath.Base(path))
			s.m.backupMade()
		}
	}()

	ctx := s.m.ctx
	snap, err := s.snapshot(ctx, held)
	s.mu.Lock()
	s.coldBackup = false
	s.mu.Unlock()
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(snap)
	path, err = archiveSnapshot(ctx, snap, s.m.backupsDir(s.meta.ID), s.meta.ID, kind)
	if err != nil {
		return "", err
	}
	if keep > 0 {
		pruneBackups(s.m.backupsDir(s.meta.ID), s.meta.ID, kind, keep)
	}
	return path, nil
}

// snapshot copies the worlds and settings into a new folder in the backups
// folder. With held (the running process), saving is paused while it copies.
func (s *Server) snapshot(ctx context.Context, held *exec.Cmd) (string, error) {
	backupDir := s.m.backupsDir(s.meta.ID)
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return "", err
	}
	// Left over from a backup that was interrupted. The caller holds
	// backupMu, so no other snapshot of this server is in progress.
	if old, _ := filepath.Glob(filepath.Join(backupDir, ".snapshot-*")); len(old) > 0 {
		for _, o := range old {
			_ = os.RemoveAll(o)
		}
	}

	snap, err := os.MkdirTemp(backupDir, ".snapshot-")
	if err != nil {
		return "", err
	}
	if held == nil {
		s.say("Backing up…")
		err = copyWorld(ctx, s.dir(), snap, nil)
	} else {
		s.say("Backing up (the server keeps running)…")
		var listed map[string]int64
		if s.isHytale() {
			err = s.hytaleHoldSave(ctx, held) // saved a moment ago; copy it all
		} else if s.isJava() {
			err = s.javaHoldSave(ctx, held) // everything is saved; copy it all
		} else {
			listed, err = s.holdSave(ctx, held)
		}
		if err == nil && afterHoldHook != nil {
			err = afterHoldHook()
		}
		if err == nil {
			err = copyWorld(ctx, s.dir(), snap, listed)
		}
		// A stop, restart or crash meanwhile means the files may have been
		// saved over while they were copied. (A stopping server still has
		// its process until it exits, so check stopping too.)
		s.mu.Lock()
		same := s.cmd == held && !s.stopping
		s.mu.Unlock()
		if err == nil && !same {
			err = errors.New("the server stopped or restarted during the backup; try again")
		}
		s.resumeSave(held)
	}
	if err != nil {
		_ = os.RemoveAll(snap)
		return "", err
	}
	s.mu.Lock()
	s.playedSinceBackup = len(s.players) > 0
	s.mu.Unlock()
	return snap, nil
}

// afterHoldHook lets tests act while a running server is held.
var afterHoldHook func() error

type saveResult struct {
	files map[string]int64
}

// holdSave pauses the running server's saving and returns the files to copy
// (relative to worlds/) and how many bytes of each.
func (s *Server) holdSave(ctx context.Context, held *exec.Cmd) (map[string]int64, error) {
	ch := make(chan saveResult, 1)
	s.mu.Lock()
	if s.cmd != held {
		s.mu.Unlock()
		return nil, errors.New("the server stopped")
	}
	s.hotBackup = true
	s.saveReady = false
	s.saveList = ch
	ok := s.sendLocked("save hold")
	s.mu.Unlock()
	if !ok {
		return nil, errors.New("the server isn't taking commands")
	}
	deadline := time.After(holdTimeout)
	query := time.NewTicker(time.Second)
	defer query.Stop()
	for {
		select {
		case r := <-ch:
			return r.files, nil
		case <-query.C:
			s.mu.Lock()
			if s.cmd != held {
				s.mu.Unlock()
				return nil, errors.New("the server stopped during the backup")
			}
			s.sendLocked("save query")
			s.mu.Unlock()
		case <-deadline:
			return nil, errors.New("the server didn't get its files ready within a minute")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// resumeSave lets the server save again. It retries for a few seconds if the
// command can't be queued, because a server left on hold never saves.
func (s *Server) resumeSave(held *exec.Cmd) {
	if s.isHytale() {
		return // Hytale's saving wasn't paused
	}
	for i := 0; i < 40; i++ {
		s.mu.Lock()
		if s.cmd != held {
			s.hotBackup, s.saveList, s.saveReady, s.javaSaved = false, nil, false, nil
			s.mu.Unlock()
			return // that process is gone, and its hold with it
		}
		resume := "save resume"
		if s.isJava() {
			resume = "save-on"
		}
		if s.sendLocked(resume) {
			s.hotBackup, s.saveList, s.saveReady, s.javaSaved = false, nil, false, nil
			s.hideSaveUntil = time.Now().Add(5 * time.Second) // its replies too
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		time.Sleep(250 * time.Millisecond)
	}
	s.mu.Lock()
	s.hotBackup, s.saveList, s.saveReady, s.javaSaved = false, nil, false, nil
	s.addLineLocked("[Panel] Couldn't tell the server to carry on saving after the backup. Restart it to be safe.")
	s.mu.Unlock()
	s.m.log.Error("could not send save resume", "id", s.meta.ID)
}

var (
	// Bedrock's log prefix, e.g. "[2026-09-27 12:00:03:000 INFO] ".
	reLogPrefix   = regexp.MustCompile(`^\[\d{4}-\d{2}-\d{2}[^\]]*\]\s*`)
	saveChatter   = []string{"Saving...", "A previous save has not been completed", "Data saved. Files are now ready to be copied", "Changes to the world are resumed", "The command is already running"}
	reSavedFileOK = regexp.MustCompile(`^(.+):(\d+)$`)
)

// onSaveLineLocked handles "save hold" and "save query" output during a
// backup. It reports whether the line belongs to the backup and should stay
// out of the console.
func (s *Server) onSaveLineLocked(line string) bool {
	if !s.hotBackup && !time.Now().Before(s.hideSaveUntil) {
		return false
	}
	text := reLogPrefix.ReplaceAllString(line, "")
	if s.saveReady {
		// The line after "Data saved" is the file list. Anything else that
		// shows up in between (a player joining) is handled as usual.
		if files := parseSaveList(text); files != nil {
			s.saveReady = false
			if s.saveList != nil {
				select {
				case s.saveList <- saveResult{files}:
				default:
				}
				s.saveList = nil
			}
			return true
		}
	}
	for _, c := range saveChatter {
		if strings.HasPrefix(text, c) {
			if strings.HasPrefix(text, "Data saved.") {
				s.saveReady = true
			}
			return true
		}
	}
	return false
}

// parseSaveList reads "Bedrock level/db/000005.ldb:83, Bedrock level/level.dat:2517".
// World names may contain ", ", so pieces without a size are joined to the
// next. All files must be in one world folder, which also tells the list
// apart from ordinary log lines.
func parseSaveList(text string) map[string]int64 {
	out := map[string]int64{}
	world := ""
	cur := ""
	for _, piece := range strings.Split(strings.TrimSpace(text), ", ") {
		if cur != "" {
			cur += ", "
		}
		cur += piece
		mm := reSavedFileOK.FindStringSubmatch(cur)
		if mm == nil {
			continue
		}
		cur = ""
		n, err := strconv.ParseInt(mm[2], 10, 64)
		if err != nil {
			return nil
		}
		name := path.Clean(filepath.ToSlash(mm[1]))
		first, _, ok := strings.Cut(name, "/")
		if !ok || name == "." || first == ".." || strings.HasPrefix(name, "/") {
			return nil
		}
		if world == "" {
			world = first
		} else if first != world {
			return nil
		}
		out[name] = n
	}
	if cur != "" || len(out) == 0 {
		return nil
	}
	return out
}

// settingsFiles are the settings a world backup holds (the files an update
// keeps).
func settingsFiles() []string {
	var out []string
	for f := range keepOnUpdate {
		out = append(out, f)
	}
	out = append(out, javaSettingsFiles...)   // Java servers' other lists and settings
	out = append(out, hytaleSettingsFiles...) // and Hytale servers'
	sort.Strings(out)
	return dedupe(out)
}

func dedupe(sorted []string) []string {
	var out []string
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

var errLinkedWorld = errors.New("is a link to another folder, which backups can't follow")

// copyWorld copies worlds/ and the settings files from dir into snap. With
// listed (from "save query"), files in that world's folder are copied only as
// far as listed, and its db/ files that aren't listed are left out.
func copyWorld(ctx context.Context, dir, snap string, listed map[string]int64) error {
	for _, name := range settingsFiles() {
		src := filepath.Join(dir, name)
		if st, err := os.Lstat(src); err == nil && st.Mode().IsRegular() {
			if err := copyFile(src, filepath.Join(snap, name), st.Mode().Perm()|0o600); err != nil {
				return err
			}
		}
	}
	// Java servers' settings folders (Paper's config, plugins and theirs).
	// Bedrock servers have a config/ folder of their own, which isn't
	// settings: only a Java server's (it has paper.jar) are kept.
	// A Hytale server's worlds (universe/) and mods.
	for _, name := range hytaleDirs {
		if !fileExists(filepath.Join(dir, hytaleJar)) {
			break
		}
		src := filepath.Join(dir, name)
		if st, err := os.Lstat(src); err == nil && st.IsDir() {
			if err := copyWorldFolder(src, filepath.Join(snap, name)); err != nil {
				return err
			}
		}
	}
	for _, name := range javaSettingsDirs {
		if !fileExists(filepath.Join(dir, paperJar)) {
			break
		}
		src := filepath.Join(dir, name)
		if st, err := os.Lstat(src); err == nil && st.IsDir() {
			if err := copyWorldFolder(src, filepath.Join(snap, name)); err != nil {
				return err
			}
		}
	}
	active := ""
	for name := range listed {
		active, _, _ = strings.Cut(name, "/")
		break
	}
	worlds := filepath.Join(dir, "worlds")
	st, err := os.Lstat(worlds)
	if os.IsNotExist(err) {
		if listed != nil {
			return errors.New("the worlds folder is missing")
		}
		return nil // never started, so no world yet
	}
	if err != nil {
		return err
	}
	if st.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("worlds %w", errLinkedWorld)
	}
	seen := 0
	err = filepath.WalkDir(worlds, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, _ := filepath.Rel(worlds, p)
		rel = filepath.ToSlash(rel)
		target := filepath.Join(snap, "worlds", filepath.FromSlash(rel))
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("worlds/%s %w", rel, errLinkedWorld)
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm() | 0o600
		first, inside, _ := strings.Cut(rel, "/")
		if listed != nil && first == active {
			n, ok := listed[rel]
			if !ok {
				if strings.HasPrefix(inside, "db/") {
					return nil // being written; not part of this save
				}
				return copyFile(p, target, mode)
			}
			seen++
			return copyPrefix(p, target, n, mode)
		}
		return copyFile(p, target, mode)
	})
	if err != nil {
		return err
	}
	if seen != len(listed) {
		return errors.New("some world files the server listed are missing")
	}
	return nil
}

// copyPrefix copies the first n bytes of src. LevelDB tables (.ldb) never
// change once written, so a whole one is hard-linked instead when it can be.
func copyPrefix(src, dst string, n int64, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if info.Size() < n {
		return fmt.Errorf("%s is shorter than the server said", filepath.Base(src))
	}
	if strings.HasSuffix(src, ".ldb") && info.Size() == n {
		if os.Link(src, dst) == nil {
			return nil
		}
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.CopyN(out, in, n); err != nil {
		out.Close()
		return fmt.Errorf("couldn't copy %s: %w", filepath.Base(src), err)
	}
	return out.Close()
}

// archiveSnapshot writes a snapshot folder to a new .tar.gz.
func archiveSnapshot(ctx context.Context, snap, backupDir, id, kind string) (string, error) {
	path := newBackupName(backupDir, id, kind)
	f, err := os.Create(path + ".partial")
	if err != nil {
		return "", err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	werr := filepath.WalkDir(snap, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(snap, p)
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		info, err := in.Stat()
		if err != nil {
			return err
		}
		hdr := &tar.Header{Name: filepath.ToSlash(rel), Mode: int64(info.Mode().Perm()), Size: info.Size(), ModTime: info.ModTime(), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err = io.Copy(tw, in)
		return err
	})
	for _, c := range []io.Closer{tw, gz, f} {
		if err := c.Close(); err != nil && werr == nil {
			werr = err
		}
	}
	if werr != nil {
		os.Remove(path + ".partial")
		return "", werr
	}
	if err := os.Rename(path+".partial", path); err != nil {
		os.Remove(path + ".partial")
		return "", err
	}
	return path, nil
}

// writeWorldBackup backs up a stopped server's worlds and settings. The
// caller makes sure the server stays stopped.
func writeWorldBackup(ctx context.Context, dir, backupDir, id, kind string) (string, error) {
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return "", err
	}
	snap, err := os.MkdirTemp(backupDir, ".snapshot-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(snap)
	if err := copyWorld(ctx, dir, snap, nil); err != nil {
		return "", err
	}
	return archiveSnapshot(ctx, snap, backupDir, id, kind)
}

// newBackupName picks a file name that isn't taken.
func newBackupName(backupDir, id, kind string) string {
	t := time.Now()
	for {
		p := filepath.Join(backupDir, fmt.Sprintf("%s-%s-%s.tar.gz", id, t.Format("20060102-150405"), kind))
		if _, err := os.Stat(p); os.IsNotExist(err) {
			if _, err := os.Stat(p + ".partial"); os.IsNotExist(err) {
				return p
			}
		}
		t = t.Add(time.Second)
	}
}

// ---- restore ----

// Restore puts a backup back: the server is stopped, the current world and
// settings are backed up first, the backup's files replace the current ones,
// and the server starts again if it was running. It returns straight away.
func (s *Server) Restore(name string) error {
	path, err := s.BackupPath(name)
	if err != nil {
		return err
	}
	// Open it now, so the file can't be pruned or deleted from under us.
	archive, err := os.Open(path)
	if err != nil {
		return err
	}
	if strings.HasSuffix(name, "-before-delete.tar.gz") {
		// Just a deleted world: it's put back next to the others.
		defer archive.Close()
		if err := s.EditBlocked(); err != nil {
			return err
		}
		return s.restoreDeletedWorld(archive)
	}
	if !s.m.beginWork() {
		archive.Close()
		return errShuttingDown
	}
	s.mu.Lock()
	if s.state == StateImporting || s.busy {
		s.mu.Unlock()
		archive.Close()
		s.m.work.Done()
		if s.state == StateImporting {
			return ErrImporting
		}
		return errBusy
	}
	s.busy = true
	wasRunning := s.cmd != nil || s.restart != nil
	s.mu.Unlock()

	go func() {
		defer s.m.work.Done()
		defer archive.Close()
		s.backupMu.Lock() // wait for a backup in progress
		defer s.backupMu.Unlock()
		if done := s.stop(true); done != nil {
			<-done
		}
		s.mu.Lock()
		s.state = StateRestoring
		s.message = "Restoring a backup…"
		s.backupActive = true
		s.mu.Unlock()

		err := s.restore(archive, name)

		s.mu.Lock()
		s.busy, s.backupActive = false, false
		s.message = ""
		if err != nil {
			s.backupErr = "Restore failed: " + err.Error()
			s.addLineLocked("[Panel] " + s.backupErr)
			s.m.log.Error("restore failed", "id", s.meta.ID, "backup", name, "error", err)
		} else {
			s.backupErr = ""
			s.playedSinceBackup = true
			s.addLineLocked("[Panel] Restored " + name + ".")
			s.m.log.Info("backup restored", "id", s.meta.ID, "backup", name)
		}
		if _, statErr := os.Stat(s.binary()); statErr != nil {
			s.state = StateError
			s.message = "The server files are missing. Use Update to download them again."
		} else if b, rerr := os.ReadFile(s.incompleteMarker()); rerr == nil {
			s.state = StateError
			s.message = strings.TrimSpace(string(b))
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

func (s *Server) restore(archive *os.File, name string) error {
	ctx := s.m.ctx
	sd := s.m.serversDir()
	staging := filepath.Join(sd, ".restore-new-"+s.meta.ID)
	old := filepath.Join(sd, ".restore-old-"+s.meta.ID)
	_ = os.RemoveAll(staging)
	_ = os.RemoveAll(old)
	defer os.RemoveAll(staging)

	s.say("Unpacking " + name + "…")
	if err := extractTarGz(ctx, archive, staging); err != nil {
		return fmt.Errorf("couldn't read the backup: %w", err)
	}
	full := false
	if _, err := os.Stat(filepath.Join(staging, "bedrock_server")); err == nil {
		full = true
	}
	if _, err := os.Stat(filepath.Join(staging, paperJar)); err == nil {
		full = true // a Java server's backup from before an update
	}
	if _, err := os.Stat(filepath.Join(staging, hytaleJar)); err == nil {
		full = true // a Hytale server's whole folder
	}

	// Back up what's there now, so the restore can be undone.
	s.say("Backing up the current world first, so this can be undone…")
	safety, err := writeWorldBackup(ctx, s.dir(), s.m.backupsDir(s.meta.ID), s.meta.ID, "before-restore")
	if err != nil {
		return fmt.Errorf("couldn't back up the current world first, so nothing was changed: %w", err)
	}
	s.forgetLastBackup()
	s.say("Saved the current world as " + filepath.Base(safety) + ".")

	// What to swap: everything in a full backup, otherwise the world and
	// settings files it holds.
	var items []string
	entries, err := os.ReadDir(staging)
	if err != nil {
		return err
	}
	allowed := map[string]bool{"worlds": true}
	for _, f := range settingsFiles() {
		allowed[f] = true
	}
	if s.isJava() {
		for _, d := range javaSettingsDirs {
			allowed[d] = true
		}
	}
	if s.isHytale() {
		for _, d := range hytaleDirs {
			allowed[d] = true
		}
	}
	for _, e := range entries {
		if full || allowed[e.Name()] {
			items = append(items, e.Name())
		}
	}
	if len(items) == 0 {
		return errors.New("the backup is empty")
	}
	// The Players and Settings pages can't change these files meanwhile.
	s.filesMu.Lock()
	err = swapIn(staging, s.dir(), old, items)
	if err == nil {
		s.mu.Lock()
		if perr := s.applyOwnProperties(); perr != nil {
			s.addLineLocked("[Panel] Couldn't set the panel's port settings: " + perr.Error())
		}
		s.mu.Unlock()
	}
	s.filesMu.Unlock()
	if err != nil {
		return err
	}
	_ = os.RemoveAll(old)
	s.notePluginFiles() // the backup may hold older console plugins
	if full {
		_ = os.Remove(s.incompleteMarker())
		_ = os.Chmod(s.binary(), 0o755)
		s.mu.Lock()
		s.meta.Version = "" // learned again when it starts
		s.mu.Unlock()
		if err := s.saveMeta(); err != nil {
			return err
		}
	}
	pruneBackups(s.m.backupsDir(s.meta.ID), s.meta.ID, "before-restore", restoreBackups)
	return nil
}

// swapIn moves each item from staging into dir, moving what was there to old
// first. If a move fails, everything moved so far is put back. old/.swap
// records the plan and old/.done that it finished, so Load can undo a swap
// that was cut short (see recoverRestore).
func swapIn(staging, dir, old string, items []string) error {
	if err := os.MkdirAll(old, 0o755); err != nil {
		return err
	}
	var plan strings.Builder
	existed := map[string]bool{}
	for _, item := range items {
		_, err := os.Lstat(filepath.Join(dir, item))
		existed[item] = err == nil
		flag := "0"
		if existed[item] {
			flag = "1"
		}
		plan.WriteString(flag + " " + item + "\n")
	}
	if err := writeSynced(filepath.Join(old, ".swap"), plan.String()); err != nil {
		return err
	}
	var done []string
	undo := func() {
		for i := len(done) - 1; i >= 0; i-- {
			item := done[i]
			_ = os.RemoveAll(filepath.Join(dir, item))
			if existed[item] {
				_ = os.Rename(filepath.Join(old, item), filepath.Join(dir, item))
			}
		}
	}
	for _, item := range items {
		cur := filepath.Join(dir, item)
		if existed[item] {
			if err := os.Rename(cur, filepath.Join(old, item)); err != nil {
				undo()
				return fmt.Errorf("couldn't move %s aside: %w", item, err)
			}
		}
		if err := os.Rename(filepath.Join(staging, item), cur); err != nil {
			if existed[item] {
				_ = os.Rename(filepath.Join(old, item), cur)
			}
			undo()
			return fmt.Errorf("couldn't put %s back: %w", item, err)
		}
		done = append(done, item)
	}
	return writeSynced(filepath.Join(old, ".done"), "")
}

func writeSynced(path, text string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// recoverRestore deals with a restore the panel was stopped in the middle
// of. If the swap finished, the old files are just removed. Otherwise it's
// undone, so the server is as it was before (a mix of old and restored files
// could give a settings file that names a world that isn't there).
func recoverRestore(old, dir string) bool {
	defer os.RemoveAll(old)
	if _, err := os.Stat(filepath.Join(old, ".done")); err == nil {
		return false
	}
	b, err := os.ReadFile(filepath.Join(old, ".swap"))
	if err != nil {
		return false // interrupted before anything moved
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		flag, item, ok := strings.Cut(line, " ")
		if !ok || item == "" || strings.ContainsAny(item, "/\\") || item == "." || item == ".." {
			continue
		}
		target := filepath.Join(dir, item)
		if flag == "1" {
			if _, err := os.Lstat(filepath.Join(old, item)); err == nil {
				_ = os.RemoveAll(target)
				_ = os.Rename(filepath.Join(old, item), target)
			}
			// Otherwise it was never moved aside: it's still the original.
		} else {
			_ = os.RemoveAll(target) // wasn't there before the restore
		}
	}
	return true
}

// extractTarGz unpacks a backup into dir, refusing anything that would land
// outside it. Only folders and plain files are unpacked.
func extractTarGz(ctx context.Context, r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(hdr.Name))
		if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("the backup has an unsafe path: %s", hdr.Name)
		}
		if base := filepath.Base(name); name == base && (base == metaFile || base == ".update-incomplete") {
			continue // the panel's own records stay as they are
		}
		target := filepath.Join(dir, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode).Perm()|0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
			_ = os.Chtimes(target, hdr.ModTime, hdr.ModTime)
		}
	}
}

// backupMade passes on that a backup was saved.
func (m *Manager) backupMade() {
	if m.opts.BackupMade != nil {
		m.opts.BackupMade()
	}
}

// BackupFile is one backup on disk, for copying off-site.
type BackupFile struct {
	Server string
	Name   string
	Path   string
	Time   time.Time
	Size   int64
}

// BackupFiles lists every backup, of servers here and deleted ones.
func (m *Manager) BackupFiles() []BackupFile {
	var out []BackupFile
	root := filepath.Join(m.opts.DataDir, "backups")
	dirs, _ := os.ReadDir(root)
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(root, d.Name()))
		for _, f := range files {
			mm := reBackupName.FindStringSubmatch(f.Name())
			if mm == nil || mm[1] != d.Name() || !f.Type().IsRegular() {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			out = append(out, BackupFile{Server: d.Name(), Name: f.Name(), Path: filepath.Join(root, d.Name(), f.Name()), Time: info.ModTime(), Size: info.Size()})
		}
	}
	return out
}

// BackupNote describes a server for its off-site backups, so it can be
// brought back from them: the same as a deleted server's note.
func (m *Manager) BackupNote(id string) []byte {
	if s, ok := m.Get(id); ok {
		s.mu.Lock()
		meta := s.meta
		s.mu.Unlock()
		meta.AutoStart, meta.Outside, meta.OpenLate, meta.ClosedForHours = false, false, time.Time{}, false
		b, _ := json.MarshalIndent(deletedNote{Meta: meta}, "", "  ")
		return b
	}
	b, err := os.ReadFile(filepath.Join(m.backupsDir(id), deletedFile))
	if err != nil {
		return nil
	}
	return b
}

// ---- schedule ----

// RunBackupSchedule makes scheduled backups until the panel shuts down.
func (m *Manager) RunBackupSchedule() {
	go func() {
		t := time.NewTicker(scheduleTick)
		defer t.Stop()
		for {
			select {
			case <-m.ctx.Done():
				return
			case now := <-t.C:
				for _, s := range m.all() {
					if m.ctx.Err() != nil {
						return
					}
					s.scheduledBackup(now)
				}
				// Overnight upkeep after the backups, which it would
				// otherwise interrupt (an update backs up first anyway).
				plan := m.NightlyPlan()
				latest, _ := m.LatestRelease()
				for _, s := range m.all() {
					if m.ctx.Err() != nil {
						return
					}
					s.nightly(now, plan, latest)
				}
			}
		}
	}()
}

// scheduledBackup makes a scheduled backup if one is due. A server nobody
// has played on since its last backup is skipped, so quiet servers don't use
// up the kept copies with identical backups.
func (s *Server) scheduledBackup(now time.Time) {
	s.mu.Lock()
	plan := s.planLocked()
	due := plan.due(s.meta.LastScheduledBackup, now) && !now.Before(s.retryScheduledAt)
	s.mu.Unlock()
	if !due {
		return
	}
	if err := s.canBackUp(); err != nil {
		return // try again next tick
	}
	if !s.m.beginWork() {
		return
	}
	defer s.m.work.Done()
	if !s.backupMu.TryLock() {
		return
	}
	defer s.backupMu.Unlock()
	if err := s.canBackUp(); err != nil {
		return
	}
	s.mu.Lock()
	idle := !s.playedSinceBackup && len(s.players) == 0
	s.mu.Unlock()
	if idle {
		s.m.log.Debug("scheduled backup skipped: nobody has played since the last one", "id", s.meta.ID)
	} else if _, err := s.backUp("scheduled", plan.Keep); err != nil {
		// Told once until a backup works again, not every retry.
		s.mu.Lock()
		first := !s.backupFailNoted
		s.backupFailNoted = true
		s.mu.Unlock()
		if first {
			s.notify(notify.BackupFailed, "Backup of "+s.Status().Name+" failed",
				err.Error()+" The panel keeps trying every 15 minutes and will only tell you again once a backup has worked.", "")
		}
		// Try again in a while rather than wait a whole period, but not
		// every minute (a full disk would fill the log).
		s.mu.Lock()
		s.retryScheduledAt = now.Add(scheduleRetry)
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.meta.LastScheduledBackup = now
	s.backupFailNoted = false
	s.mu.Unlock()
	if err := s.saveMeta(); err != nil {
		s.m.log.Warn("could not save server details", "id", s.meta.ID, "error", err)
	}
}
