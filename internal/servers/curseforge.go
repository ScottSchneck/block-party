package servers

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// CurseForge: Bedrock add-ons, texture packs and worlds for the Add-ons tab,
// and Java modpacks for Add server, with the owner's CurseForge API key
// (from console.curseforge.com). The key is kept in its own file, never
// shown again.
//
// Some creators turn off downloads through other apps; those can't be
// installed by the panel (it says where to get them), and the panel doesn't
// work around that.

// DefaultCurseForgeAPI is CurseForge's API.
const DefaultCurseForgeAPI = "https://api.curseforge.com"

// curseForgeJava is Minecraft: Java Edition's game ID on CurseForge.
const curseForgeJava = 432

// CurseForge search kinds.
const (
	CFAddons   = "addons"
	CFTextures = "texture-packs"
	CFWorlds   = "worlds"
	CFModpacks = "modpacks"
)

// ErrNoCurseForgeKey: the owner hasn't added a CurseForge key.
var ErrNoCurseForgeKey = errors.New("add a CurseForge API key on the Settings page first")

func (m *Manager) curseForgeKeyPath() string { return filepath.Join(m.opts.DataDir, "curseforge-key") }

func (m *Manager) curseForgeKey() string {
	b, err := os.ReadFile(m.curseForgeKeyPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// CurseForgeKeySet reports whether a CurseForge key is saved.
func (m *Manager) CurseForgeKeySet() bool { return m.curseForgeKey() != "" }

// SetCurseForgeKey checks a key with CurseForge and saves it ("" removes
// it).
func (m *Manager) SetCurseForgeKey(ctx context.Context, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		err := os.Remove(m.curseForgeKeyPath())
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		m.cf.forget()
		return err
	}
	if len(key) > 200 || strings.ContainsAny(key, "\r\n\t ") {
		return errors.New("that doesn't look like a CurseForge API key")
	}
	var games struct {
		Data []struct {
			ID int `json:"id"`
		} `json:"data"`
	}
	if err := m.cfGetWith(ctx, key, "/v1/games?pageSize=1", &games); err != nil {
		return err
	}
	m.cf.forget()
	return os.WriteFile(m.curseForgeKeyPath(), []byte(key+"\n"), 0o600)
}

// cfCache keeps CurseForge's game and class IDs.
type cfCache struct {
	mu      sync.Mutex
	bedrock int
	classes map[int]map[string]int // game -> class slug -> ID
}

func (c *cfCache) forget() {
	c.mu.Lock()
	c.bedrock, c.classes = 0, nil
	c.mu.Unlock()
}

var errCFKeyRefused = errors.New("CurseForge refused the API key; check it on console.curseforge.com and add it again in Settings")

func (m *Manager) cfGet(ctx context.Context, p string, v any) error {
	key := m.curseForgeKey()
	if key == "" {
		return ErrNoCurseForgeKey
	}
	return m.cfGetWith(ctx, key, p, v)
}

func (m *Manager) cfGetWith(ctx context.Context, key, p string, v any) error {
	return m.cfDo(ctx, key, http.MethodGet, p, nil, v)
}

func (m *Manager) cfPost(ctx context.Context, p string, body, v any) error {
	key := m.curseForgeKey()
	if key == "" {
		return ErrNoCurseForgeKey
	}
	b, _ := json.Marshal(body)
	return m.cfDo(ctx, key, http.MethodPost, p, b, v)
}

func (m *Manager) cfDo(ctx context.Context, key, method, p string, body []byte, v any) error {
	u := m.opts.CurseForgeAPI + p
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", m.javaUserAgent())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.http().Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach CurseForge: %w", plainErr(u, err))
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		return errCFKeyRefused
	case resp.StatusCode == http.StatusNotFound:
		return errors.New("CurseForge doesn't have that")
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("CurseForge answered %s", resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(v)
}

// cfBedrock is Minecraft Bedrock's game ID on CurseForge.
func (m *Manager) cfBedrock(ctx context.Context) (int, error) {
	m.cf.mu.Lock()
	id := m.cf.bedrock
	m.cf.mu.Unlock()
	if id != 0 {
		return id, nil
	}
	var games struct {
		Data []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
			Slug string `json:"slug"`
		} `json:"data"`
	}
	if err := m.cfGet(ctx, "/v1/games?pageSize=50", &games); err != nil {
		return 0, err
	}
	for _, g := range games.Data {
		if g.Slug == "minecraft-bedrock" || strings.EqualFold(g.Name, "Minecraft Bedrock") {
			m.cf.mu.Lock()
			m.cf.bedrock = g.ID
			m.cf.mu.Unlock()
			return g.ID, nil
		}
	}
	return 0, errors.New("CurseForge didn't list Minecraft Bedrock")
}

// cfClasses maps a game's class slugs ("addons", "modpacks"…) to IDs.
func (m *Manager) cfClasses(ctx context.Context, game int) (map[string]int, error) {
	m.cf.mu.Lock()
	c := m.cf.classes[game]
	m.cf.mu.Unlock()
	if c != nil {
		return c, nil
	}
	var cats struct {
		Data []struct {
			ID      int    `json:"id"`
			Slug    string `json:"slug"`
			Name    string `json:"name"`
			IsClass bool   `json:"isClass"`
		} `json:"data"`
	}
	if err := m.cfGet(ctx, fmt.Sprintf("/v1/categories?gameId=%d&classesOnly=true", game), &cats); err != nil {
		return nil, err
	}
	c = map[string]int{}
	for _, k := range cats.Data {
		c[k.Slug] = k.ID
	}
	m.cf.mu.Lock()
	if m.cf.classes == nil {
		m.cf.classes = map[int]map[string]int{}
	}
	m.cf.classes[game] = c
	m.cf.mu.Unlock()
	return c, nil
}

// cfClassFor finds the class for a kind (CurseForge's Bedrock slugs, with a
// few spellings).
func cfClassFor(classes map[string]int, kind string) int {
	want := map[string][]string{
		CFAddons:   {"addons", "add-ons", "mcaddons"},
		CFTextures: {"texture-packs", "resource-packs", "texturepacks"},
		CFWorlds:   {"maps", "worlds"},
		CFModpacks: {"modpacks"},
		cfJavaMods: {"mc-mods"},
	}[kind]
	for _, w := range want {
		if id := classes[w]; id != 0 {
			return id
		}
	}
	return 0
}

// CFResult is something found on CurseForge.
type CFResult struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Author      string `json:"author"`
	Description string `json:"description"`
	Icon        string `json:"icon,omitempty"`
	Downloads   int64  `json:"downloads"`
	Page        string `json:"page,omitempty"`
	// Installable: the creator allows downloads through other apps.
	Installable bool     `json:"installable"`
	Versions    []string `json:"versions,omitempty"`
}

type cfMod struct {
	ID                   int     `json:"id"`
	Name                 string  `json:"name"`
	Slug                 string  `json:"slug"`
	Summary              string  `json:"summary"`
	ClassID              int     `json:"classId"`
	DownloadCount        float64 `json:"downloadCount"`
	AllowModDistribution *bool   `json:"allowModDistribution"`
	Logo                 *struct {
		ThumbnailURL string `json:"thumbnailUrl"`
	} `json:"logo"`
	Links struct {
		WebsiteURL string `json:"websiteUrl"`
	} `json:"links"`
	Authors []struct {
		Name string `json:"name"`
	} `json:"authors"`
	LatestFilesIndexes []cfIndex `json:"latestFilesIndexes"`
}

// cfIndex is one of a project's newest files, for one game version.
type cfIndex struct {
	GameVersion string `json:"gameVersion"`
	FileID      int    `json:"fileId"`
	ReleaseType int    `json:"releaseType"`
	ModLoader   *int   `json:"modLoader"`
}

// CurseForge's mod loader numbers.
const (
	cfForge    = 1
	cfFabric   = 4
	cfQuilt    = 5
	cfNeoForge = 6
)

// runsOnPanel: a modpack whose newest files are all for loaders the panel
// doesn't run (Quilt) is left out of searches.
func (c cfMod) runsOnPanel() bool {
	known := false
	for _, f := range c.LatestFilesIndexes {
		if f.ModLoader == nil {
			continue
		}
		known = true
		switch *f.ModLoader {
		case cfForge, cfFabric, cfNeoForge:
			return true
		}
	}
	return !known
}

// bedrockVer reads a Bedrock version for comparing. Minecraft renumbered
// in 2026, so "26.60" (as CurseForge writes it), "1.26.60.1" (the
// server's) and [26, 60, 0] all become [1 26 60]. nil if it isn't one.
func bedrockVer(v string) []int {
	var n []int
	for _, p := range strings.Split(strings.TrimSpace(v), ".") {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return nil
		}
		n = append(n, x)
	}
	if len(n) == 0 || n[0] == 0 {
		return nil
	}
	if n[0] >= 26 {
		n = append([]int{1}, n...)
	}
	if len(n) > 3 {
		n = n[:3]
	}
	return n
}

func cmpVer(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// cfCandidate is a file, for one game version it's made for (ver nil:
// it doesn't say).
type cfCandidate struct {
	tag     string
	ver     []int
	file    int
	release bool
	date    string
}

// cfCandidates lists a file once for each Bedrock version it's made for.
func cfCandidates(file int, tags []string, release bool, date string) []cfCandidate {
	var out []cfCandidate
	for _, t := range tags {
		if v := bedrockVer(t); v != nil {
			out = append(out, cfCandidate{tag: t, ver: v, file: file, release: release, date: date})
		}
	}
	if len(out) == 0 {
		out = append(out, cfCandidate{file: file, release: release, date: date})
	}
	return out
}

// cfPick chooses the file for a Bedrock server on version server: the one
// made for the newest version that isn't newer than the server (add-ons
// made for an older version keep working), a release over a beta, the
// newest. When every file needs a newer Minecraft, ok is false and needs is
// the oldest version one is made for. Without a server version, the newest
// file is chosen.
func cfPick(cands []cfCandidate, server []int) (best cfCandidate, ok bool, needs string) {
	better := func(a, b cfCandidate) bool {
		if c := cmpVer(a.ver, b.ver); c != 0 && server != nil {
			return c > 0
		}
		if a.release != b.release {
			return a.release
		}
		if a.date != b.date {
			return a.date > b.date
		}
		return a.file > b.file
	}
	var oldest []int
	for _, c := range cands {
		if server != nil && c.ver != nil && cmpVer(c.ver, server) > 0 {
			if oldest == nil || cmpVer(c.ver, oldest) < 0 {
				oldest, needs = c.ver, c.tag
			}
			continue
		}
		if !ok || better(c, best) {
			best, ok = c, true
		}
	}
	if ok {
		needs = ""
	}
	return best, ok, needs
}

func (c cfMod) result(server []int) (CFResult, string) {
	r := CFResult{ID: c.ID, Name: c.Name, Description: c.Summary, Downloads: int64(c.DownloadCount),
		Page: httpsOnly(c.Links.WebsiteURL), Installable: c.AllowModDistribution == nil || *c.AllowModDistribution}
	if c.Logo != nil {
		r.Icon = httpsOnly(c.Logo.ThumbnailURL)
	}
	if len(c.Authors) > 0 {
		r.Author = c.Authors[0].Name
	}
	if server != nil {
		// A Bedrock server: the file it would get, and what that's made for.
		var order []int
		tags, release := map[int][]string{}, map[int]bool{}
		for _, f := range c.LatestFilesIndexes {
			if _, seen := tags[f.FileID]; !seen {
				order = append(order, f.FileID)
			}
			tags[f.FileID] = append(tags[f.FileID], f.GameVersion)
			release[f.FileID] = f.ReleaseType == 1
		}
		var cands []cfCandidate
		for _, id := range order {
			cands = append(cands, cfCandidates(id, tags[id], release[id], "")...)
		}
		best, ok, needs := cfPick(cands, server)
		if !ok {
			return r, needs
		}
		if best.tag != "" {
			r.Versions = []string{best.tag}
		}
		return r, ""
	}
	seen := map[string]bool{}
	for _, f := range c.LatestFilesIndexes {
		if plainVersion.MatchString(f.GameVersion) && !seen[f.GameVersion] && len(r.Versions) < 3 {
			seen[f.GameVersion] = true
			r.Versions = append(r.Versions, f.GameVersion)
		}
	}
	return r, ""
}

// SearchCurseForge searches a kind of thing on CurseForge. Only what the
// panel can install is shown: for a Bedrock server on version forVersion,
// what's made for it (or an older version); for modpacks, ones for Fabric,
// Forge or NeoForge. Notes say what was left out.
func (m *Manager) SearchCurseForge(ctx context.Context, kind, query, forVersion string) ([]CFResult, []string, error) {
	game := curseForgeJava
	if kind != CFModpacks {
		var err error
		if game, err = m.cfBedrock(ctx); err != nil {
			return nil, nil, err
		}
	}
	classes, err := m.cfClasses(ctx, game)
	if err != nil {
		return nil, nil, err
	}
	class := cfClassFor(classes, kind)
	if class == 0 {
		return nil, nil, fmt.Errorf("CurseForge has no %s section", kind)
	}
	q := url.Values{"gameId": {strconv.Itoa(game)}, "classId": {strconv.Itoa(class)}, "pageSize": {"20"}, "sortOrder": {"desc"}}
	if query = strings.TrimSpace(query); query != "" {
		if len([]rune(query)) > 100 {
			query = string([]rune(query)[:100])
		}
		q.Set("searchFilter", query)
		q.Set("sortField", "2") // popularity, among matches
	} else {
		q.Set("sortField", "6") // most downloaded
	}
	var res struct {
		Data []cfMod `json:"data"`
	}
	if err := m.cfGet(ctx, "/v1/mods/search?"+q.Encode(), &res); err != nil {
		return nil, nil, err
	}
	var server []int
	if kind != CFModpacks {
		server = bedrockVer(forVersion)
	}
	out := []CFResult{}
	kept, tooNew, quilt := 0, 0, 0
	for _, c := range res.Data {
		r, needs := c.result(server)
		switch {
		case !r.Installable:
			kept++
		case needs != "":
			tooNew++
		case kind == CFModpacks && !c.runsOnPanel():
			quilt++
		default:
			out = append(out, r)
		}
	}
	var notes []string
	if tooNew > 0 {
		notes = append(notes, fmt.Sprintf("%d more found need a newer Minecraft than this server's %s, so they're not shown.", tooNew, forVersion))
	}
	if quilt > 0 {
		notes = append(notes, fmt.Sprintf("%d more found are for Quilt, which the panel doesn't run.", quilt))
	}
	if kept > 0 {
		notes = append(notes, fmt.Sprintf("%d more found can only be downloaded from CurseForge's own site.", kept))
	}
	return out, notes, nil
}

type cfFile struct {
	ID           int      `json:"id"`
	ModID        int      `json:"modId"`
	DisplayName  string   `json:"displayName"`
	FileName     string   `json:"fileName"`
	ReleaseType  int      `json:"releaseType"`
	FileDate     string   `json:"fileDate"`
	DownloadURL  string   `json:"downloadUrl"`
	GameVersions []string `json:"gameVersions"`
	IsServerPack bool     `json:"isServerPack"`
	Dependencies []struct {
		ModID        int `json:"modId"`
		RelationType int `json:"relationType"` // 3: it's needed
	} `json:"dependencies"`
	ServerPackFileID int   `json:"serverPackFileId"`
	FileLength       int64 `json:"fileLength"`
	Hashes           []struct {
		Value string `json:"value"`
		Algo  int    `json:"algo"`
	} `json:"hashes"`
}

// sha1 is the file's SHA-1, when CurseForge gives one.
func (f cfFile) sha1() string {
	for _, h := range f.Hashes {
		if h.Algo == 1 && len(h.Value) == 40 {
			return strings.ToLower(h.Value)
		}
	}
	return ""
}

// cfDownloadHosts are where CurseForge's files come from.
var cfDownloadHosts = map[string]bool{"edge.forgecdn.net": true, "mediafilez.forgecdn.net": true, "media.forgecdn.net": true}

// cfSchemes: https only (tests add http).
var cfSchemes = map[string]bool{"https": true}

func cfDownloadable(u string) bool {
	p, err := url.Parse(u)
	return err == nil && cfSchemes[p.Scheme] && cfDownloadHosts[strings.ToLower(p.Hostname())]
}

func (m *Manager) cfMod(ctx context.Context, id int) (cfMod, error) {
	var res struct {
		Data cfMod `json:"data"`
	}
	err := m.cfGet(ctx, fmt.Sprintf("/v1/mods/%d", id), &res)
	return res.Data, err
}

// cfNewestFile is a project's newest file (a release over a beta), not a
// server pack.
func (m *Manager) cfNewestFile(ctx context.Context, id int) (cfFile, error) {
	return m.cfFileFor(ctx, id, "")
}

// cfFileFor is the file for a Bedrock server on version forVersion (see
// cfPick), or with no version the newest.
func (m *Manager) cfFileFor(ctx context.Context, id int, forVersion string) (cfFile, error) {
	var res struct {
		Data []cfFile `json:"data"`
	}
	if err := m.cfGet(ctx, fmt.Sprintf("/v1/mods/%d/files?pageSize=50", id), &res); err != nil {
		return cfFile{}, err
	}
	files := res.Data[:0]
	for _, f := range res.Data {
		if !f.IsServerPack {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return cfFile{}, errors.New("it has no files on CurseForge")
	}
	byID := map[int]cfFile{}
	var cands []cfCandidate
	for _, f := range files {
		byID[f.ID] = f
		cands = append(cands, cfCandidates(f.ID, f.GameVersions, f.ReleaseType == 1, f.FileDate)...)
	}
	best, ok, needs := cfPick(cands, bedrockVer(forVersion))
	if !ok {
		return cfFile{}, fmt.Errorf("it needs Minecraft %s or newer, and this server has %s", needs, forVersion)
	}
	return byID[best.file], nil
}

// cfNotDistributable says a file can't be downloaded by the panel.
func cfNotDistributable(name, page string) error {
	where := "CurseForge"
	if page != "" {
		where = page
	}
	return fmt.Errorf("%s's creator only allows downloading it from CurseForge itself: get it from %s and upload it here", name, where)
}

// cfDownload downloads a file into dir, checking its SHA-1.
func (m *Manager) cfDownload(ctx context.Context, f cfFile, dir string, say func(string)) (string, error) {
	if f.DownloadURL == "" {
		return "", errors.New("not downloadable")
	}
	if !cfDownloadable(f.DownloadURL) {
		return "", fmt.Errorf("CurseForge gave an unexpected download address for %s", f.FileName)
	}
	return m.downloadVerified(ctx, f.DownloadURL, dir, f.sha1(), say, m.javaUserAgent())
}

// InstallCurseForge installs a Bedrock add-on, texture pack or world from
// CurseForge into the server (a world is added, not played).
func (s *Server) InstallCurseForge(ctx context.Context, id int) (name string, res InstallResult, world UploadResult, err error) {
	if !s.isBedrock() {
		return "", res, world, errJavaAddons
	}
	mod, err := s.m.cfMod(ctx, id)
	if err != nil {
		return "", res, world, err
	}
	page := httpsOnly(mod.Links.WebsiteURL)
	if mod.AllowModDistribution != nil && !*mod.AllowModDistribution {
		return mod.Name, res, world, cfNotDistributable(mod.Name, page)
	}
	f, err := s.m.cfFileFor(ctx, id, s.Status().Version)
	if err != nil {
		return mod.Name, res, world, fmt.Errorf("%s: %w", mod.Name, err)
	}
	if f.DownloadURL == "" {
		return mod.Name, res, world, cfNotDistributable(mod.Name, page)
	}
	file, err := s.m.cfDownload(ctx, f, s.m.serversDir(), func(string) {})
	if err != nil {
		return mod.Name, res, world, err
	}
	defer os.Remove(file)
	game, _ := s.m.cfBedrock(ctx)
	classes, _ := s.m.cfClasses(ctx, game)
	if c := cfClassFor(classes, CFWorlds); c != 0 && mod.ClassID == c || strings.HasSuffix(strings.ToLower(f.FileName), ".mcworld") {
		world, err = s.AddWorld(ctx, file, f.FileName, false)
		return mod.Name, res, world, err
	}
	res, err = s.InstallPacks(ctx, file)
	return mod.Name, res, world, err
}

// ---- Java modpacks ----

// cfManifest is a CurseForge modpack's manifest.json.
type cfManifest struct {
	ManifestType string `json:"manifestType"`
	Name         string `json:"name"`
	Version      string `json:"version"`
	Overrides    string `json:"overrides"`
	Minecraft    struct {
		Version    string `json:"version"`
		ModLoaders []struct {
			ID      string `json:"id"`
			Primary bool   `json:"primary"`
		} `json:"modLoaders"`
	} `json:"minecraft"`
	Files []struct {
		ProjectID int  `json:"projectID"`
		FileID    int  `json:"fileID"`
		Required  bool `json:"required"`
	} `json:"files"`
}

// loader reads the pack's mod loader ("forge-47.2.0").
func (mf cfManifest) loader() (string, string, error) {
	for _, l := range mf.Minecraft.ModLoaders {
		if !l.Primary && len(mf.Minecraft.ModLoaders) > 1 {
			continue
		}
		name, v, _ := strings.Cut(l.ID, "-")
		switch strings.ToLower(name) {
		case "forge":
			return LoaderForge, v, nil
		case "neoforge":
			return LoaderNeoForge, v, nil
		case "fabric":
			return LoaderFabric, v, nil
		case "quilt":
			return "", "", errors.New("the modpack needs Quilt, which the panel doesn't run (Fabric, Forge and NeoForge packs work)")
		}
	}
	return LoaderVanilla, "", nil
}

// serverPackSkip: what a server pack's own start files are, which the
// panel replaces with its own.
func serverPackSkip(rel string) bool {
	low := strings.ToLower(rel)
	base := path.Base(low)
	switch {
	case strings.HasPrefix(low, "libraries/"), strings.HasPrefix(low, "versions/"), strings.HasPrefix(low, ".fabric/"):
		return true
	case strings.HasSuffix(base, ".sh"), strings.HasSuffix(base, ".bat"), strings.HasSuffix(base, ".cmd"), strings.HasSuffix(base, ".ps1"), strings.HasSuffix(base, ".command"):
		return true
	case !strings.Contains(low, "/") && strings.HasSuffix(base, ".jar"):
		return true // the server and installer jars at the top
	}
	switch base {
	case "eula.txt", "server.properties", "user_jvm_args.txt", "variables.txt", "run.txt", metaFile:
		return true
	}
	return false
}

// installCurseForgePack installs a new server from a CurseForge modpack.
func (s *Server) installCurseForgePack(ctx context.Context, id int) error {
	mod, err := s.m.cfMod(ctx, id)
	if err != nil {
		return err
	}
	page := httpsOnly(mod.Links.WebsiteURL)
	if mod.AllowModDistribution != nil && !*mod.AllowModDistribution {
		return cfNotDistributable(mod.Name, page)
	}
	f, err := s.m.cfNewestFile(ctx, id)
	if err != nil {
		return fmt.Errorf("%s: %w", mod.Name, err)
	}
	if f.DownloadURL == "" {
		return cfNotDistributable(mod.Name, page)
	}
	s.say("Downloading the modpack (" + f.DisplayName + ")…")
	file, err := s.m.cfDownload(ctx, f, s.m.serversDir(), s.say)
	if err != nil {
		return err
	}
	defer os.Remove(file)
	zr, err := zip.OpenReader(file)
	if err != nil {
		return errors.New("that isn't a CurseForge modpack (not a zip)")
	}
	defer zr.Close()
	var mf cfManifest
	for _, zf := range zr.File {
		if zf.Name != "manifest.json" {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		err = json.NewDecoder(io.LimitReader(rc, 16<<20)).Decode(&mf)
		rc.Close()
		if err != nil {
			return fmt.Errorf("couldn't read the modpack's manifest: %w", err)
		}
	}
	if mf.ManifestType != "minecraftModpack" || !plainVersion.MatchString(mf.Minecraft.Version) {
		return errors.New("that isn't a Minecraft modpack in CurseForge's format")
	}
	loader, lv, err := mf.loader()
	if err != nil {
		return err
	}
	label := strings.TrimSpace(mf.Name + " " + mf.Version)
	s.say(fmt.Sprintf("Modpack %s: Minecraft %s with %s %s, %d mods.", label, mf.Minecraft.Version, LoaderName(loader), lv, len(mf.Files)))
	s.mu.Lock()
	s.meta.Loader, s.meta.Version, s.meta.LoaderVersion, s.meta.KeepVersion, s.meta.Modpack = loader, mf.Minecraft.Version, lv, true, label
	s.meta.NoConsoles = loader != LoaderFabric
	s.mu.Unlock()
	if err := s.saveMeta(); err != nil {
		return err
	}
	if err := s.installLoader(ctx, false); err != nil {
		return err
	}
	root, err := os.OpenRoot(s.dir())
	if err != nil {
		return err
	}
	defer root.Close()

	// The pack's server pack, when it has one, holds exactly what a server
	// needs; otherwise the mods are fetched one by one.
	usedServerPack := false
	if f.ServerPackFileID > 0 {
		if err := s.cfServerPack(ctx, root, id, f.ServerPackFileID); err != nil {
			s.say("Couldn't use the modpack's server pack (" + err.Error() + "); getting the mods one by one instead.")
		} else {
			usedServerPack = true
		}
	}
	var blocked []string
	if !usedServerPack {
		if blocked, err = s.cfPackMods(ctx, root, mf); err != nil {
			return err
		}
	}
	// The pack's own files (configs, scripts for the mods).
	over := strings.Trim(mf.Overrides, "/")
	if over == "" {
		over = "overrides"
	}
	if !usedServerPack {
		for _, zf := range zr.File {
			name, ok := strings.CutPrefix(zf.Name, over+"/")
			if !ok || name == "" || strings.HasSuffix(zf.Name, "/") {
				continue
			}
			rel, err := cleanRel(name)
			if err != nil || rel == "." || hiddenFile(rel) {
				return fmt.Errorf("the modpack has an unsafe file name: %q", zf.Name)
			}
			if zf.UncompressedSize64 > 1<<30 {
				return fmt.Errorf("the modpack's %s is too big", rel)
			}
			if err := extractIntoRoot(root, zf, rel); err != nil {
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
	if len(blocked) > 0 {
		sort.Strings(blocked)
		s.say(fmt.Sprintf("%d mods couldn't be downloaded because their creators only allow downloads from CurseForge itself: %s. Download them from CurseForge and upload them to the mods folder (Files tab), or the server may not start.",
			len(blocked), strings.Join(blocked, ", ")))
	}
	s.say("Modpack " + label + " is installed.")
	return nil
}

// cfServerPack downloads a modpack's server pack and copies in its files
// (not its start scripts and jars: the panel starts the server its own way).
func (s *Server) cfServerPack(ctx context.Context, root *os.Root, modID, fileID int) error {
	var res struct {
		Data cfFile `json:"data"`
	}
	if err := s.m.cfGet(ctx, fmt.Sprintf("/v1/mods/%d/files/%d", modID, fileID), &res); err != nil {
		return err
	}
	if res.Data.DownloadURL == "" {
		return errors.New("its creator only allows downloading it from CurseForge itself")
	}
	s.say("Downloading the modpack's server pack…")
	file, err := s.m.cfDownload(ctx, res.Data, s.m.serversDir(), s.say)
	if err != nil {
		return err
	}
	defer os.Remove(file)
	zr, err := zip.OpenReader(file)
	if err != nil {
		return errors.New("it isn't a zip")
	}
	defer zr.Close()
	// Server packs often put everything in one top folder.
	prefix := ""
	if len(zr.File) > 0 {
		first, _, ok := strings.Cut(zr.File[0].Name, "/")
		same := ok
		for _, zf := range zr.File {
			if !strings.HasPrefix(zf.Name, first+"/") {
				same = false
				break
			}
		}
		if same && first != "mods" && first != "config" {
			prefix = first + "/"
		}
	}
	count := 0
	for _, zf := range zr.File {
		name := strings.TrimPrefix(zf.Name, prefix)
		if name == "" || strings.HasSuffix(zf.Name, "/") {
			continue
		}
		rel, err := cleanRel(name)
		if err != nil || rel == "." || hiddenFile(rel) {
			return fmt.Errorf("it has an unsafe file name: %q", zf.Name)
		}
		if serverPackSkip(rel) {
			continue
		}
		if zf.UncompressedSize64 > 1<<30 {
			return fmt.Errorf("its %s is too big", rel)
		}
		if err := extractIntoRoot(root, zf, rel); err != nil {
			return err
		}
		count++
	}
	if count == 0 {
		return errors.New("it was empty")
	}
	return nil
}

// cfPackMods downloads a modpack's mods (not client-only ones, texture
// packs or shaders) into mods/. It returns the ones whose creators don't
// allow other apps to download them.
func (s *Server) cfPackMods(ctx context.Context, root *os.Root, mf cfManifest) ([]string, error) {
	var ids []int
	var modIDs []int
	for _, f := range mf.Files {
		if f.FileID > 0 {
			ids = append(ids, f.FileID)
			modIDs = append(modIDs, f.ProjectID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	var files struct {
		Data []cfFile `json:"data"`
	}
	if err := s.m.cfPost(ctx, "/v1/mods/files", map[string]any{"fileIds": ids}, &files); err != nil {
		return nil, fmt.Errorf("couldn't ask CurseForge for the modpack's mods: %w", err)
	}
	var mods struct {
		Data []cfMod `json:"data"`
	}
	if err := s.m.cfPost(ctx, "/v1/mods", map[string]any{"modIds": modIDs}, &mods); err != nil {
		return nil, fmt.Errorf("couldn't ask CurseForge about the modpack's mods: %w", err)
	}
	classes, _ := s.m.cfClasses(ctx, curseForgeJava)
	modClass, names := map[int]int{}, map[int]string{}
	for _, m := range mods.Data {
		modClass[m.ID], names[m.ID] = m.ClassID, m.Name
	}
	modsClass := classes["mc-mods"]
	var blocked []string
	got, skipped := 0, 0
	for _, f := range files.Data {
		if c := modClass[f.ModID]; modsClass != 0 && c != 0 && c != modsClass {
			skipped++ // a texture pack, shader or world: not for the server
			continue
		}
		client, server := false, false
		for _, v := range f.GameVersions {
			client = client || strings.EqualFold(v, "Client")
			server = server || strings.EqualFold(v, "Server")
		}
		if client && !server {
			skipped++ // marked as only for players' games
			continue
		}
		name := names[f.ModID]
		if name == "" {
			name = f.FileName
		}
		if f.DownloadURL == "" {
			blocked = append(blocked, name)
			continue
		}
		base := path.Base(strings.ReplaceAll(f.FileName, "\\", "/"))
		if base == "." || base == ".." || !strings.HasSuffix(strings.ToLower(base), ".jar") {
			return nil, fmt.Errorf("the modpack has a mod with an odd file name: %q", f.FileName)
		}
		tmp, err := s.m.cfDownload(ctx, f, s.m.serversDir(), func(string) {})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		err = copyIntoRoot(root, tmp, "mods/"+base)
		os.Remove(tmp)
		if err != nil {
			return nil, err
		}
		got++
		if got%20 == 0 {
			s.say(fmt.Sprintf("Downloaded %d of the modpack's mods…", got))
		}
	}
	s.say(fmt.Sprintf("Downloaded %d mods (%d left out: only for players' games, texture packs or shaders).", got, skipped))
	return blocked, nil
}

// ---- Java mods (the Mods tab) ----

// cfJavaMods is the class of Java mods.
const cfJavaMods = "mc-mods"

// cfLoaderType is CurseForge's number for a mod loader.
func cfLoaderType(loader string) int {
	switch loader {
	case LoaderForge:
		return cfForge
	case LoaderFabric:
		return cfFabric
	case LoaderNeoForge:
		return cfNeoForge
	}
	return 0
}

// cfLoaderNames are the names CurseForge tags files with for each loader.
var cfLoaderNames = map[string]string{"forge": LoaderForge, "fabric": LoaderFabric, "neoforge": LoaderNeoForge, "quilt": "quilt"}

// searchCurseForgeMods searches CurseForge's Java mods for a server type and
// Minecraft version. blocked counts the ones only downloadable from
// CurseForge itself, which are left out.
func (m *Manager) searchCurseForgeMods(ctx context.Context, query, minecraft, loader string) (out []PluginResult, blocked int, err error) {
	classes, err := m.cfClasses(ctx, curseForgeJava)
	if err != nil {
		return nil, 0, err
	}
	class := cfClassFor(classes, cfJavaMods)
	if class == 0 {
		return nil, 0, errors.New("CurseForge has no mods section")
	}
	q := url.Values{"gameId": {strconv.Itoa(curseForgeJava)}, "classId": {strconv.Itoa(class)}, "pageSize": {strconv.Itoa(searchLimit)},
		"sortOrder": {"desc"}, "modLoaderType": {strconv.Itoa(cfLoaderType(loader))}}
	if minecraft != "" {
		q.Set("gameVersion", minecraft)
	}
	if query != "" {
		q.Set("searchFilter", query)
		q.Set("sortField", "2")
	} else {
		q.Set("sortField", "6")
	}
	var res struct {
		Data []cfMod `json:"data"`
	}
	if err := m.cfGet(ctx, "/v1/mods/search?"+q.Encode(), &res); err != nil {
		return nil, 0, err
	}
	for _, c := range res.Data {
		r, _ := c.result(nil)
		if !r.Installable {
			blocked++
			continue
		}
		out = append(out, PluginResult{Source: SourceCurseForge, ID: strconv.Itoa(c.ID), Name: r.Name, Author: r.Author,
			Description: r.Description, Icon: r.Icon, Page: r.Page, Downloads: r.Downloads})
	}
	return out, blocked, nil
}

// latestCurseForgeMod finds the newest file of a CurseForge mod for a
// Minecraft version and loader, made for servers.
func (m *Manager) latestCurseForgeMod(ctx context.Context, id, minecraft, loader string) (pluginFile, error) {
	n, err := strconv.Atoi(id)
	if err != nil || n <= 0 {
		return pluginFile{}, errors.New("that isn't a CurseForge mod")
	}
	mod, err := m.cfMod(ctx, n)
	if err != nil {
		return pluginFile{}, fmt.Errorf("couldn't find it on CurseForge: %w", err)
	}
	title, page := mod.Name, httpsOnly(mod.Links.WebsiteURL)
	if title == "" {
		title = id
	}
	if mod.AllowModDistribution != nil && !*mod.AllowModDistribution {
		return pluginFile{}, cfNotDistributable(title, page)
	}
	q := url.Values{"pageSize": {"50"}, "modLoaderType": {strconv.Itoa(cfLoaderType(loader))}}
	if minecraft != "" {
		q.Set("gameVersion", minecraft)
	}
	var res struct {
		Data []cfFile `json:"data"`
	}
	if err := m.cfGet(ctx, fmt.Sprintf("/v1/mods/%d/files?%s", n, q.Encode()), &res); err != nil {
		return pluginFile{}, fmt.Errorf("couldn't ask CurseForge for its files: %w", err)
	}
	var fits []cfFile
	for _, f := range res.Data {
		if f.IsServerPack || !strings.HasSuffix(strings.ToLower(f.FileName), ".jar") {
			continue
		}
		mc, client, server, loaders, ours := minecraft == "", false, false, 0, false
		for _, v := range f.GameVersions {
			switch {
			case v == minecraft:
				mc = true
			case strings.EqualFold(v, "Client"):
				client = true
			case strings.EqualFold(v, "Server"):
				server = true
			}
			if l, ok := cfLoaderNames[strings.ToLower(v)]; ok {
				loaders++
				ours = ours || l == loader
			}
		}
		if mc && (loaders == 0 || ours) && !(client && !server) {
			fits = append(fits, f)
		}
	}
	if len(fits) == 0 {
		return pluginFile{}, &noVersionError{title, minecraft}
	}
	sort.SliceStable(fits, func(i, j int) bool {
		a, b := fits[i], fits[j]
		if (a.ReleaseType == 1) != (b.ReleaseType == 1) {
			return a.ReleaseType == 1 // a release over a beta
		}
		return a.FileDate > b.FileDate
	})
	f := fits[0]
	if f.DownloadURL == "" {
		return pluginFile{}, cfNotDistributable(title, page)
	}
	if !cfDownloadable(f.DownloadURL) {
		return pluginFile{}, fmt.Errorf("CurseForge gave an unexpected download address for %s", title)
	}
	if f.sha1() == "" {
		return pluginFile{}, fmt.Errorf("CurseForge has no checked download of %s", title)
	}
	version := strings.TrimSuffix(f.DisplayName, ".jar")
	if version == "" {
		version = strings.TrimSuffix(f.FileName, ".jar")
	}
	switch f.ReleaseType {
	case 2:
		version += " (beta)"
	case 3:
		version += " (alpha)"
	}
	out := pluginFile{Source: SourceCurseForge, ID: strconv.Itoa(n), Title: title, Page: page, Version: version,
		URL: f.DownloadURL, Hash: f.sha1(), FileName: path.Base(strings.ReplaceAll(f.FileName, "\\", "/"))}
	for _, d := range f.Dependencies {
		if d.RelationType == 3 && d.ModID > 0 {
			out.Needs = append(out.Needs, pluginNeed{Source: SourceCurseForge, ID: strconv.Itoa(d.ModID)})
		}
	}
	return out, nil
}
