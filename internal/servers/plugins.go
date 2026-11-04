package servers

import (
	"archive/zip"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Plugins on Java servers. The owner finds them on Hangar (PaperMC's plugin
// site) and Modrinth, and the panel installs the newest release made for the
// server's Minecraft version, with the plugins it needs, checking each
// download against the site's checksum. Plugins live in plugins/ like any
// Paper server's; a turned-off one is renamed to .jar.disabled, which Paper
// skips. Changes apply when the server next starts.

// Default addresses of the plugin sites (Hangar's is DefaultHangarAPI).
const DefaultModrinthAPI = "https://api.modrinth.com/v2"

// Plugin sources.
const (
	SourceHangar   = "hangar"
	SourceModrinth = "modrinth"
)

const disabledSuffix = ".disabled"

// PluginResult is a plugin found by a search.
type PluginResult struct {
	Source      string `json:"source"`
	ID          string `json:"id"` // Hangar slug, or Modrinth project ID
	Name        string `json:"name"`
	Author      string `json:"author,omitempty"`
	Description string `json:"description,omitempty"`
	Icon        string `json:"icon,omitempty"`
	Page        string `json:"page,omitempty"`
	Downloads   int64  `json:"downloads"`
}

// AddedPlugin is a plugin the panel installed (kept in Meta).
type AddedPlugin struct {
	Source  string `json:"source"`
	ID      string `json:"id"`
	Title   string `json:"title"`
	Page    string `json:"page,omitempty"`
	Version string `json:"version"`
	File    string `json:"file"` // in plugins/, without .disabled
	SHA256  string `json:"sha256"`
	// For: installed because another plugin needs it (that plugin's title).
	For string `json:"for,omitempty"`
}

// InstalledPlugin is a plugin in a Java server's plugins folder.
type InstalledPlugin struct {
	File    string `json:"file"` // without .disabled
	Name    string `json:"name"` // its own name (plugin.yml)
	Version string `json:"version,omitempty"`
	Enabled bool   `json:"enabled"`
	// Console: Geyser, Floodgate or ViaVersion (console support; the panel
	// manages them).
	Console bool   `json:"console,omitempty"`
	Source  string `json:"source,omitempty"` // "" when added by hand
	ID      string `json:"id,omitempty"`
	Title   string `json:"title,omitempty"`
	Page    string `json:"page,omitempty"`
	For     string `json:"for,omitempty"`
	// Needs are the plugins it needs, by their own names.
	Needs []string `json:"needs,omitempty"`
}

// PluginsView is the Plugins tab of a Java server.
type PluginsView struct {
	Plugins       []InstalledPlugin `json:"plugins"`
	Minecraft     string            `json:"minecraft"`
	Running       bool              `json:"running"`
	RestartNeeded bool              `json:"restartNeeded"`
}

var (
	errNotJavaPlugins = errors.New("plugins are for Paper and Purpur servers (other Java servers use mods, in the Files tab)")
	errNoSuchPlugin   = errors.New("there's no such plugin on this server")
	errConsolePlugin  = errors.New("that's part of console support: turn it on or off under Settings, Console players")
)

// ---- the sites ----

// pluginFile is a plugin version to download.
type pluginFile struct {
	Source, ID, Title, Page string
	Version                 string
	URL                     string
	Hash                    string // SHA-256 (Hangar) or SHA-512 (Modrinth), hex
	FileName                string
	Needs                   []pluginNeed
}

// pluginNeed is a plugin another one needs.
type pluginNeed struct {
	Source, ID, Name string
	External         string // only downloadable from its own site
}

// SearchPlugins looks for Paper plugins made for a Minecraft version on both
// sites. An empty query lists the most downloaded. Results alternate
// between the sites; a site that can't be reached is left out and noted.
func (m *Manager) SearchPlugins(ctx context.Context, query, minecraft string) ([]PluginResult, []string) {
	query = strings.TrimSpace(query)
	if len([]rune(query)) > 100 {
		query = string([]rune(query)[:100])
	}
	var hangar, modrinth []PluginResult
	var herr, merr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); hangar, herr = m.searchHangar(ctx, query, minecraft) }()
	go func() { defer wg.Done(); modrinth, merr = m.searchModrinth(ctx, query, minecraft) }()
	wg.Wait()
	var notes []string
	if herr != nil {
		notes = append(notes, "Hangar couldn't be searched: "+herr.Error())
	}
	if merr != nil {
		notes = append(notes, "Modrinth couldn't be searched: "+merr.Error())
	}
	all := []PluginResult{}
	for i := 0; i < len(hangar) || i < len(modrinth); i++ {
		if i < len(hangar) {
			all = append(all, hangar[i])
		}
		if i < len(modrinth) {
			all = append(all, modrinth[i])
		}
	}
	if minecraft == "" {
		return all, notes
	}
	// The sites' searches match a plugin if any of its versions is for this
	// Minecraft; only keep the ones with a download the server can use.
	ok := make([]bool, len(all))
	sem := make(chan struct{}, 8)
	var cwg sync.WaitGroup
	for i, r := range all {
		cwg.Add(1)
		go func() {
			defer cwg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			_, err := m.latestPlugin(ctx, r.Source, r.ID, minecraft) // not the update check's cache
			var nv *noVersionError
			ok[i] = !errors.As(err, &nv)
		}()
	}
	cwg.Wait()
	out := []PluginResult{}
	hidden := 0
	for i, r := range all {
		if ok[i] {
			out = append(out, r)
		} else {
			hidden++
		}
	}
	if hidden > 0 {
		notes = append(notes, fmt.Sprintf("%d more found have no version for Minecraft %s yet, so they're not shown.", hidden, minecraft))
	}
	return out, notes
}

// noVersionError: a plugin has no download for the server's Minecraft
// version (yet).
type noVersionError struct{ title, minecraft string }

func (e *noVersionError) Error() string {
	return fmt.Sprintf("%s doesn't have a version for Minecraft %s yet", e.title, e.minecraft)
}

// paperLoaders are the Modrinth loaders whose plugins Paper and Purpur run.
const paperLoaders = `["paper","spigot","bukkit","purpur"]`

const searchLimit = 20

func (m *Manager) searchHangar(ctx context.Context, query, minecraft string) ([]PluginResult, error) {
	q := url.Values{"platform": {"PAPER"}, "limit": {fmt.Sprint(searchLimit)}, "offset": {"0"}}
	if minecraft != "" {
		q.Set("version", minecraft)
	}
	if query != "" {
		q.Set("q", query)
	} else {
		q.Set("sort", "-downloads")
	}
	var res struct {
		Result []struct {
			Name      string `json:"name"`
			Namespace struct {
				Owner string `json:"owner"`
				Slug  string `json:"slug"`
			} `json:"namespace"`
			Stats struct {
				Downloads int64 `json:"downloads"`
			} `json:"stats"`
			Description string `json:"description"`
			AvatarURL   string `json:"avatarUrl"`
		} `json:"result"`
	}
	if err := m.getJSON(ctx, m.opts.HangarAPI+"?"+q.Encode(), &res); err != nil {
		return nil, err
	}
	var out []PluginResult
	for _, p := range res.Result {
		if p.Namespace.Slug == "" {
			continue
		}
		out = append(out, PluginResult{
			Source: SourceHangar, ID: p.Namespace.Slug, Name: p.Name, Author: p.Namespace.Owner,
			Description: p.Description, Icon: httpsOnly(p.AvatarURL), Downloads: p.Stats.Downloads,
			Page: "https://hangar.papermc.io/" + url.PathEscape(p.Namespace.Owner) + "/" + url.PathEscape(p.Namespace.Slug),
		})
	}
	return out, nil
}

func (m *Manager) searchModrinth(ctx context.Context, query, minecraft string) ([]PluginResult, error) {
	facets := `[["project_type:plugin"],["categories:paper","categories:spigot","categories:bukkit","categories:purpur"]`
	if minecraft != "" {
		facets += `,["versions:` + minecraft + `"]`
	}
	facets += `]`
	q := url.Values{"limit": {fmt.Sprint(searchLimit)}, "facets": {facets}}
	if query != "" {
		q.Set("query", query)
	} else {
		q.Set("index", "downloads")
	}
	var res struct {
		Hits []struct {
			ProjectID   string `json:"project_id"`
			Slug        string `json:"slug"`
			Title       string `json:"title"`
			Author      string `json:"author"`
			Description string `json:"description"`
			IconURL     string `json:"icon_url"`
			Downloads   int64  `json:"downloads"`
		} `json:"hits"`
	}
	if err := m.getJSON(ctx, m.opts.ModrinthAPI+"/search?"+q.Encode(), &res); err != nil {
		return nil, err
	}
	var out []PluginResult
	for _, p := range res.Hits {
		if p.ProjectID == "" {
			continue
		}
		page := ""
		if p.Slug != "" {
			page = "https://modrinth.com/plugin/" + url.PathEscape(p.Slug)
		}
		out = append(out, PluginResult{
			Source: SourceModrinth, ID: p.ProjectID, Name: p.Title, Author: p.Author,
			Description: p.Description, Icon: httpsOnly(p.IconURL), Downloads: p.Downloads, Page: page,
		})
	}
	return out, nil
}

// httpsOnly keeps a picture address only if it's https (the page shows it).
func httpsOnly(u string) string {
	if strings.HasPrefix(u, "https://") {
		return u
	}
	return ""
}

var pluginIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,64}$`)

// latestPlugin finds the newest release of a plugin for a Minecraft
// version.
func (m *Manager) latestPlugin(ctx context.Context, source, id, minecraft string) (pluginFile, error) {
	if !pluginIDPattern.MatchString(id) {
		return pluginFile{}, errors.New("that isn't a plugin")
	}
	switch source {
	case SourceHangar:
		return m.latestHangar(ctx, id, minecraft)
	case SourceModrinth:
		return m.latestModrinth(ctx, id, minecraft)
	}
	return pluginFile{}, errors.New("plugins come from Hangar or Modrinth")
}

func (m *Manager) latestHangar(ctx context.Context, id, minecraft string) (pluginFile, error) {
	base := m.opts.HangarAPI + "/" + url.PathEscape(id)
	var project struct {
		Name      string `json:"name"`
		Namespace struct {
			Owner string `json:"owner"`
			Slug  string `json:"slug"`
		} `json:"namespace"`
	}
	if err := m.getJSON(ctx, base, &project); err != nil {
		return pluginFile{}, fmt.Errorf("couldn't find it on Hangar: %w", err)
	}
	type version struct {
		Name    string `json:"name"`
		Channel struct {
			Name string `json:"name"`
		} `json:"channel"`
		Downloads map[string]struct {
			FileInfo *struct {
				Name   string `json:"name"`
				SHA256 string `json:"sha256Hash"`
			} `json:"fileInfo"`
			ExternalURL string `json:"externalUrl"`
			DownloadURL string `json:"downloadUrl"`
		} `json:"downloads"`
		PluginDependencies map[string][]struct {
			Name        string `json:"name"`
			ProjectID   *int64 `json:"projectId"`
			Required    bool   `json:"required"`
			ExternalURL string `json:"externalUrl"`
		} `json:"pluginDependencies"`
	}
	var found *version
	for _, channel := range []string{"Release", ""} { // a release if there is one
		q := url.Values{"platform": {"PAPER"}, "limit": {"5"}, "offset": {"0"}}
		if minecraft != "" {
			q.Set("platformVersion", minecraft)
		}
		if channel != "" {
			q.Set("channel", channel)
		}
		var res struct {
			Result []version `json:"result"`
		}
		if err := m.getJSON(ctx, base+"/versions?"+q.Encode(), &res); err != nil {
			return pluginFile{}, fmt.Errorf("couldn't ask Hangar for its versions: %w", err)
		}
		for i := range res.Result {
			if _, ok := res.Result[i].Downloads["PAPER"]; ok {
				found = &res.Result[i]
				break
			}
		}
		if found != nil {
			break
		}
	}
	title := project.Name
	if title == "" {
		title = id
	}
	if found == nil {
		return pluginFile{}, &noVersionError{title, minecraft}
	}
	d := found.Downloads["PAPER"]
	if d.DownloadURL == "" || d.FileInfo == nil || len(d.FileInfo.SHA256) != 64 {
		if d.ExternalURL != "" {
			return pluginFile{}, fmt.Errorf("%s is downloaded from its own site, so the panel can't check the download; get it from %s", title, d.ExternalURL)
		}
		return pluginFile{}, fmt.Errorf("Hangar has no checked download of %s", title)
	}
	label := found.Name
	if c := found.Channel.Name; c != "" && !strings.EqualFold(c, "Release") {
		label += " (" + strings.ToLower(c) + ")"
	}
	f := pluginFile{
		Source: SourceHangar, ID: id, Title: title, Version: label, URL: d.DownloadURL,
		Hash: strings.ToLower(d.FileInfo.SHA256), FileName: d.FileInfo.Name,
	}
	if project.Namespace.Slug != "" {
		f.ID = project.Namespace.Slug
		f.Page = "https://hangar.papermc.io/" + url.PathEscape(project.Namespace.Owner) + "/" + url.PathEscape(project.Namespace.Slug)
	}
	for _, dep := range found.PluginDependencies["PAPER"] {
		if !dep.Required {
			continue
		}
		n := pluginNeed{Source: SourceHangar, Name: dep.Name, External: dep.ExternalURL}
		if dep.ProjectID != nil {
			n.ID, n.External = fmt.Sprint(*dep.ProjectID), ""
		}
		f.Needs = append(f.Needs, n)
	}
	return f, nil
}

func (m *Manager) latestModrinth(ctx context.Context, id, minecraft string) (pluginFile, error) {
	return m.latestModrinthFor(ctx, id, minecraft, "paper")
}

// latestModrinthFor is latestModrinth for a loader ("paper", "fabric"…).
func (m *Manager) latestModrinthFor(ctx context.Context, id, minecraft, loader string) (pluginFile, error) {
	var project struct {
		ID    string `json:"id"`
		Slug  string `json:"slug"`
		Title string `json:"title"`
	}
	if err := m.getJSON(ctx, m.opts.ModrinthAPI+"/project/"+url.PathEscape(id), &project); err != nil {
		return pluginFile{}, fmt.Errorf("couldn't find it on Modrinth: %w", err)
	}
	loaders := `["` + loader + `"]`
	if loader == "paper" {
		loaders = paperLoaders // Paper runs Spigot and Bukkit plugins too
	}
	q := url.Values{"loaders": {loaders}}
	if minecraft != "" {
		q.Set("game_versions", `["`+minecraft+`"]`)
	}
	var versions []struct {
		Name          string `json:"name"`
		VersionNumber string `json:"version_number"`
		VersionType   string `json:"version_type"`
		Files         []struct {
			URL      string            `json:"url"`
			FileName string            `json:"filename"`
			Hashes   map[string]string `json:"hashes"`
			Primary  bool              `json:"primary"`
		} `json:"files"`
		Dependencies []struct {
			ProjectID      string `json:"project_id"`
			DependencyType string `json:"dependency_type"`
		} `json:"dependencies"`
	}
	if err := m.getJSON(ctx, m.opts.ModrinthAPI+"/project/"+url.PathEscape(id)+"/version?"+q.Encode(), &versions); err != nil {
		return pluginFile{}, fmt.Errorf("couldn't ask Modrinth for its versions: %w", err)
	}
	title := project.Title
	if title == "" {
		title = id
	}
	pick := -1
	for i, v := range versions { // newest first; a release if there is one
		if v.VersionType == "release" {
			pick = i
			break
		}
		if pick < 0 {
			pick = i
		}
	}
	if pick < 0 {
		return pluginFile{}, &noVersionError{title, minecraft}
	}
	v := versions[pick]
	fi := -1
	for i, file := range v.Files {
		if strings.HasSuffix(strings.ToLower(file.FileName), ".jar") && (fi < 0 || file.Primary) {
			fi = i
		}
	}
	if fi < 0 || len(v.Files[fi].Hashes["sha512"]) != 128 || v.Files[fi].URL == "" {
		return pluginFile{}, fmt.Errorf("Modrinth has no checked download of %s", title)
	}
	file := v.Files[fi]
	version := v.VersionNumber
	if v.VersionType != "" && v.VersionType != "release" {
		version += " (" + v.VersionType + ")"
	}
	f := pluginFile{
		Source: SourceModrinth, ID: project.ID, Title: title, Version: version,
		URL: file.URL, Hash: strings.ToLower(file.Hashes["sha512"]), FileName: file.FileName,
	}
	if f.ID == "" {
		f.ID = id
	}
	if project.Slug != "" {
		f.Page = "https://modrinth.com/plugin/" + url.PathEscape(project.Slug)
	}
	for _, d := range v.Dependencies {
		if d.DependencyType == "required" && d.ProjectID != "" {
			f.Needs = append(f.Needs, pluginNeed{Source: SourceModrinth, ID: d.ProjectID})
		}
	}
	return f, nil
}

// ---- what's installed ----

func (s *Server) pluginsDir() string { return filepath.Join(s.dir(), "plugins") }

// pluginRecordsFile lists the plugins the panel installed. It lives in
// plugins/ (Paper ignores it), so backups and restores keep it in step with
// the jars.
// Named before the panel was called Block Party (kept for existing servers).
const pluginRecordsFile = ".blockheads-plugins.json"

func (s *Server) pluginRecords() []AddedPlugin {
	list, _ := readJSONList[AddedPlugin](filepath.Join(s.pluginsDir(), pluginRecordsFile))
	return list
}

// savePluginRecords writes the records. Caller holds pluginsMu.
func (s *Server) savePluginRecords(list []AddedPlugin) error {
	if err := os.MkdirAll(s.pluginsDir(), 0o755); err != nil {
		return err
	}
	return writeJSONList(filepath.Join(s.pluginsDir(), pluginRecordsFile), list)
}

// pluginInfo reads a plugin's name and version from its plugin.yml (or
// paper-plugin.yml).
func pluginInfo(path string) (name, version string, err error) {
	name, version, _, err = pluginMeta(path)
	return name, version, err
}

// pluginMeta also returns the plugins it needs (plugin.yml's depend).
func pluginMeta(path string) (name, version string, depends []string, err error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return "", "", nil, errors.New("it isn't a plugin (not a .jar file)")
	}
	defer r.Close()
	for _, want := range []string{"paper-plugin.yml", "plugin.yml"} {
		for _, f := range r.File {
			if f.Name != want {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return "", "", nil, err
			}
			sc := bufio.NewScanner(io.LimitReader(rc, 1<<20))
			inDepend := false
			for sc.Scan() {
				line := sc.Text()
				if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "-") {
					// depend:\n  - Vault
					if t := strings.TrimSpace(line); inDepend && strings.HasPrefix(t, "-") {
						depends = append(depends, strings.Trim(strings.TrimSpace(t[1:]), `"'`))
					}
					continue
				}
				inDepend = false
				k, v, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				v = strings.Trim(strings.TrimSpace(v), `"'`)
				switch strings.TrimSpace(k) {
				case "name":
					name = v
				case "version":
					version = v
				case "depend":
					inDepend = v == ""
					// depend: [Vault, WorldEdit]
					for _, d := range strings.Split(strings.Trim(v, "[]"), ",") {
						if d = strings.Trim(strings.TrimSpace(d), `"'`); d != "" {
							depends = append(depends, d)
						}
					}
				}
			}
			rc.Close()
			if name != "" {
				return name, version, depends, nil
			}
		}
	}
	return "", "", nil, errors.New("it isn't a Paper plugin (no plugin.yml)")
}

// jarEntry is a plugin file in plugins/.
type jarEntry struct {
	file    string // without .disabled
	enabled bool
	name    string // from plugin.yml ("" if unreadable)
	version string
	depends []string
	console bool
}

func (j jarEntry) path(dir string) string {
	if j.enabled {
		return filepath.Join(dir, j.file)
	}
	return filepath.Join(dir, j.file+disabledSuffix)
}

// pluginJars lists the plugin jars, by file name.
func (s *Server) pluginJars() (map[string]jarEntry, error) {
	dir := s.pluginsDir()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]jarEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]jarEntry{}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		n := e.Name()
		lower := strings.ToLower(n)
		var j jarEntry
		switch {
		case strings.HasSuffix(lower, ".jar"):
			j = jarEntry{file: n, enabled: true}
		case strings.HasSuffix(lower, ".jar"+disabledSuffix):
			j = jarEntry{file: strings.TrimSuffix(n, disabledSuffix)}
			if _, on := out[j.file]; on {
				continue // both: the one Paper loads counts
			}
		default:
			continue
		}
		j.console = isConsolePluginFile(j.file)
		j.name, j.version, j.depends, _ = pluginMeta(j.path(dir))
		out[j.file] = j
	}
	return out, nil
}

// pluginFiles lists the plugin jars (file name → turned on).
func (s *Server) pluginFiles() (map[string]bool, error) {
	jars, err := s.pluginJars()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for f, j := range jars {
		out[f] = j.enabled
	}
	return out, nil
}

func isConsolePluginFile(file string) bool {
	for _, p := range consolePlugins {
		if strings.EqualFold(p.File, file) {
			return true
		}
	}
	return false
}

// consolePluginNames are console support's plugins by their own names.
var consolePluginNames = map[string]string{"geyser-spigot": "Geyser", "floodgate": "Floodgate", "viaversion": "ViaVersion"}

type jarSum struct {
	size int64
	mod  time.Time
	sum  string
}

// jarSHA256 returns a file's SHA-256, remembered while the file is
// unchanged.
func (s *Server) jarSHA256(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if c, ok := s.jarSums.Load(path); ok {
		if c := c.(jarSum); c.size == st.Size() && c.mod.Equal(st.ModTime()) {
			return c.sum
		}
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return ""
	}
	s.jarSums.Store(path, jarSum{size: st.Size(), mod: st.ModTime(), sum: sum})
	return sum
}

// matchedRecords pairs the records with the jars they describe: same file,
// same contents. A jar changed or put back by hand isn't the panel's any
// more.
func (s *Server) matchedRecords(jars map[string]jarEntry) map[string]AddedPlugin {
	out := map[string]AddedPlugin{}
	for _, a := range s.pluginRecords() {
		j, ok := jars[a.File]
		if ok && !j.console && s.jarSHA256(j.path(s.pluginsDir())) == a.SHA256 {
			out[a.File] = a
		}
	}
	return out
}

// Plugins returns the Plugins tab of a Java server.
func (s *Server) Plugins() (PluginsView, error) {
	if !s.isJava() || !runsPlugins(s.Loader()) {
		return PluginsView{}, errNotJavaPlugins
	}
	jars, err := s.pluginJars()
	if err != nil {
		return PluginsView{}, err
	}
	added := s.matchedRecords(jars)
	s.mu.Lock()
	v := PluginsView{Plugins: []InstalledPlugin{}, Minecraft: s.meta.Version, Running: s.cmd != nil}
	v.RestartNeeded = s.cmd != nil && s.pluginsChanged.After(s.startedAt)
	s.mu.Unlock()
	for file, j := range jars {
		p := InstalledPlugin{File: file, Enabled: j.enabled, Console: j.console, Name: j.name, Version: j.version, Needs: j.depends}
		if p.Name == "" {
			p.Name = strings.TrimSuffix(file, filepath.Ext(file))
		}
		if a, ok := added[file]; ok {
			p.Source, p.ID, p.Title, p.Page, p.For = a.Source, a.ID, a.Title, a.Page, a.For
			p.Version = a.Version
		}
		v.Plugins = append(v.Plugins, p)
	}
	sort.Slice(v.Plugins, func(i, j int) bool {
		a, b := v.Plugins[i], v.Plugins[j]
		if a.Console != b.Console {
			return !a.Console // console support last
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return v, nil
}

// ---- changes ----

// pluginsEditable says whether plugins can be changed now, and returns the
// server's Minecraft version (which installs and updates need to know).
func (s *Server) pluginsEditable(needVersion bool) (string, error) {
	if !s.isJava() || !runsPlugins(s.Loader()) {
		return "", errNotJavaPlugins
	}
	if err := s.EditBlocked(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		return "", errBusy
	}
	if needVersion && s.meta.Version == "" {
		return "", errors.New("the server's Minecraft version isn't known yet; start it once first")
	}
	return s.meta.Version, nil
}

var errPluginsBusy = errors.New("a backup, restore or update of this server is running; try again in a moment")

// lockPlugins takes the locks for changing plugins/ (nothing else may copy
// or swap the folder meanwhile) and checks again that changes are allowed.
// minecraft, if given, must still be the server's version. Call the
// returned function to unlock.
func (s *Server) lockPlugins(minecraft string) (func(), error) {
	if !s.backupMu.TryLock() {
		return nil, errPluginsBusy
	}
	s.pluginsMu.Lock()
	unlock := func() { s.pluginsMu.Unlock(); s.backupMu.Unlock() }
	version, err := s.pluginsEditable(minecraft != "")
	if err == nil && minecraft != "" && version != minecraft {
		err = errors.New("the server's Minecraft version changed meanwhile; try again")
	}
	if err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

func (s *Server) notePluginsChanged() {
	s.mu.Lock()
	s.pluginsChanged = time.Now()
	s.mu.Unlock()
}

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9._+\-]+`)

// pluginFileName makes a safe, unused file name for a download.
func (s *Server) pluginFileName(name string, files map[string]bool, replacing string) string {
	name = unsafeFileChars.ReplaceAllString(filepath.Base(name), "_")
	name = strings.TrimLeft(name, ".")
	if !strings.HasSuffix(strings.ToLower(name), ".jar") {
		name += ".jar"
	}
	if len(name) > 100 {
		name = name[len(name)-100:]
	}
	if name == ".jar" || name == "" {
		name = "plugin.jar"
	}
	base := strings.TrimSuffix(name, ".jar")
	for i := 2; ; i++ {
		_, taken := files[name]
		if (!taken || name == replacing) && !isConsolePluginFile(name) {
			return name
		}
		name = fmt.Sprintf("%s-%d.jar", base, i)
	}
}

// planned is a plugin downloaded and ready to go into plugins/.
type planned struct {
	f    pluginFile
	tmp  string
	name string // its own name
	sum  string // SHA-256
	For  string
}

// pluginPlan is what an install or update puts in: the plugins it needs
// first, then the plugin itself.
type pluginPlan struct {
	items  []planned
	notes  []string
	relies []string // turned-on plugin files it counts on being there
}

func (p *pluginPlan) rely(j jarEntry) {
	if j.file != "" && j.enabled {
		p.relies = append(p.relies, j.file)
	}
}

func (p *pluginPlan) discard() {
	for _, it := range p.items {
		os.Remove(it.tmp)
	}
	p.items = nil
}

// planPlugin downloads a plugin and the plugins it needs that aren't here
// yet. Nothing in plugins/ changes. replacing is the file an update
// replaces.
func (s *Server) planPlugin(ctx context.Context, root pluginFile, minecraft, replacing string) (*pluginPlan, error) {
	jars, err := s.pluginJars()
	if err != nil {
		return nil, err
	}
	present := map[string]jarEntry{} // by its own name, lower case
	for _, j := range jars {
		if j.name != "" && j.file != replacing {
			present[strings.ToLower(j.name)] = j
		}
	}
	records := s.matchedRecords(jars)
	plan := &pluginPlan{}
	visited := map[string]bool{}
	plannedNames := map[string]bool{} // own names
	var visit func(f pluginFile, forTitle string, depth int) error
	visit = func(f pluginFile, forTitle string, depth int) error {
		visited[f.Source+"/"+strings.ToLower(f.ID)] = true
		dep := forTitle != ""
		if depth > 6 {
			return errors.New("its plugins need too many other plugins")
		}
		if dep {
			for _, a := range records {
				if a.Source == f.Source && strings.EqualFold(a.ID, f.ID) {
					plan.rely(jars[a.File])
					s.noteIfOff(plan, jars[a.File], f.Title, forTitle)
					return nil // already here
				}
			}
		}
		tmp, err := s.m.downloadVerified(ctx, f.URL, s.m.serversDir(), f.Hash, func(string) {}, s.m.javaUserAgent())
		if err != nil {
			return fmt.Errorf("%s: %w", f.Title, err)
		}
		it := planned{f: f, tmp: tmp, For: forTitle}
		keep := false
		defer func() {
			if !keep {
				os.Remove(tmp)
			}
		}()
		if it.name, _, err = pluginInfo(tmp); err != nil {
			return fmt.Errorf("%s: %w", f.Title, err)
		}
		key := strings.ToLower(it.name)
		if console, ok := consolePluginNames[key]; ok {
			if !dep {
				return fmt.Errorf("%s is part of console support, which the panel keeps installed (Settings, Console players)", it.name)
			}
			if j, ok := present[key]; ok && j.enabled {
				plan.rely(j)
				return nil // console support has it (or a copy put in by hand)
			}
			return fmt.Errorf("%s needs %s, which comes with console support: turn on Console players in Settings first", forTitle, console)
		}
		if j, ok := present[key]; ok {
			if !dep {
				if j.console {
					return fmt.Errorf("%s is part of console support", it.name)
				}
				if _, ours := records[j.file]; ours {
					return fmt.Errorf("%s is already installed", f.Title)
				}
				return errByHand{name: it.name, file: j.file}
			}
			plan.rely(j)
			s.noteIfOff(plan, j, it.name, forTitle)
			return nil // here already (perhaps added by hand)
		}
		if plannedNames[key] {
			return nil
		}
		if it.sum, err = fileSHA256(tmp); err != nil {
			return err
		}
		// What it needs goes in first.
		for _, n := range f.Needs {
			if n.ID == "" {
				where := ""
				if n.External != "" {
					where = ": " + n.External
				}
				plan.notes = append(plan.notes, fmt.Sprintf("%s also needs %s, which the panel can't install (look for it, or get it from its own site%s).", f.Title, n.Name, where))
				continue
			}
			if visited[n.Source+"/"+strings.ToLower(n.ID)] {
				continue
			}
			nf, err := s.m.latestPlugin(ctx, n.Source, n.ID, minecraft)
			if err != nil {
				return fmt.Errorf("%s needs another plugin that can't be installed: %w", f.Title, err)
			}
			if err := visit(nf, f.Title, depth+1); err != nil {
				return err
			}
		}
		plannedNames[key] = true
		keep = true
		plan.items = append(plan.items, it)
		return nil
	}
	if err := visit(root, "", 0); err != nil {
		plan.discard()
		return nil, err
	}
	return plan, nil
}

func (s *Server) noteIfOff(plan *pluginPlan, j jarEntry, name, forTitle string) {
	if j.file != "" && !j.enabled {
		plan.notes = append(plan.notes, fmt.Sprintf("%s needs %s, which is turned off here: turn it on too.", forTitle, name))
	}
}

// commitPlugins puts a plan's plugins in plugins/, all or none. Caller holds
// the plugin locks. replacing is the file an update replaces (the last item
// replaces it).
func (s *Server) commitPlugins(plan *pluginPlan, replacing string) error {
	jars, err := s.pluginJars()
	if err != nil {
		return err
	}
	files := map[string]bool{}
	for f, j := range jars {
		files[f] = j.enabled
	}
	// Something may have been removed or turned off, or added, meanwhile.
	for _, f := range plan.relies {
		if j, ok := jars[f]; !ok || !j.enabled {
			return fmt.Errorf("%s was removed or turned off meanwhile; try again", f)
		}
	}
	for _, it := range plan.items {
		for _, j := range jars {
			if j.file != replacing && strings.EqualFold(j.name, it.name) {
				return fmt.Errorf("%s was added meanwhile; try again", it.name)
			}
		}
	}
	if err := os.MkdirAll(s.pluginsDir(), 0o755); err != nil {
		return err
	}
	records := s.pluginRecords()
	var placed []string
	undo := func() {
		for _, p := range placed {
			os.Remove(p)
		}
	}
	for i, it := range plan.items {
		repl := ""
		if i == len(plan.items)-1 {
			repl = replacing
		}
		file := s.pluginFileName(it.f.FileName, files, repl)
		target := filepath.Join(s.pluginsDir(), file)
		if repl != "" && !files[repl] {
			target += disabledSuffix // it was turned off: the new one stays off
		}
		if repl != "" && file == repl {
			// Same name: keep the old one until the new one is in.
			if err := os.Rename(target, target+".old"); err != nil && !errors.Is(err, os.ErrNotExist) {
				undo()
				return err
			}
		}
		_ = os.Chmod(it.tmp, 0o644)
		if err := os.Rename(it.tmp, target); err != nil {
			if repl != "" && file == repl {
				_ = os.Rename(target+".old", target)
			}
			undo()
			return err
		}
		placed = append(placed, target)
		files[file] = true
		rec := AddedPlugin{Source: it.f.Source, ID: it.f.ID, Title: it.f.Title, Page: it.f.Page, Version: it.f.Version, File: file, SHA256: it.sum, For: it.For}
		if repl != "" {
			old := filepath.Join(s.pluginsDir(), repl)
			_ = os.Remove(old + disabledSuffix + ".old")
			_ = os.Remove(old + ".old")
			if file != repl {
				_ = os.Remove(old)
				_ = os.Remove(old + disabledSuffix)
			}
			kept := records[:0]
			for _, a := range records {
				if a.File == repl {
					rec.For = a.For // still needed by whatever needed it
					continue
				}
				kept = append(kept, a)
			}
			records = kept
		}
		records = append(records, rec)
	}
	// Records of jars that are gone are forgotten.
	now, _ := s.pluginJars()
	kept := records[:0]
	for _, a := range records {
		if _, ok := now[a.File]; ok {
			kept = append(kept, a)
		}
	}
	if err := s.savePluginRecords(kept); err != nil {
		return err
	}
	for _, it := range plan.items {
		what := it.f.Title + " " + it.f.Version
		if it.For != "" {
			what += " (" + it.For + " needs it)"
		}
		if replacing != "" && it.For == "" {
			s.say("Updated the plugin " + what + ". The new version starts the next time the server does.")
		} else {
			s.say("Installed the plugin " + what + ". It starts the next time the server does.")
		}
		s.m.log.Info("plugin installed", "id", s.meta.ID, "plugin", it.name, "version", it.f.Version, "source", it.f.Source)
	}
	for _, n := range plan.notes {
		s.say(n)
	}
	s.notePluginsChanged()
	return nil
}

// errByHand: the same plugin is already in plugins/, put there by hand.
type errByHand struct{ name, file string }

func (e errByHand) Error() string {
	return fmt.Sprintf("%s is already installed by hand (%s)", e.name, e.file)
}

// InstallPlugin installs the newest release of a plugin for the server's
// Minecraft version, and the plugins it needs. It returns the plugin's
// title.
func (s *Server) InstallPlugin(source, id string) (string, error) {
	minecraft, err := s.pluginsEditable(true)
	if err != nil {
		return "", err
	}
	if !s.m.beginWork() {
		return "", errShuttingDown
	}
	defer s.m.work.Done()
	ctx, cancel := context.WithTimeout(s.m.ctx, 5*time.Minute)
	defer cancel()
	f, err := s.m.latestPlugin(ctx, source, id, minecraft)
	if err != nil {
		return "", err
	}
	s.say(fmt.Sprintf("Downloading the plugin %s %s…", f.Title, f.Version))
	plan, err := s.planPlugin(ctx, f, minecraft, "")
	if err != nil {
		return "", err
	}
	defer plan.discard()
	unlock, err := s.lockPlugins(minecraft)
	if err != nil {
		return "", err
	}
	defer unlock()
	if err := s.commitPlugins(plan, ""); err != nil {
		return "", err
	}
	return f.Title, nil
}

// pluginUpdateCache remembers the newest release of each plugin for an hour,
// so opening the Plugins tab doesn't ask the sites every time.
type pluginUpdateCache struct {
	mu   sync.Mutex
	at   map[string]time.Time
	file map[string]pluginFile
}

func (m *Manager) cachedLatestPlugin(ctx context.Context, source, id, minecraft string) (pluginFile, error) {
	key := source + "/" + strings.ToLower(id) + "/" + minecraft
	c := &m.pluginUpdates
	c.mu.Lock()
	if f, ok := c.file[key]; ok && time.Since(c.at[key]) < versionCheckEvery {
		c.mu.Unlock()
		return f, nil
	}
	c.mu.Unlock()
	f, err := m.latestPlugin(ctx, source, id, minecraft)
	if err != nil {
		return f, err
	}
	c.mu.Lock()
	if c.file == nil {
		c.file, c.at = map[string]pluginFile{}, map[string]time.Time{}
	}
	c.file[key], c.at[key] = f, time.Now()
	c.mu.Unlock()
	return f, nil
}

// PluginUpdates returns the plugins the panel installed that have a newer
// release for the server's Minecraft version (file → new version).
func (s *Server) PluginUpdates(ctx context.Context) map[string]string {
	out := map[string]string{}
	if !s.isJava() || !runsPlugins(s.Loader()) {
		return out
	}
	jars, err := s.pluginJars()
	if err != nil {
		return out
	}
	s.mu.Lock()
	minecraft := s.meta.Version
	s.mu.Unlock()
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, a := range s.matchedRecords(jars) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			f, err := s.m.cachedLatestPlugin(ctx, a.Source, a.ID, minecraft)
			if err != nil || f.Version == a.Version || f.Hash == "" {
				return
			}
			mu.Lock()
			out[a.File] = f.Version
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// checkPluginFile checks a file name from the page: a plugin in plugins/
// that isn't console support.
func (s *Server) checkPluginFile(file string) (jarEntry, error) {
	if file == "" || filepath.Base(file) != file || strings.HasPrefix(file, ".") || !strings.HasSuffix(strings.ToLower(file), ".jar") {
		return jarEntry{}, errNoSuchPlugin
	}
	jars, err := s.pluginJars()
	if err != nil {
		return jarEntry{}, err
	}
	j, ok := jars[file]
	if !ok {
		return jarEntry{}, errNoSuchPlugin
	}
	if j.console {
		return jarEntry{}, errConsolePlugin
	}
	return j, nil
}

// UpdatePlugin moves a plugin the panel installed to its newest release
// (and adds plugins the new release needs). It returns the new version.
func (s *Server) UpdatePlugin(file string) (string, error) {
	minecraft, err := s.pluginsEditable(true)
	if err != nil {
		return "", err
	}
	if !s.m.beginWork() {
		return "", errShuttingDown
	}
	defer s.m.work.Done()
	ctx, cancel := context.WithTimeout(s.m.ctx, 5*time.Minute)
	defer cancel()
	j, err := s.checkPluginFile(file)
	if err != nil {
		return "", err
	}
	a, ok := s.matchedRecords(map[string]jarEntry{file: j})[file]
	if !ok {
		return "", errors.New("that plugin was added by hand (or changed since), so the panel can't update it; remove it and install it here instead")
	}
	f, err := s.m.latestPlugin(ctx, a.Source, a.ID, minecraft)
	if err != nil {
		return "", err
	}
	if f.Version == a.Version {
		s.say(a.Title + " is already on its newest version (" + a.Version + ").")
		return a.Version, nil
	}
	s.say(fmt.Sprintf("Downloading the plugin %s %s…", f.Title, f.Version))
	plan, err := s.planPlugin(ctx, f, minecraft, file)
	if err != nil {
		return "", err
	}
	defer plan.discard()
	unlock, err := s.lockPlugins(minecraft)
	if err != nil {
		return "", err
	}
	defer unlock()
	if _, err := s.checkPluginFile(file); err != nil {
		return "", err
	}
	if err := s.commitPlugins(plan, file); err != nil {
		return "", err
	}
	return f.Version, nil
}

// SetPluginEnabled turns a plugin off (Paper skips it) or on again.
func (s *Server) SetPluginEnabled(file string, on bool) error {
	unlock, err := s.lockPlugins("")
	if err != nil {
		return err
	}
	defer unlock()
	j, err := s.checkPluginFile(file)
	if err != nil {
		return err
	}
	if j.enabled == on {
		return nil
	}
	path := filepath.Join(s.pluginsDir(), file)
	if on {
		err = os.Rename(path+disabledSuffix, path)
	} else {
		err = os.Rename(path, path+disabledSuffix)
	}
	if err != nil {
		return err
	}
	s.notePluginsChanged()
	name := j.name
	if name == "" {
		name = file
	}
	if on {
		s.say(name + " is turned on; it starts the next time the server does.")
	} else {
		s.say(name + " is turned off from the next start.")
	}
	return nil
}

// RemovePlugin deletes a plugin. Its settings folder stays, so adding it
// again keeps them.
func (s *Server) RemovePlugin(file string) error {
	unlock, err := s.lockPlugins("")
	if err != nil {
		return err
	}
	defer unlock()
	j, err := s.checkPluginFile(file)
	if err != nil {
		return err
	}
	jars, _ := s.pluginJars()
	removed, ours := s.matchedRecords(jars)[file]
	path := filepath.Join(s.pluginsDir(), file)
	for _, p := range []string{path, path + disabledSuffix} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	records := s.pluginRecords()
	kept := records[:0]
	for _, a := range records {
		if a.File == file {
			continue
		}
		if ours && a.For == removed.Title {
			a.For = "" // what it was installed for is gone
		}
		kept = append(kept, a)
	}
	if err := s.savePluginRecords(kept); err != nil {
		return err
	}
	s.notePluginsChanged()
	name := j.name
	if name == "" {
		name = file
	}
	s.say("Removed the plugin " + name + ". Its settings folder in plugins/ is kept. The server keeps running it until it restarts.")
	return nil
}
