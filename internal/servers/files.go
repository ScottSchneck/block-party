package servers

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// The file manager: browse, edit, upload, download, rename and delete the
// files in a server's folder, like Crafty's. Everything goes through an
// os.Root for the server's folder, so no path (or symlink) can reach
// outside it.
//
// Rules on top of that:
//   - The panel's own record of the server (.blockheads.json) is hidden.
//   - Only the owner may add, change, rename or delete code: plugins and
//     mods folders, .jar files, and the server programs. Plugins run their
//     own code on the computer, which is why installing them is the
//     owner's job too.
//   - Only the owner may delete a whole folder (a world, say).

// FileEntry is one file or folder in a listing.
type FileEntry struct {
	Name    string    `json:"name"`
	Dir     bool      `json:"dir,omitempty"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
	// Editable: small enough and text, so the page can open it to edit.
	Editable bool `json:"editable,omitempty"`
	// Locked: only the owner may change it (code).
	Locked bool `json:"locked,omitempty"`
}

// FileText is a text file opened to edit.
type FileText struct {
	Path    string    `json:"path"`
	Text    string    `json:"text"`
	ModTime time.Time `json:"modTime"`
}

const (
	maxEditBytes = 2 << 20
	// MaxUploadBytes is the most one file manager upload may hold.
	MaxUploadBytes = 2 << 30
)

var (
	// ErrFileNotFound is a path that isn't there.
	ErrFileNotFound = errors.New("there's no such file or folder")
	// ErrFileNotAllowed is a change the person may not make.
	ErrFileNotAllowed = errors.New("only the owner can change that")
	errBadPath        = errors.New("that isn't a usable name")
)

// cleanRel turns a path from the page ("plugins/x.yml", "" for the top)
// into one inside the server's folder, or fails.
func cleanRel(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	if strings.ContainsRune(p, 0) || len(p) > 1024 {
		return "", errBadPath
	}
	p = strings.Trim(p, "/")
	if p == "" {
		return ".", nil
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", errBadPath
		}
	}
	c := path.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") || strings.HasPrefix(c, "/") {
		return "", errBadPath
	}
	return c, nil
}

// hiddenFile: the panel's own records, never shown or touched.
func hiddenFile(rel string) bool {
	base := path.Base(rel)
	return base == metaFile || base == metaFile+".tmp" || base == modpackRecordFile || base == modpackRecordFile+".tmp"
}

// codeFile: files and folders that run code on the computer: the server
// programs, anything with a program's extension, the plugins and mods
// folders and what's straight inside them (plugin settings a level down,
// like plugins/LuckPerms/config.yml, are fine), and Java's libraries.
func codeFile(rel string) bool {
	if rel == "." {
		return false
	}
	parts := strings.Split(rel, "/")
	switch path.Base(rel) {
	case "bedrock_server", paperJar, hytaleJar, "run.sh":
		if len(parts) == 1 {
			return true
		}
	}
	if isCodeName(rel) {
		return true
	}
	switch strings.ToLower(parts[0]) {
	case "plugins", "mods":
		return len(parts) <= 2
	case "libraries", "versions", "bundler":
		return true
	}
	return false
}

func isCodeName(rel string) bool {
	switch strings.ToLower(path.Ext(rel)) {
	case ".jar", ".so", ".sh", ".exe", ".dll", ".py", ".class", ".bat", ".cmd":
		return true
	}
	return false
}

func (s *Server) openFiles() (*os.Root, error) {
	return os.OpenRoot(s.dir())
}

// ListFiles lists a folder: folders first, then files, by name.
func (s *Server) ListFiles(dir string) ([]FileEntry, error) {
	rel, err := cleanRel(dir)
	if err != nil {
		return nil, err
	}
	root, err := s.openFiles()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(rel)
	if err != nil {
		return nil, notFound(err)
	}
	defer f.Close()
	infos, err := f.Readdir(-1)
	if err != nil {
		return nil, ErrFileNotFound
	}
	out := make([]FileEntry, 0, len(infos))
	for _, fi := range infos {
		name := fi.Name()
		child := path.Join(rel, name)
		if hiddenFile(child) {
			continue
		}
		e := FileEntry{Name: name, Dir: fi.IsDir(), Size: fi.Size(), ModTime: fi.ModTime(), Locked: codeFile(child)}
		if fi.Mode()&fs.ModeSymlink != 0 {
			// Follow it (inside the folder only) to say what it is.
			if st, err := root.Stat(child); err == nil {
				e.Dir, e.Size = st.IsDir(), st.Size()
			}
		}
		if e.Dir {
			e.Size = 0
		} else if e.Size <= maxEditBytes {
			e.Editable = looksLikeText(root, child)
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// looksLikeText: no NUL bytes and valid-looking at the start.
func looksLikeText(root *os.Root, rel string) bool {
	if isCodeName(rel) {
		return false
	}
	f, err := root.Open(rel)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 8192)
	n, _ := io.ReadFull(f, buf)
	return !bytes.ContainsRune(buf[:n], 0)
}

func notFound(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return ErrFileNotFound
	}
	if strings.Contains(err.Error(), "escapes from parent") || strings.Contains(err.Error(), "path escapes") {
		return errBadPath
	}
	return err
}

// ReadText opens a text file to edit.
func (s *Server) ReadText(p string) (FileText, error) {
	rel, err := cleanRel(p)
	if err != nil || rel == "." || hiddenFile(rel) {
		return FileText{}, errBadPath
	}
	root, err := s.openFiles()
	if err != nil {
		return FileText{}, err
	}
	defer root.Close()
	st, err := root.Stat(rel)
	if err != nil {
		return FileText{}, notFound(err)
	}
	if st.IsDir() {
		return FileText{}, errors.New("that's a folder")
	}
	if st.Size() > maxEditBytes {
		return FileText{}, errors.New("that file is too big to edit here (over 2 MB); download it instead")
	}
	b, err := root.ReadFile(rel)
	if err != nil {
		return FileText{}, notFound(err)
	}
	if bytes.ContainsRune(b, 0) {
		return FileText{}, errors.New("that isn't a text file; download it instead")
	}
	return FileText{Path: rel, Text: string(b), ModTime: st.ModTime()}, nil
}

// may checks that who may change rel.
func (s *Server) may(rel string, owner bool) error {
	if rel == "." || hiddenFile(rel) {
		return errBadPath
	}
	if err := s.EditBlocked(); err != nil {
		return err
	}
	if !owner && codeFile(rel) {
		return fmt.Errorf("%w: %s runs code on the computer", ErrFileNotAllowed, path.Base(rel))
	}
	return nil
}

// ErrFileChanged means a file changed since it was opened.
var ErrFileChanged = errors.New("the file changed since you opened it (the server may have saved it); open it again")

// WriteText saves a text file. With since set, it refuses if the file
// changed after that.
func (s *Server) WriteText(p, text string, since time.Time, owner bool) error {
	rel, err := cleanRel(p)
	if err != nil {
		return err
	}
	if err := s.may(rel, owner); err != nil {
		return err
	}
	if len(text) > maxEditBytes {
		return errors.New("that's too much text (over 2 MB)")
	}
	root, err := s.openFiles()
	if err != nil {
		return err
	}
	defer root.Close()
	mode := fs.FileMode(0o644)
	if st, err := root.Stat(rel); err == nil {
		if st.IsDir() {
			return errors.New("that's a folder")
		}
		if !since.IsZero() && !st.ModTime().Equal(since) {
			return ErrFileChanged
		}
		mode = st.Mode().Perm()
	}
	tmp := path.Join(path.Dir(rel), ".bp-edit-"+newTaskID())
	if err := root.WriteFile(tmp, []byte(text), mode); err != nil {
		return notFound(err)
	}
	if err := root.Rename(tmp, rel); err != nil {
		_ = root.Remove(tmp)
		return notFound(err)
	}
	return nil
}

// OpenDownload opens a file to download. The caller closes it.
func (s *Server) OpenDownload(p string) (*os.File, fs.FileInfo, error) {
	rel, err := cleanRel(p)
	if err != nil || rel == "." || hiddenFile(rel) {
		return nil, nil, errBadPath
	}
	root, err := s.openFiles()
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	f, err := root.Open(rel)
	if err != nil {
		return nil, nil, notFound(err)
	}
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		f.Close()
		return nil, nil, errors.New("only files can be downloaded")
	}
	return f, st, nil
}

// SaveUpload writes an uploaded file into dir (replacing one of the same
// name).
func (s *Server) SaveUpload(dir, name string, r io.Reader, owner bool) error {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == "/" || name == ".." || strings.HasPrefix(name, ".bp-") {
		return errBadPath
	}
	rel, err := cleanRel(path.Join(dir, name))
	if err != nil {
		return err
	}
	if err := s.may(rel, owner); err != nil {
		return err
	}
	root, err := s.openFiles()
	if err != nil {
		return err
	}
	defer root.Close()
	if st, err := root.Stat(rel); err == nil && st.IsDir() {
		return fmt.Errorf("there's a folder called %s here", name)
	}
	tmp := path.Join(path.Dir(rel), ".bp-upload-"+newTaskID())
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return notFound(err)
	}
	n, err := io.Copy(f, io.LimitReader(r, MaxUploadBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > MaxUploadBytes {
		err = errors.New("that file is too big (over 2 GB)")
	}
	if err != nil {
		_ = root.Remove(tmp)
		return err
	}
	if err := root.Rename(tmp, rel); err != nil {
		_ = root.Remove(tmp)
		return notFound(err)
	}
	return nil
}

// MakeFolder makes a folder.
func (s *Server) MakeFolder(p string, owner bool) error {
	rel, err := cleanRel(p)
	if err != nil {
		return err
	}
	if err := s.may(rel, owner); err != nil {
		return err
	}
	root, err := s.openFiles()
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Mkdir(rel, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return errors.New("there's already something with that name")
		}
		return notFound(err)
	}
	return nil
}

// RenameFile renames or moves a file or folder (within the server's folder).
func (s *Server) RenameFile(from, to string, owner bool) error {
	a, err := cleanRel(from)
	if err != nil {
		return err
	}
	b, err := cleanRel(to)
	if err != nil {
		return err
	}
	if err := s.may(a, owner); err != nil {
		return err
	}
	if err := s.may(b, owner); err != nil {
		return err
	}
	if a == b {
		return nil
	}
	if strings.HasPrefix(b+"/", a+"/") {
		return errors.New("a folder can't go inside itself")
	}
	root, err := s.openFiles()
	if err != nil {
		return err
	}
	defer root.Close()
	if _, err := root.Lstat(b); err == nil {
		return errors.New("there's already something with that name")
	}
	if !owner {
		// Moving a folder could carry code with it into a place that runs it.
		if st, err := root.Lstat(a); err == nil && st.IsDir() {
			return fmt.Errorf("%w: only the owner can move or rename folders", ErrFileNotAllowed)
		}
	}
	if err := root.Rename(a, b); err != nil {
		return notFound(err)
	}
	return nil
}

// DeleteFile deletes a file, or (for the owner) a folder and everything in
// it.
func (s *Server) DeleteFile(p string, owner bool) error {
	rel, err := cleanRel(p)
	if err != nil {
		return err
	}
	if err := s.may(rel, owner); err != nil {
		return err
	}
	root, err := s.openFiles()
	if err != nil {
		return err
	}
	defer root.Close()
	st, err := root.Lstat(rel)
	if err != nil {
		return notFound(err)
	}
	if st.IsDir() {
		if !owner {
			return fmt.Errorf("%w: only the owner can delete folders", ErrFileNotAllowed)
		}
		return notFound(root.RemoveAll(rel))
	}
	return notFound(root.Remove(rel))
}
