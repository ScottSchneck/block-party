package servers

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Modpacks: a Java server made from a Modrinth modpack (.mrpack), found on
// Modrinth or uploaded. The pack says which Minecraft version and mod loader
// it needs; the panel installs those, downloads the pack's mods (checking
// each one's checksum, only from the hosts Modrinth's format allows) and
// copies in its own files (overrides/, then server-overrides/).

// ModpackResult is a modpack found by a search.
type ModpackResult struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Author      string `json:"author"`
	Description string `json:"description"`
	Icon        string `json:"icon,omitempty"`
	Downloads   int64  `json:"downloads"`
	Page        string `json:"page,omitempty"`
	// Latest is the newest version's Minecraft versions and loaders.
	Versions []string `json:"versions,omitempty"`
}

// SearchModpacks looks for modpacks on Modrinth that run on a server, with
// a loader the panel runs (not Quilt only).
func (m *Manager) SearchModpacks(ctx context.Context, query string) ([]ModpackResult, error) {
	q := url.Values{"limit": {"20"}, "facets": {`[["project_type:modpack"],["server_side:required","server_side:optional"],["categories:fabric","categories:forge","categories:neoforge"]]`}}
	if query = strings.TrimSpace(query); query != "" {
		q.Set("query", query)
	} else {
		q.Set("index", "downloads")
	}
	var res struct {
		Hits []struct {
			ProjectID   string   `json:"project_id"`
			Slug        string   `json:"slug"`
			Title       string   `json:"title"`
			Author      string   `json:"author"`
			Description string   `json:"description"`
			IconURL     string   `json:"icon_url"`
			Downloads   int64    `json:"downloads"`
			Versions    []string `json:"versions"`
		} `json:"hits"`
	}
	if err := m.getJSON(ctx, m.opts.ModrinthAPI+"/search?"+q.Encode(), &res); err != nil {
		return nil, fmt.Errorf("couldn't search Modrinth: %w", err)
	}
	out := []ModpackResult{}
	for _, h := range res.Hits {
		if h.ProjectID == "" {
			continue
		}
		r := ModpackResult{ID: h.ProjectID, Name: h.Title, Author: h.Author, Description: h.Description, Icon: httpsOnly(h.IconURL), Downloads: h.Downloads}
		if h.Slug != "" {
			r.Page = "https://modrinth.com/modpack/" + url.PathEscape(h.Slug)
		}
		if n := len(h.Versions); n > 0 {
			r.Versions = []string{h.Versions[n-1]}
		}
		out = append(out, r)
	}
	return out, nil
}

// ModpackSource is where a new modpack server's pack comes from: a
// Modrinth project, or a .mrpack file already saved (by the caller) in the
// servers folder.
type ModpackSource struct {
	Project    string
	File       string
	CurseForge int // a CurseForge modpack's project ID
}

// CreateModpack sets up a new Java server from a modpack.
func (m *Manager) CreateModpack(name string, memoryMB int, src ModpackSource) (Status, error) {
	if src.Project != "" && !pluginIDPattern.MatchString(src.Project) {
		return Status{}, errors.New("that isn't a modpack")
	}
	if src.File != "" {
		if filepath.Dir(src.File) != m.serversDir() || !strings.HasPrefix(filepath.Base(src.File), ".upload-") {
			return Status{}, errors.New("that isn't an uploaded modpack")
		}
	}
	n := 0
	for _, set := range []bool{src.Project != "", src.File != "", src.CurseForge != 0} {
		if set {
			n++
		}
	}
	if n != 1 || src.CurseForge < 0 {
		return Status{}, errors.New("choose a modpack")
	}
	if src.CurseForge != 0 && !m.CurseForgeKeySet() {
		return Status{}, ErrNoCurseForgeKey
	}
	pack := "modrinth:" + src.Project
	switch {
	case src.File != "":
		pack = "file:" + filepath.Base(src.File)
	case src.CurseForge != 0:
		pack = fmt.Sprintf("curseforge:%d", src.CurseForge)
	}
	return m.createWith(TypeJava, name, false, memoryMB, nil, func(meta *Meta) {
		meta.PackSource = pack
		meta.KeepVersion = true
	})
}

// UploadsDir is where an uploaded modpack is saved before CreateModpack.
func (m *Manager) UploadsDir() string { return m.serversDir() }

// mrpackIndex is a modpack's modrinth.index.json.
type mrpackIndex struct {
	FormatVersion int    `json:"formatVersion"`
	Game          string `json:"game"`
	VersionID     string `json:"versionId"`
	Name          string `json:"name"`
	Files         []struct {
		Path   string            `json:"path"`
		Hashes map[string]string `json:"hashes"`
		Env    struct {
			Server string `json:"server"`
		} `json:"env"`
		Downloads []string `json:"downloads"`
		FileSize  int64    `json:"fileSize"`
	} `json:"files"`
	Dependencies map[string]string `json:"dependencies"`
}

// mrpackHosts are where a modpack's files may be downloaded from (as
// Modrinth's format says).
var mrpackHosts = map[string]bool{"cdn.modrinth.com": true, "github.com": true, "raw.githubusercontent.com": true, "gitlab.com": true}

// mrpackSchemes: https only (tests add http).
var mrpackSchemes = map[string]bool{"https": true}

// mrpackVersion is a version of a modpack on Modrinth.
type mrpackVersion struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	VersionNumber string   `json:"version_number"`
	VersionType   string   `json:"version_type"`
	Loaders       []string `json:"loaders"`
	Files         []struct {
		URL      string            `json:"url"`
		FileName string            `json:"filename"`
		Hashes   map[string]string `json:"hashes"`
		Primary  bool              `json:"primary"`
	} `json:"files"`
}

// newestMrpack is a Modrinth modpack's newest version the panel can run (a
// release over a beta).
func (m *Manager) newestMrpack(ctx context.Context, project string) (mrpackVersion, error) {
	var versions []mrpackVersion
	if err := m.getJSON(ctx, m.opts.ModrinthAPI+"/project/"+url.PathEscape(project)+"/version", &versions); err != nil {
		return mrpackVersion{}, fmt.Errorf("couldn't ask Modrinth for the modpack's versions: %w", err)
	}
	pick := -1
	for i, v := range versions { // newest first; a release if there is one
		usable := false
		for _, l := range v.Loaders {
			if l == "fabric" || l == "forge" || l == "neoforge" || l == "minecraft" {
				usable = true
			}
		}
		if !usable {
			continue
		}
		if v.VersionType == "release" {
			pick = i
			break
		}
		if pick < 0 {
			pick = i
		}
	}
	if pick < 0 {
		return mrpackVersion{}, errors.New("the modpack has no version for Fabric, Forge or NeoForge")
	}
	v := versions[pick]
	if v.ID == "" {
		v.ID = v.VersionNumber
	}
	return v, nil
}

// modpackFile gets the pack: downloads it from Modrinth, or takes the
// uploaded one, and says which version it is. The caller removes the file.
func (s *Server) modpackFile(ctx context.Context, src string) (string, string, error) {
	kind, arg, _ := strings.Cut(src, ":")
	if kind == "file" {
		p := filepath.Join(s.m.serversDir(), arg)
		if !strings.HasPrefix(arg, ".upload-") || strings.ContainsAny(arg, `/\`) || !fileExists(p) {
			return "", "", errors.New("the uploaded modpack is gone (the panel restarted?); add the server again")
		}
		return p, "", nil
	}
	v, err := s.m.newestMrpack(ctx, arg)
	if err != nil {
		return "", "", err
	}
	for _, f := range v.Files {
		if !strings.HasSuffix(strings.ToLower(f.FileName), ".mrpack") || (!f.Primary && len(v.Files) > 1) {
			continue
		}
		if len(f.Hashes["sha512"]) != 128 {
			return "", "", errors.New("Modrinth has no checked download of the modpack")
		}
		s.say("Downloading the modpack (" + v.Name + ")…")
		file, err := s.m.downloadVerified(ctx, f.URL, s.m.serversDir(), strings.ToLower(f.Hashes["sha512"]), s.say, s.m.javaUserAgent())
		return file, v.ID, err
	}
	return "", "", errors.New("the modpack's newest version has no .mrpack file")
}

// installModpack installs a server's modpack from src ("modrinth:<project>",
// "curseforge:<project>" or "file:<upload>"): a new server's, or on an
// update its newest version, over the old one (see packWriter).
func (s *Server) installModpack(ctx context.Context, src string, update bool) error {
	if id, ok := strings.CutPrefix(src, "curseforge:"); ok {
		n, err := strconv.Atoi(id)
		if err != nil {
			return errors.New("bad modpack")
		}
		return s.installCurseForgePack(ctx, n, update)
	}
	file, packVersion, err := s.modpackFile(ctx, src)
	if err != nil {
		return err
	}
	defer os.Remove(file)
	zr, err := zip.OpenReader(file)
	if err != nil {
		return errors.New("that isn't a Modrinth modpack (.mrpack)")
	}
	defer zr.Close()
	var idx mrpackIndex
	for _, f := range zr.File {
		if f.Name != "modrinth.index.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = json.NewDecoder(io.LimitReader(rc, 16<<20)).Decode(&idx)
		rc.Close()
		if err != nil {
			return fmt.Errorf("couldn't read the modpack: %w", err)
		}
	}
	if idx.Game != "minecraft" || idx.FormatVersion != 1 {
		return errors.New("that isn't a Minecraft modpack in Modrinth's format")
	}
	mc := idx.Dependencies["minecraft"]
	if !plainVersion.MatchString(mc) {
		return fmt.Errorf("the modpack is for Minecraft %q, which the panel can't run", mc)
	}
	loader, lv := LoaderVanilla, ""
	switch {
	case idx.Dependencies["fabric-loader"] != "":
		loader, lv = LoaderFabric, idx.Dependencies["fabric-loader"]
	case idx.Dependencies["neoforge"] != "":
		loader, lv = LoaderNeoForge, idx.Dependencies["neoforge"]
	case idx.Dependencies["forge"] != "":
		loader, lv = LoaderForge, idx.Dependencies["forge"]
	case idx.Dependencies["quilt-loader"] != "":
		return errors.New("the modpack needs Quilt, which the panel doesn't run (Fabric, Forge and NeoForge packs work)")
	}
	label := strings.TrimSpace(idx.Name + " " + idx.VersionID)
	s.say(fmt.Sprintf("Modpack %s: Minecraft %s with %s %s, %d files.", label, mc, LoaderName(loader), lv, len(idx.Files)))
	if err := s.setModpack(src, packVersion, label, loader, mc, lv, update); err != nil {
		return err
	}
	// The loader first (it may make folders the pack's files go in).
	if err := s.installLoader(ctx, false); err != nil {
		return err
	}
	root, err := os.OpenRoot(s.dir())
	if err != nil {
		return err
	}
	defer root.Close()
	w := s.newPackWriter(root, update)
	// The pack's own downloads.
	got := 0
	for _, f := range idx.Files {
		if f.Env.Server == "unsupported" {
			continue
		}
		rel, err := cleanRel(f.Path)
		if err != nil || rel == "." || hiddenFile(rel) {
			return fmt.Errorf("the modpack has an unsafe file name: %q", f.Path)
		}
		sum := strings.ToLower(f.Hashes["sha512"])
		if len(sum) != 128 {
			sum = strings.ToLower(f.Hashes["sha1"])
		}
		if len(sum) != 128 && len(sum) != 40 {
			return fmt.Errorf("the modpack gives no checksum for %s", rel)
		}
		var from string
		for _, d := range f.Downloads {
			if u, err := url.Parse(d); err == nil && mrpackSchemes[u.Scheme] && mrpackHosts[strings.ToLower(u.Hostname())] {
				from = d
				break
			}
		}
		if from == "" {
			return fmt.Errorf("the modpack's %s comes from a site Modrinth's format doesn't allow", rel)
		}
		tmp, err := s.m.downloadVerified(ctx, from, s.m.serversDir(), sum, func(string) {}, s.m.javaUserAgent())
		if err != nil {
			return fmt.Errorf("%s: %w", path.Base(rel), err)
		}
		err = w.put(tmp, rel)
		os.Remove(tmp)
		if err != nil {
			return err
		}
		got++
		if got%20 == 0 {
			s.say(fmt.Sprintf("Downloaded %d of the modpack's files…", got))
		}
	}
	// Its own files: overrides, then the ones only for servers.
	for _, prefix := range []string{"overrides/", "server-overrides/"} {
		for _, f := range zr.File {
			name, ok := strings.CutPrefix(f.Name, prefix)
			if !ok || name == "" || strings.HasSuffix(f.Name, "/") {
				continue
			}
			rel, err := cleanRel(name)
			if err != nil || rel == "." || hiddenFile(rel) {
				return fmt.Errorf("the modpack has an unsafe file name: %q", f.Name)
			}
			if f.UncompressedSize64 > 1<<30 {
				return fmt.Errorf("the modpack's %s is too big", rel)
			}
			if err := w.extract(f, rel); err != nil {
				return err
			}
		}
	}
	if err := w.finish(); err != nil {
		return err
	}
	s.mu.Lock()
	s.meta.PackSource = ""
	s.mu.Unlock()
	if err := s.saveMeta(); err != nil {
		return err
	}
	s.say(fmt.Sprintf("Modpack %s is installed (%d downloaded files).", label, got))
	return nil
}

// setModpack records the modpack's version and what it runs on. A new
// server gets console support only on Fabric; an update keeps the server's
// choice unless the pack moved to another loader.
func (s *Server) setModpack(src, packVersion, label, loader, mc, lv string, update bool) error {
	s.mu.Lock()
	moved := s.meta.Loader != loader
	if update && (moved || s.meta.Version != mc || s.meta.LoaderVersion != lv) {
		s.meta.Launch = "" // the new loader or Minecraft is installed afresh
	}
	s.meta.Loader, s.meta.Version, s.meta.LoaderVersion, s.meta.KeepVersion, s.meta.Modpack = loader, mc, lv, true, label
	if !update || moved {
		s.meta.NoConsoles = len(consoleSet(loader)) == 0
	}
	if !strings.HasPrefix(src, "file:") {
		s.meta.PackOrigin, s.meta.PackVersion = src, packVersion
	}
	s.mu.Unlock()
	return s.saveMeta()
}

func copyIntoRoot(root *os.Root, from, rel string) error {
	if err := root.MkdirAll(path.Dir(rel), 0o755); err != nil {
		return err
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := root.OpenFile(rel, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func extractIntoRoot(root *os.Root, f *zip.File, rel string) error {
	if err := root.MkdirAll(path.Dir(rel), 0o755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := root.OpenFile(rel, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(rc, 1<<30)); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ---- what a modpack put where (for its updates) ----

// modpackRecordFile lists the files the server's modpack put in, with
// their SHA-256, so an update can tell the pack's files from ones changed or
// added by hand. It's in the server folder, so a before-update backup and
// its restore keep it in step.
const modpackRecordFile = ".blockheads-modpack.json"

type modpackRecord struct {
	Files map[string]string `json:"files"` // path → SHA-256 as the pack put it
}

func (s *Server) readModpackRecord() (map[string]string, bool) {
	b, err := os.ReadFile(filepath.Join(s.dir(), modpackRecordFile))
	if err != nil {
		return nil, false
	}
	var r modpackRecord
	if json.Unmarshal(b, &r) != nil || r.Files == nil {
		return nil, false
	}
	return r.Files, true
}

// packWriter puts a modpack's files in the server folder and remembers
// them. On an update (old set), a file changed or added by hand since is
// left as it is, and files the old version put in that the new one doesn't
// have are removed, unless they were changed.
type packWriter struct {
	s    *Server
	root *os.Root
	old  map[string]string // nil: a new server
	now  map[string]string
	kept []string // changed by hand, so not replaced
}

func (s *Server) newPackWriter(root *os.Root, update bool) *packWriter {
	w := &packWriter{s: s, root: root, now: map[string]string{}}
	if update {
		w.old, _ = s.readModpackRecord()
		if w.old == nil {
			w.old = map[string]string{}
		}
	}
	return w
}

// rootSum is the SHA-256 of a file in the server folder ("" if missing).
func (w *packWriter) rootSum(rel string) string {
	f, err := w.root.Open(rel)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// put copies a downloaded file to rel.
func (w *packWriter) put(tmp, rel string) error {
	sum, err := fileSHA256(tmp)
	if err != nil {
		return err
	}
	was, ours := w.old[rel]
	if p, wrote := w.now[rel]; wrote {
		was, ours = p, true // put there earlier in this install (overrides, then server-overrides)
	}
	w.now[rel] = sum
	if w.old != nil {
		if cur := w.rootSum(rel); cur != "" && cur != sum && (!ours || cur != was) {
			w.kept = append(w.kept, rel) // changed or added by hand
			return nil
		}
	}
	return copyIntoRoot(w.root, tmp, rel)
}

// extract puts a file from the pack's zip at rel.
func (w *packWriter) extract(f *zip.File, rel string) error {
	tmp, err := os.CreateTemp(w.s.m.serversDir(), ".packfile-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	rc, err := f.Open()
	if err != nil {
		tmp.Close()
		return err
	}
	_, err = io.Copy(tmp, io.LimitReader(rc, 1<<30))
	rc.Close()
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return w.put(tmp.Name(), rel)
}

// finish removes what the old version had and the new one doesn't, and
// saves the record.
func (w *packWriter) finish() error {
	removed := 0
	for rel, was := range w.old {
		if _, still := w.now[rel]; still {
			continue
		}
		if cur := w.rootSum(rel); cur != "" && cur == was {
			if err := w.root.Remove(rel); err == nil {
				removed++
			}
		}
	}
	b, err := json.Marshal(modpackRecord{Files: w.now})
	if err != nil {
		return err
	}
	p := filepath.Join(w.s.dir(), modpackRecordFile)
	if err := writeSynced(p+".tmp", string(b)); err != nil {
		return err
	}
	if err := os.Rename(p+".tmp", p); err != nil {
		return err
	}
	if w.old != nil {
		if removed > 0 {
			w.s.say(fmt.Sprintf("Removed %d of the old version's files that the new one doesn't have.", removed))
		}
		if kept := sortedUnique(w.kept); len(kept) > 0 {
			list := kept
			if len(list) > 8 {
				list = append(list[:8:8], fmt.Sprintf("and %d more", len(kept)-8))
			}
			files := "files"
			if len(kept) == 1 {
				files = "file"
			}
			w.s.say(fmt.Sprintf("Kept %d %s changed by hand instead of the modpack's new version: %s.", len(kept), files, strings.Join(list, ", ")))
		}
	}
	return nil
}

// ---- modpack updates ----

// ModpackView is a modpack server's pack, and whether there's a newer
// version of it.
type ModpackView struct {
	Modpack string `json:"modpack"`
	Source  string `json:"source,omitempty"` // "modrinth" or "curseforge"; "" for an uploaded pack
	// Updatable: the panel knows what the pack put where (servers made
	// before modpack updates, and uploaded packs, can't be updated).
	Updatable bool   `json:"updatable"`
	Newer     string `json:"newer,omitempty"` // the newer version's name
	Problem   string `json:"problem,omitempty"`
}

// newestModpackVersion is the version a modpack update would install: its
// ID and name.
func (m *Manager) newestModpackVersion(ctx context.Context, origin string) (string, string, error) {
	kind, arg, _ := strings.Cut(origin, ":")
	switch kind {
	case "modrinth":
		v, err := m.newestMrpack(ctx, arg)
		if err != nil {
			return "", "", err
		}
		name := v.Name
		if name == "" {
			name = v.VersionNumber
		}
		return v.ID, name, nil
	case "curseforge":
		n, err := strconv.Atoi(arg)
		if err != nil {
			return "", "", errors.New("bad modpack")
		}
		f, err := m.cfNewestFile(ctx, n)
		if err != nil {
			return "", "", err
		}
		return strconv.Itoa(f.ID), strings.TrimSuffix(f.DisplayName, ".zip"), nil
	}
	return "", "", errors.New("the modpack came from an upload")
}

// ModpackInfo says which modpack the server runs and whether a newer
// version is out (asked at most once an hour).
func (s *Server) ModpackInfo(ctx context.Context) ModpackView {
	s.mu.Lock()
	v := ModpackView{Modpack: s.meta.Modpack}
	origin, have := s.meta.PackOrigin, s.meta.PackVersion
	cached, at := s.packCheck, s.packCheckAt
	s.mu.Unlock()
	if v.Modpack == "" || origin == "" {
		return v
	}
	v.Source, _, _ = strings.Cut(origin, ":")
	_, v.Updatable = s.readModpackRecord()
	if !v.Updatable {
		return v
	}
	if cached.origin == origin && time.Since(at) < versionCheckEvery {
		if cached.id != have {
			v.Newer = cached.name
		}
		return v
	}
	id, name, err := s.m.newestModpackVersion(ctx, origin)
	if err != nil {
		v.Problem = "Couldn't check for a newer version: " + err.Error()
		return v
	}
	s.mu.Lock()
	s.packCheck, s.packCheckAt = packCheck{origin: origin, id: id, name: name}, time.Now()
	s.mu.Unlock()
	if id != have {
		v.Newer = name
	}
	return v
}

// packCheck is the newest version of the server's modpack, last asked.
type packCheck struct{ origin, id, name string }

// UpdateModpack moves a modpack server to its pack's newest version, the
// way Update does: a backup of the whole server folder first, then the new
// version's files over the old ones. Worlds aren't touched; files changed or
// added by hand since are kept; the old version's files the new one doesn't
// have are removed. The pack's Minecraft version and loader come with it.
func (s *Server) UpdateModpack() error {
	s.mu.Lock()
	origin := s.meta.PackOrigin
	s.packCheckAt = time.Time{} // ask again afterwards
	s.mu.Unlock()
	if !s.isJava() || origin == "" {
		return errors.New("this server wasn't made from a modpack on Modrinth or CurseForge, so there's nothing to update it to")
	}
	if _, ok := s.readModpackRecord(); !ok {
		return errors.New("this server was made before the panel could update modpacks, so it doesn't know which files are the pack's; make a new server from the modpack instead")
	}
	if strings.HasPrefix(origin, "curseforge:") && !s.m.CurseForgeKeySet() {
		return ErrNoCurseForgeKey
	}
	return s.startUpdate(false, true)
}
