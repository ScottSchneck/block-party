package servers

import (
	"archive/zip"
	"context"
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

// modpackFile gets the pack: downloads it from Modrinth, or takes the
// uploaded one. The caller removes the file.
func (s *Server) modpackFile(ctx context.Context, src string) (string, string, error) {
	kind, arg, _ := strings.Cut(src, ":")
	if kind == "file" {
		p := filepath.Join(s.m.serversDir(), arg)
		if !strings.HasPrefix(arg, ".upload-") || strings.ContainsAny(arg, `/\`) || !fileExists(p) {
			return "", "", errors.New("the uploaded modpack is gone (the panel restarted?); add the server again")
		}
		return p, "", nil
	}
	var versions []struct {
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
	if err := s.m.getJSON(ctx, s.m.opts.ModrinthAPI+"/project/"+url.PathEscape(arg)+"/version", &versions); err != nil {
		return "", "", fmt.Errorf("couldn't ask Modrinth for the modpack's versions: %w", err)
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
		return "", "", errors.New("the modpack has no version for Fabric, Forge or NeoForge")
	}
	v := versions[pick]
	for _, f := range v.Files {
		if !strings.HasSuffix(strings.ToLower(f.FileName), ".mrpack") || (!f.Primary && len(v.Files) > 1) {
			continue
		}
		if len(f.Hashes["sha512"]) != 128 {
			return "", "", errors.New("Modrinth has no checked download of the modpack")
		}
		s.say("Downloading the modpack (" + v.Name + ")…")
		file, err := s.m.downloadVerified(ctx, f.URL, s.m.serversDir(), strings.ToLower(f.Hashes["sha512"]), s.say, s.m.javaUserAgent())
		return file, v.VersionNumber, err
	}
	return "", "", errors.New("the modpack's newest version has no .mrpack file")
}

// installModpack installs a new server from its modpack.
func (s *Server) installModpack(ctx context.Context) error {
	s.mu.Lock()
	src := s.meta.PackSource
	s.mu.Unlock()
	if id, ok := strings.CutPrefix(src, "curseforge:"); ok {
		n, err := strconv.Atoi(id)
		if err != nil {
			return errors.New("bad modpack")
		}
		return s.installCurseForgePack(ctx, n)
	}
	file, _, err := s.modpackFile(ctx, src)
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
	s.mu.Lock()
	s.meta.Loader, s.meta.Version, s.meta.LoaderVersion, s.meta.KeepVersion, s.meta.Modpack = loader, mc, lv, true, label
	s.meta.NoConsoles = loader != LoaderFabric
	s.mu.Unlock()
	if err := s.saveMeta(); err != nil {
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
		err = copyIntoRoot(root, tmp, rel)
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
			if err := extractIntoRoot(root, f, rel); err != nil {
				return err
			}
		}
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
