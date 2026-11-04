package servers

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Consoles on Java servers. Three plugins let Minecraft Bedrock (Xbox,
// PlayStation, Switch, phones, tablets, Windows) join a Java server:
//
//   - Geyser (geysermc.org) translates between Bedrock and Java. It listens
//     for Bedrock players on the server's port (UDP; Java players use TCP).
//   - Floodgate lets those players in with their Xbox sign-in, without a
//     Java account. Their names start with "." (".Gamer_Tag"), and their
//     account ID is made from their Xbox ID.
//   - ViaVersion lets Geyser in when the server is on a slightly older
//     Minecraft version than Geyser speaks (Paper often is for a while).
//
// The panel installs and updates them itself (they're on by default; each
// Java server can turn them off). Bedrock gets updated often and consoles
// update by themselves, so Geyser is kept current: Update now and the
// overnight updates fetch new builds.

// Default addresses the console plugins come from.
const (
	DefaultGeyserAPI  = "https://download.geysermc.org/v2/projects"
	DefaultHangarAPI  = "https://hangar.papermc.io/api/v1/projects"
	DefaultXboxLookup = "https://api.geysermc.org/v2/xbox/xuid/"
)

// consolePlugin is one of the plugins the panel manages. Its file name in
// plugins/ is fixed, so the panel only ever touches its own files.
type consolePlugin struct {
	Key      string // in Meta.Plugins
	Name     string // its own name (plugin.yml)
	File     string
	Label    string
	Required bool   // without it consoles can't join
	Dir      string // plugins (Paper, Purpur) or mods (Fabric)
	// Source: "geyser:<platform>" (download.geysermc.org), "hangar" (Hangar's
	// ViaVersion), or "modrinth:<project>" (for the server's Minecraft
	// version and mod loader).
	Source string
}

var consolePlugins = []consolePlugin{
	{Key: "geyser", Name: "Geyser-Spigot", File: "Geyser-Spigot.jar", Label: "Geyser", Required: true, Dir: "plugins", Source: "geyser:spigot"},
	{Key: "floodgate", Name: "floodgate", File: "floodgate-spigot.jar", Label: "Floodgate", Required: true, Dir: "plugins", Source: "geyser:spigot"},
	{Key: "viaversion", Name: "ViaVersion", File: "ViaVersion.jar", Label: "ViaVersion", Dir: "plugins", Source: "hangar"},
}

// fabricConsolePlugins: Geyser and Floodgate's Fabric mods, and the Fabric
// API they need. A copy put in mods/ by hand (fabric-api-0.x.jar) is used
// instead of the panel's own.
var fabricConsolePlugins = []consolePlugin{
	{Key: "geyser", Name: "geyser", File: "Geyser-Fabric.jar", Label: "Geyser", Required: true, Dir: "mods", Source: "geyser:fabric"},
	{Key: "floodgate", Name: "floodgate", File: "floodgate-fabric.jar", Label: "Floodgate", Required: true, Dir: "mods", Source: "modrinth:floodgate"},
	{Key: "fabric-api", Name: "fabric-api", File: "fabric-api.jar", Label: "Fabric API", Required: true, Dir: "mods", Source: "modrinth:fabric-api"},
}

// consoleSet is console support's plugins (or mods) for a server type; nil
// for types consoles can't join.
func consoleSet(loader string) []consolePlugin {
	switch loader {
	case LoaderPaper, LoaderPurpur, "":
		return consolePlugins
	case LoaderFabric:
		return fabricConsolePlugins
	}
	return nil
}

// consoleSetLocked is the server's. Caller holds s.mu.
func (s *Server) consoleSetLocked() []consolePlugin { return consoleSet(s.loaderLocked()) }

func (s *Server) consoleSet() []consolePlugin {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.consoleSetLocked()
}

// modrinthLoader is the loader name Modrinth uses for a server type.
func modrinthLoader(loader string) string {
	if runsMods(loader) {
		return loader
	}
	return "paper"
}

// pluginRelease is a plugin build to download.
type pluginRelease struct {
	Version string
	URL     string
	SHA256  string
}

func (r pluginRelease) id() string { return strings.ToLower(r.SHA256) }

// consolesOn reports whether console players are wanted on this server.
// Caller holds s.mu.
func (s *Server) consolesOnLocked() bool {
	return s.isJava() && !s.meta.NoConsoles && len(s.consoleSetLocked()) > 0
}

// ConsolesOn reports whether console players are wanted on this server.
func (s *Server) ConsolesOn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.consolesOnLocked()
}

// consolesInstalled reports whether Geyser and Floodgate are in plugins/.
func (s *Server) consolesInstalled() bool { return s.consolesInstalledIn(s.consoleSet()) }

// consolesInstalledLocked is consolesInstalled for callers holding s.mu.
func (s *Server) consolesInstalledLocked() bool { return s.consolesInstalledIn(s.consoleSetLocked()) }

func (s *Server) consolesInstalledIn(set []consolePlugin) bool {
	if len(set) == 0 {
		return false
	}
	for _, p := range set {
		if p.Required && !fileExists(filepath.Join(s.dir(), p.Dir, p.File)) && s.otherModCopy(p) == "" {
			return false
		}
	}
	return true
}

// otherModCopy returns the file name of a copy of a Fabric console mod put
// in mods/ by hand (fabric-api-0.92.jar), which is used instead of the
// panel's own, or "".
func (s *Server) otherModCopy(p consolePlugin) string {
	if p.Dir != "mods" {
		return ""
	}
	entries, _ := os.ReadDir(filepath.Join(s.dir(), "mods"))
	prefix := strings.ToLower(p.Name)
	for _, e := range entries {
		n := strings.ToLower(e.Name())
		if strings.EqualFold(e.Name(), p.File) || !strings.HasSuffix(n, ".jar") || !strings.HasPrefix(n, prefix) {
			continue
		}
		return e.Name()
	}
	return ""
}

// ---- where the plugins come from ----

// latestGeyserProject asks GeyserMC for the newest build of geyser or
// floodgate for a platform ("spigot" for Paper, "fabric").
func (m *Manager) latestGeyserProject(ctx context.Context, project, platform string) (pluginRelease, error) {
	var b struct {
		Version   string `json:"version"`
		Build     int    `json:"build"`
		Downloads map[string]struct {
			Name   string `json:"name"`
			SHA256 string `json:"sha256"`
		} `json:"downloads"`
	}
	base := m.opts.GeyserAPI + "/" + project
	if err := m.getJSON(ctx, base+"/versions/latest/builds/latest", &b); err != nil {
		return pluginRelease{}, err
	}
	d, ok := b.Downloads[platform]
	if !ok || b.Version == "" || b.Build <= 0 || len(d.SHA256) != 64 {
		return pluginRelease{}, fmt.Errorf("geysermc.org has no %s download of %s", platform, project)
	}
	return pluginRelease{
		Version: fmt.Sprintf("%s (build %d)", b.Version, b.Build),
		URL:     fmt.Sprintf("%s/versions/%s/builds/%d/downloads/%s", base, url.PathEscape(b.Version), b.Build, platform),
		SHA256:  strings.ToLower(d.SHA256),
	}, nil
}

// geyserJavaVersions asks Modrinth which Minecraft Java versions the newest
// Geyser build for Paper supports (GeyserMC publishes every build there with
// that list). A server on a newer version than these can't load Geyser.
func (m *Manager) geyserJavaVersions(ctx context.Context) (map[string]bool, error) {
	return m.geyserVersionsFor(ctx, LoaderPaper)
}

// geyserVersionsFor is geyserJavaVersions for a server type (Paper's are
// also kept for GeyserSupports).
func (m *Manager) geyserVersionsFor(ctx context.Context, loader string) (map[string]bool, error) {
	var builds []struct {
		GameVersions  []string `json:"game_versions"`
		DatePublished string   `json:"date_published"`
	}
	u := m.opts.ModrinthAPI + "/project/geyser/version?loaders=" + url.QueryEscape(`["`+modrinthLoader(loader)+`"]`)
	if err := m.getJSON(ctx, u, &builds); err != nil {
		return nil, err
	}
	newest := -1
	for i, b := range builds {
		if len(b.GameVersions) > 0 && (newest < 0 || b.DatePublished > builds[newest].DatePublished) {
			newest = i
		}
	}
	if newest < 0 {
		return nil, errors.New("Modrinth didn't list the Minecraft versions Geyser supports")
	}
	set := map[string]bool{}
	for _, v := range builds[newest].GameVersions {
		set[v] = true
	}
	if modrinthLoader(loader) != "paper" {
		return set, nil
	}
	m.geyserMC.mu.Lock()
	m.geyserMC.versions, m.geyserMC.at = set, time.Now()
	m.geyserMC.mu.Unlock()
	return set, nil
}

// geyserVersionCache keeps the Minecraft versions Geyser supports.
type geyserVersionCache struct {
	mu       sync.Mutex
	versions map[string]bool // never changed in place
	at       time.Time
	busy     bool
}

// GeyserSupports reports whether the newest Geyser supports a Minecraft
// version, and whether that's known yet. It checks at most once an hour,
// without waiting.
func (m *Manager) GeyserSupports(version string) (supported, known bool) {
	c := &m.geyserMC
	c.mu.Lock()
	if !c.busy && time.Since(c.at) > versionCheckEvery {
		c.busy = true
		go func() {
			ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
			defer cancel()
			_, _ = m.geyserJavaVersions(ctx)
			c.mu.Lock()
			c.busy = false
			if c.at.IsZero() || time.Since(c.at) > versionCheckEvery {
				c.at = time.Now() // failed: try again in an hour
			}
			c.mu.Unlock()
		}()
	}
	v := c.versions
	c.mu.Unlock()
	if v == nil {
		return false, false
	}
	return v[version], true
}

// latestViaVersion asks Hangar (PaperMC's plugin site) for ViaVersion.
func (m *Manager) latestViaVersion(ctx context.Context) (pluginRelease, error) {
	base := m.opts.HangarAPI + "/ViaVersion"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/latestrelease", nil)
	if err != nil {
		return pluginRelease{}, err
	}
	req.Header.Set("User-Agent", m.javaUserAgent())
	resp, err := m.http().Do(req)
	if err != nil {
		return pluginRelease{}, err
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	resp.Body.Close()
	if err != nil {
		return pluginRelease{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return pluginRelease{}, fmt.Errorf("%s answered %s", hostOf(base), resp.Status)
	}
	version := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if version == "" || strings.ContainsAny(version, "/?#% ") {
		return pluginRelease{}, errors.New("hangar.papermc.io gave an odd answer")
	}
	var v struct {
		Downloads map[string]struct {
			FileInfo struct {
				SHA256 string `json:"sha256Hash"`
			} `json:"fileInfo"`
			DownloadURL string `json:"downloadUrl"`
		} `json:"downloads"`
	}
	if err := m.getJSON(ctx, base+"/versions/"+url.PathEscape(version), &v); err != nil {
		return pluginRelease{}, err
	}
	d := v.Downloads["PAPER"]
	if d.DownloadURL == "" || len(d.FileInfo.SHA256) != 64 {
		return pluginRelease{}, errors.New("hangar.papermc.io has no checked Paper download of ViaVersion")
	}
	return pluginRelease{Version: version, URL: d.DownloadURL, SHA256: strings.ToLower(d.FileInfo.SHA256)}, nil
}

// consoleReleases returns the newest build of each console plugin. errs has
// a reason for each one that couldn't be found.
func (m *Manager) consoleReleases(ctx context.Context) (map[string]pluginRelease, map[string]error) {
	return m.consoleReleasesFor(ctx, consolePlugins, "", "")
}

// consoleReleasesFor is consoleReleases for a set of console plugins, on a
// Minecraft version and mod loader (for the ones from Modrinth).
func (m *Manager) consoleReleasesFor(ctx context.Context, set []consolePlugin, minecraft, loader string) (map[string]pluginRelease, map[string]error) {
	out, errs := map[string]pluginRelease{}, map[string]error{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range set {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var r pluginRelease
			var err error
			kind, arg, _ := strings.Cut(p.Source, ":")
			switch kind {
			case "hangar":
				r, err = m.latestViaVersion(ctx)
			case "modrinth":
				var f pluginFile
				if f, err = m.latestModrinthFor(ctx, arg, minecraft, loader); err == nil {
					r = pluginRelease{Version: f.Version, URL: f.URL, SHA256: f.Hash}
				}
			default:
				r, err = m.latestGeyserProject(ctx, p.Key, arg)
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[p.Key] = err
			} else {
				out[p.Key] = r
			}
		}()
	}
	wg.Wait()
	return out, errs
}

// pluginCache keeps the newest console plugin builds for "update ready"
// notes and overnight updates.
type pluginCache struct {
	mu       sync.Mutex
	releases map[string]pluginRelease
	at       time.Time
	busy     bool
}

// LatestConsolePlugins returns the newest console plugin builds, checking at
// most once an hour, without waiting (like LatestPaper).
func (m *Manager) LatestConsolePlugins() map[string]pluginRelease {
	c := &m.plugins
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.busy && time.Since(c.at) > versionCheckEvery {
		c.busy = true
		go func() {
			ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
			defer cancel()
			r, _ := m.consoleReleases(ctx)
			c.mu.Lock()
			defer c.mu.Unlock()
			c.busy, c.at = false, time.Now()
			if len(r) > 0 {
				merged := map[string]pluginRelease{}
				for k, v := range c.releases {
					merged[k] = v
				}
				for k, v := range r {
					merged[k] = v
				}
				c.releases = merged
			}
		}()
	}
	return c.releases // never changed in place, so safe to share
}

// consolePluginsBehindLocked reports whether Geyser or Floodgate has a newer
// build than the one installed (or isn't installed). Caller holds s.mu.
func (s *Server) consolePluginsBehindLocked() bool {
	if !s.consolesOnLocked() || s.meta.Version == "" || !runsPlugins(s.loaderLocked()) {
		return false // Fabric's Geyser is checked on Update and overnight
	}
	latest := s.m.LatestConsolePlugins()
	for _, p := range consolePlugins {
		r, ok := latest[p.Key]
		if !p.Required || !ok {
			continue
		}
		// New builds come out most days; a new version (2.11.3 → 2.11.4)
		// is what new Bedrock versions need, so only that counts.
		sum, version, _ := strings.Cut(s.meta.Plugins[p.Key], " ")
		if sum != r.id() && baseVersion(version) != baseVersion(r.Version) {
			return true
		}
	}
	return false
}

// baseVersion is a plugin version without its build ("2.11.3 (build 9)" →
// "2.11.3").
func baseVersion(v string) string {
	v, _, _ = strings.Cut(v, " (build")
	return strings.TrimSpace(v)
}

// ---- installing ----

// ensureConsolePlugins installs or updates the console plugins (or removes
// Geyser and Floodgate when consoles are turned off). The running server
// picks the change up when it next starts. It returns an error only if
// consoles are on and Geyser or Floodgate couldn't be installed.
func (s *Server) ensureConsolePlugins(ctx context.Context) (updated bool, err error) {
	s.pluginsMu.Lock()
	defer s.pluginsMu.Unlock()
	s.mu.Lock()
	on := s.consolesOnLocked()
	have := map[string]string{}
	for k, v := range s.meta.Plugins {
		have[k] = v
	}
	set := s.consoleSetLocked()
	loader, minecraft := s.loaderLocked(), s.meta.Version
	s.mu.Unlock()
	if len(set) == 0 {
		return false, nil // consoles can't join this type of server
	}
	paper := set[0].Dir == "plugins"
	dir := filepath.Join(s.dir(), set[0].Dir)

	if !on {
		if jars, err := s.pluginJars(); paper && err == nil {
			var needs []string
			for _, j := range jars {
				if j.console || !j.enabled {
					continue
				}
				for _, d := range j.depends {
					if strings.EqualFold(d, "floodgate") || strings.EqualFold(d, "Geyser-Spigot") {
						needs = append(needs, j.name)
						break
					}
				}
			}
			if len(needs) > 0 {
				sort.Strings(needs)
				s.say("These plugins need console support and won't start without it: " + strings.Join(needs, ", ") + ".")
			}
		}
		removed := false
		for _, p := range set {
			if !p.Required || p.Key == "fabric-api" {
				continue // ViaVersion also helps Java players on other versions, and other mods need Fabric API: they stay
			}
			if err := os.Remove(filepath.Join(dir, p.File)); err == nil {
				removed = true
			}
			delete(have, p.Key)
		}
		if removed {
			s.say("Removed console support (Geyser and Floodgate). Their settings stay in the " + set[0].Dir + " and config folders.")
		}
		return false, s.setPlugins(have)
	}

	releases, errs := s.m.consoleReleasesFor(ctx, set, minecraft, modrinthLoader(loader))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	var problems []string
	jars := map[string]jarEntry{}
	if paper {
		jars, _ = s.pluginJars()
	}
	for _, p := range set {
		r, ok := releases[p.Key]
		path := filepath.Join(dir, p.File)
		if other := s.otherModCopy(p); other != "" {
			// A copy put in mods/ by hand is the one that runs (two copies
			// stop Fabric from starting).
			if err := os.Remove(path); err == nil {
				s.say(fmt.Sprintf("Using %s, already in the mods folder, for console support's %s.", other, p.Label))
			}
			delete(have, p.Key)
			continue
		}
		if other := otherCopy(jars, p); other != "" {
			// Two copies of a plugin make Paper refuse one of them: the
			// one put in by hand is turned off.
			old := filepath.Join(dir, other)
			if err := os.Rename(old, old+disabledSuffix); err == nil {
				s.say(fmt.Sprintf("Turned off %s, a second copy of %s, so console support's own copy is the one that runs.", other, p.Label))
			}
		}
		if !ok {
			why := "couldn't be found"
			if e := errs[p.Key]; e != nil {
				why = e.Error()
			}
			if p.Required {
				problems = append(problems, p.Label+": "+why)
			} else {
				s.say(fmt.Sprintf("Couldn't check for a newer %s (%s); keeping the one installed.", p.Label, why))
			}
			continue
		}
		if sum, err := fileSum(path, len(r.SHA256)); err == nil && sum == r.id() {
			have[p.Key] = sum + " " + r.Version // also right after a restore brought an older one back
			continue
		}
		s.say(fmt.Sprintf("Downloading %s %s for console players…", p.Label, r.Version))
		file, err := s.m.downloadVerified(ctx, r.URL, s.m.serversDir(), r.SHA256, s.say, s.m.javaUserAgent())
		if err == nil {
			err = os.Rename(file, path)
			if err != nil {
				os.Remove(file)
			}
		}
		if err != nil {
			if p.Required {
				problems = append(problems, p.Label+": "+err.Error())
			} else {
				s.say(fmt.Sprintf("Couldn't download %s (%v); keeping the one installed.", p.Label, err))
			}
			continue
		}
		have[p.Key] = r.id() + " " + r.Version
		updated = true
		s.say(fmt.Sprintf("%s %s is installed.", p.Label, r.Version))
	}
	if err := s.setPlugins(have); err != nil {
		return updated, err
	}
	if len(problems) > 0 {
		what := "installed"
		if s.consolesInstalled() {
			what = "updated (the one installed is kept)"
		}
		return updated, errors.New("console support couldn't be " + what + ": " + strings.Join(problems, "; ") + ".")
	}
	return updated, nil
}

// otherCopy returns the file of another turned-on copy of a console plugin
// (one put in by hand), or "".
func otherCopy(jars map[string]jarEntry, p consolePlugin) string {
	for file, j := range jars {
		if j.enabled && !strings.EqualFold(file, p.File) && strings.EqualFold(j.name, p.Name) {
			return file
		}
	}
	return ""
}

// fileSum returns a file's SHA-512 (n 128) or SHA-256 in hex, to compare
// with a download's checksum.
func fileSum(path string, n int) (string, error) {
	if n != 128 {
		return fileSHA256(path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha512.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fileSHA256 returns a file's SHA-256 in hex.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// notePluginFiles records which console plugin builds are in plugins/ (after
// a restore, which may bring older ones back).
func (s *Server) notePluginFiles() {
	if !s.isJava() {
		return
	}
	s.pluginsMu.Lock()
	defer s.pluginsMu.Unlock()
	have := map[string]string{}
	for _, p := range s.consoleSet() {
		if sum, err := fileSHA256(filepath.Join(s.dir(), p.Dir, p.File)); err == nil {
			have[p.Key] = sum
		}
	}
	if err := s.setPlugins(have); err != nil {
		s.m.log.Warn("could not save server details", "id", s.meta.ID, "error", err)
	}
}

func (s *Server) setPlugins(have map[string]string) error {
	s.mu.Lock()
	if len(have) == 0 {
		have = nil
	}
	s.meta.Plugins = have
	s.mu.Unlock()
	return s.saveMeta()
}

// SetConsoles turns console players on or off for a Java server. The
// plugins are installed (or removed) straight away; the running server
// picks the change up when it next starts.
func (s *Server) SetConsoles(on bool) error {
	if !s.isJava() {
		return errors.New("only Java servers have this setting")
	}
	if err := s.EditBlocked(); err != nil {
		return err
	}
	if !s.m.beginWork() {
		return errShuttingDown
	}
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		s.m.work.Done()
		return errBusy
	}
	changed := s.meta.NoConsoles == on
	s.meta.NoConsoles = !on
	s.mu.Unlock()
	if err := s.saveMeta(); err != nil {
		s.m.work.Done()
		return err
	}
	go func() {
		defer s.m.work.Done()
		// Not while a backup, restore, update or delete runs.
		s.backupMu.Lock()
		defer s.backupMu.Unlock()
		s.mu.Lock()
		gone := s.deleted
		s.mu.Unlock()
		if gone {
			return
		}
		ctx, cancel := context.WithTimeout(s.m.ctx, 10*time.Minute)
		defer cancel()
		_, err := s.ensureConsolePlugins(ctx)
		if err != nil {
			s.say(err.Error() + " Try again later with Update now.")
			return
		}
		s.mu.Lock()
		up := s.cmd != nil
		s.mu.Unlock()
		switch {
		case !changed:
		case up && on:
			s.say("Console players can join after the server restarts.")
		case up:
			s.say("Console players are turned off from the next restart.")
		case on:
			s.say("Console players can join when the server starts.")
		}
	}()
	return nil
}

// ---- console players on the allowlist ----

// consoleIDs turns what was typed for a console player (a gamertag, or the
// name they have on the server, like ".Gamer_Tag") into their gamertag and
// their name on the server. Floodgate replaces spaces with _ (gamertags
// never contain _) and keeps names to 16 characters.
func consoleIDs(name string) (gamertag, javaName string, err error) {
	name = strings.TrimPrefix(strings.TrimSpace(name), ".")
	gamertag, err = cleanGamertag(strings.ReplaceAll(name, "_", " "))
	if err != nil {
		return "", "", err
	}
	javaName = "." + strings.ReplaceAll(gamertag, " ", "_")
	if r := []rune(javaName); len(r) > 16 {
		javaName = string(r[:16])
	}
	return gamertag, javaName, nil
}

// onServerConsoleName is a console player's name on a Java server.
var onServerConsoleName = regexp.MustCompile(`^\.[\p{L}\p{N}_#]{1,15}$`)

// cleanJavaPlayer checks a name of someone on a Java server: a Java username,
// or a console player's ".Gamer_Tag".
func cleanJavaPlayer(name string) (string, error) {
	name = strings.TrimSpace(name)
	if isConsoleName(name) {
		if !onServerConsoleName.MatchString(name) {
			return "", ErrBadJavaName
		}
		return name, nil
	}
	return cleanJavaName(name)
}

// whoName is how the panel mentions someone on a Java server: console
// players by gamertag ("Kid One (console)").
func whoName(name string) string {
	if !isConsoleName(name) {
		return name
	}
	if gamertag, _, err := consoleIDs(name); err == nil {
		return gamertag + " (console)"
	}
	return name
}

// javaListName is the name someone has in a Java server's lists: console
// players as ".Gamer_Tag", whatever was typed.
func javaListName(name string) string {
	if !isConsoleName(name) {
		return strings.TrimSpace(name)
	}
	if _, javaName, err := consoleIDs(name); err == nil {
		return javaName
	}
	return name
}

// isConsoleName reports whether a name on a Java server is a console
// player's (Floodgate's "." prefix).
func isConsoleName(name string) bool { return strings.HasPrefix(strings.TrimSpace(name), ".") }

// floodgateUUID is the account ID Floodgate gives a console player: their
// Xbox ID in the low 64 bits.
func floodgateUUID(xuid string) (string, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(xuid), 10, 64)
	if err != nil || n == 0 {
		return "", errors.New("that isn't an Xbox ID")
	}
	return fmt.Sprintf("00000000-0000-0000-%04x-%012x", n>>48, n&0xffffffffffff), nil
}

var errNotKnown = errors.New("not known")

// xboxLookup finds a gamertag's Xbox ID: from players the panel has seen
// sign in, or else from GeyserMC's lookup service.
func (m *Manager) xboxLookup(ctx context.Context, gamertag string) (string, error) {
	if x := m.people.xuidFor(gamertag); x != "" {
		return x, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.opts.XboxLookup+url.PathEscape(gamertag), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", m.javaUserAgent())
	resp, err := m.http().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v struct {
		XUID json.Number `json:"xuid"`
	}
	derr := jsonDecodeLimited(resp.Body, &v)
	if resp.StatusCode == http.StatusNotFound || (resp.StatusCode < 300 && derr == nil && v.XUID == "") {
		return "", errNotKnown
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("geysermc.org answered %s", resp.Status)
	}
	if derr != nil {
		return "", derr
	}
	if _, err := floodgateUUID(v.XUID.String()); err != nil {
		return "", errors.New("geysermc.org gave an odd answer")
	}
	return v.XUID.String(), nil
}

var errNoConsoles = errors.New("console players are turned off on this server (turn them on in Settings first)")

// errUnknownGamertag: GeyserMC's lookup only knows players who have played
// on a server with console support (any, anywhere), and the panel only
// those it has seen sign in.
func errUnknownGamertag(gamertag string) error {
	return fmt.Errorf("%s isn't known yet: have them try to join once (or play on one of your Bedrock servers), then let them in under Tried to join. Check the spelling too", gamertag)
}

// consoleChange makes an allowlist or operator change for a console player
// on a Java server, in the files (their account ID comes from their Xbox
// ID): a running server is told to reload its allowlist. Operators are made
// by command, which needs them online while the server runs, so that waits
// for their next join. If their Xbox ID can't be looked up now, the change
// waits for the next start (and Floodgate looks them up then).
func (s *Server) consoleChange(name, action string) (pending bool, err error) {
	gamertag, javaName, err := consoleIDs(name)
	if err != nil {
		return false, err
	}
	adding := action == "allow" || action == "op"
	if adding && !s.ConsolesOn() {
		return false, errNoConsoles
	}
	s.dropJavaPending(javaName, action)

	var uuid string
	if adding {
		ctx, cancel := context.WithTimeout(s.m.ctx, 10*time.Second)
		xuid, lerr := s.m.xboxLookup(ctx, gamertag)
		cancel()
		if errors.Is(lerr, errNotKnown) {
			return false, errUnknownGamertag(gamertag)
		}
		if lerr == nil {
			uuid, _ = floodgateUUID(xuid)
		}
	}

	s.mu.Lock()
	up, running := s.cmd != nil, s.runningLocked()
	_, online := s.players[javaName]
	queue := func() (bool, error) {
		s.meta.JavaPending = append(s.meta.JavaPending, JavaPending{Name: javaName, Action: action})
		s.mu.Unlock()
		return true, s.saveMeta()
	}
	switch action {
	case "allow":
		if uuid == "" || (up && !running) { // can't look them up, or it's starting
			return queue()
		}
	case "op":
		if running && online {
			s.mu.Unlock()
			s.javaCommandIfRunning("op " + javaName)
			time.Sleep(300 * time.Millisecond)
			return false, nil
		}
		if uuid == "" || up {
			return queue() // made by command when they join (or at the next start)
		}
	case "deop":
		if running && online {
			s.mu.Unlock()
			s.javaCommandIfRunning("deop " + javaName) // the server knows them
			return false, nil
		}
		if up {
			// By command, and again in the file before the next start in
			// case the server didn't know them.
			s.meta.JavaPending = append(s.meta.JavaPending, JavaPending{Name: javaName, Action: action})
			s.mu.Unlock()
			if err := s.saveMeta(); err != nil {
				return false, err
			}
			s.javaCommandIfRunning("deop " + javaName)
			return false, nil
		}
	case "unallow":
		if up && !running {
			// It's starting: change the file now, and reload once it's up.
			s.mu.Unlock()
			if err := s.editJavaLists(javaName, "", action); err != nil {
				return false, err
			}
			s.mu.Lock()
			return queue()
		}
	}
	s.mu.Unlock()
	if err := s.editJavaLists(javaName, uuid, action); err != nil {
		return false, err
	}
	if action == "allow" || action == "unallow" {
		s.reloadIfRunning("whitelist reload")
	}
	return false, nil
}

// applyConsolePendingLocked makes a queued console change once the server
// is up, or keeps it: operators wait until the player is online, and
// removals are made in the files before the next start. Caller holds s.mu.
func (s *Server) applyConsolePendingLocked(p JavaPending) (keep bool) {
	gamertag, javaName, err := consoleIDs(p.Name)
	if err != nil {
		return false
	}
	var cmd string
	switch p.Action {
	case "op":
		if _, online := s.players[javaName]; !online {
			return true
		}
		cmd = "op " + javaName
	case "allow":
		if !s.consolesLive {
			return s.consolesOnLocked() // Floodgate isn't running: next start
		}
		cmd = fmt.Sprintf("fwhitelist add %q", gamertag) // Floodgate looks them up
	case "unallow":
		cmd = "whitelist reload" // the file was changed while it started
	default: // deop: now, and in the files before the next start
		cmd = "deop " + javaName
		s.addLineLocked("> " + cmd)
		s.sendLocked(cmd)
		return true
	}
	s.addLineLocked("> " + cmd)
	s.sendLocked(cmd)
	return false
}

// applyConsoleFileChanges makes queued console removals in the files while
// no process is up (just before a start). With consoles off it also takes
// out Geyser and Floodgate a restore may have brought back.
func (s *Server) applyConsoleFileChanges() {
	if !s.isJava() {
		return
	}
	s.mu.Lock()
	var todo []JavaPending
	off := !s.consolesOnLocked()
	if s.cmd == nil && off {
		for _, p := range s.consoleSetLocked() {
			if p.Required && p.Key != "fabric-api" {
				_ = os.Remove(filepath.Join(s.dir(), p.Dir, p.File))
			}
		}
	}
	if s.cmd == nil {
		for _, p := range s.meta.JavaPending {
			if isConsoleName(p.Name) && (p.Action == "deop" || p.Action == "unallow") {
				todo = append(todo, p)
			}
		}
	}
	s.mu.Unlock()
	for _, p := range todo {
		if err := s.editJavaLists(p.Name, "", p.Action); err != nil {
			s.say("Couldn't update the player lists: " + err.Error())
			return
		}
		s.mu.Lock()
		kept := s.meta.JavaPending[:0]
		for _, q := range s.meta.JavaPending {
			if q != p {
				kept = append(kept, q)
			}
		}
		s.meta.JavaPending = kept
		s.mu.Unlock()
	}
	if len(todo) > 0 {
		_ = s.saveMeta()
	}
}

// onConsoleJoinLocked makes a console player who just joined an operator if
// that was waiting for them. Caller holds s.mu.
func (s *Server) onConsoleJoinLocked(name string) {
	kept := s.meta.JavaPending[:0]
	changed := false
	for _, p := range s.meta.JavaPending {
		if p.Action == "op" && strings.EqualFold(p.Name, name) {
			cmd := "op " + name
			s.addLineLocked("> " + cmd)
			s.sendLocked(cmd)
			changed = true
			continue
		}
		kept = append(kept, p)
	}
	s.meta.JavaPending = kept
	if changed {
		s.saveMetaLater()
	}
}
