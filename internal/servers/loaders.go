package servers

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Java server types. Besides Paper (java.go), a Java server can run:
//
//   - Purpur: a Paper fork with more settings; runs Paper plugins.
//   - Vanilla: Mojang's own server.
//   - Fabric, Forge and NeoForge: mod loaders. Their mods go in mods/.
//
// Any of them can be on an older Minecraft version, chosen when the server
// is added. A server on a chosen version, and every modded one, keeps its
// version: Update gets the newest build of the same Minecraft version
// (mods only work on the version they're made for).

// Loader values for Meta.Loader ("" is Paper, for servers from before).
const (
	LoaderPaper    = "paper"
	LoaderPurpur   = "purpur"
	LoaderVanilla  = "vanilla"
	LoaderFabric   = "fabric"
	LoaderForge    = "forge"
	LoaderNeoForge = "neoforge"
)

// JavaLoaders are the server types, in the order the page offers them.
var JavaLoaders = []string{LoaderPaper, LoaderPurpur, LoaderVanilla, LoaderFabric, LoaderForge, LoaderNeoForge, LoaderVelocity}

var loaderNames = map[string]string{
	LoaderPaper: "Paper", LoaderPurpur: "Purpur", LoaderVanilla: "Vanilla",
	LoaderFabric: "Fabric", LoaderForge: "Forge", LoaderNeoForge: "NeoForge",
	LoaderVelocity: "Velocity",
}

// LoaderName is a server type's name ("Fabric").
func LoaderName(l string) string {
	if n, ok := loaderNames[l]; ok {
		return n
	}
	return "Paper"
}

// Default addresses of the other Java server types.
const (
	DefaultPurpurAPI     = "https://api.purpurmc.org/v2/purpur"
	DefaultFabricMeta    = "https://meta.fabricmc.net/v2"
	DefaultForgePromos   = "https://files.minecraftforge.net/net/minecraftforge/forge/promotions_slim.json"
	DefaultForgeMaven    = "https://maven.minecraftforge.net/net/minecraftforge/forge"
	DefaultNeoForgeMaven = "https://maven.neoforged.net/releases/net/neoforged/neoforge"
)

// Launch files. A modern Forge or NeoForge server starts from an argument
// file its installer writes (Meta.Launch is "@" and its path).
const (
	vanillaJar      = "server.jar"
	fabricLaunchJar = "fabric-server-launch.jar"
	forgeLegacyJar  = "forge-server.jar"
)

// loaderLocked is the server's type. Caller holds s.mu.
func (s *Server) loaderLocked() string {
	if s.meta.Loader == "" {
		return LoaderPaper
	}
	return s.meta.Loader
}

// Loader is the server's type ("paper" for Java servers from before, ""
// for Bedrock and Hytale).
func (s *Server) Loader() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isJava() {
		return ""
	}
	return s.loaderLocked()
}

// runsPlugins: Paper and Purpur run Paper plugins.
func runsPlugins(loader string) bool { return loader == LoaderPaper || loader == LoaderPurpur }

// runsMods: the mod loaders.
func runsMods(loader string) bool {
	return loader == LoaderFabric || loader == LoaderForge || loader == LoaderNeoForge
}

// javaLaunchPath is the file a Java server starts from (missing: it needs
// installing again).
func (s *Server) javaLaunchPath() string {
	switch {
	case strings.HasPrefix(s.meta.Launch, "@"):
		return filepath.Join(s.dir(), filepath.FromSlash(s.meta.Launch[1:]))
	case s.meta.Launch != "":
		return filepath.Join(s.dir(), filepath.FromSlash(s.meta.Launch))
	}
	return filepath.Join(s.dir(), paperJar)
}

// isJavaDir reports whether dir holds a Java server (any type), for
// backups made from its folder.
func isJavaDir(dir string) bool {
	for _, f := range []string{paperJar, vanillaJar, fabricLaunchJar, forgeLegacyJar, "libraries"} {
		if fileExists(filepath.Join(dir, f)) {
			return true
		}
	}
	return false
}

// ---- versions ----

type versionListCache struct {
	mu   sync.Mutex
	list map[string][]string
	at   map[string]time.Time
}

// JavaVersions lists the Minecraft versions a server type can run, newest
// first (releases only), asking at most once an hour.
func (m *Manager) JavaVersions(ctx context.Context, loader string) ([]string, error) {
	if _, ok := loaderNames[loader]; !ok {
		return nil, errors.New("unknown server type")
	}
	c := &m.javaVersions
	c.mu.Lock()
	if l, ok := c.list[loader]; ok && time.Since(c.at[loader]) < versionCheckEvery {
		c.mu.Unlock()
		return l, nil
	}
	c.mu.Unlock()
	var list []string
	var err error
	switch loader {
	case LoaderPaper:
		list, err = m.paperVersions(ctx)
	case LoaderPurpur:
		list, err = m.purpurVersions(ctx)
	case LoaderVanilla:
		list, err = m.vanillaVersions(ctx)
	case LoaderFabric:
		list, err = m.fabricGameVersions(ctx)
	case LoaderForge:
		var p map[string]string
		if p, err = m.forgePromos(ctx); err == nil {
			list = forgeMCVersions(p)
		}
	case LoaderNeoForge:
		var vs []string
		if vs, err = m.neoForgeVersions(ctx); err == nil {
			list = neoForgeMCVersions(vs)
		}
	case LoaderVelocity:
		if list, err = m.velocityVersions(ctx); err == nil && len(list) > 1 {
			list = list[:1] // a lobby is always on the newest Velocity
		}
	}
	if err != nil {
		return nil, err
	}
	if loader == LoaderVelocity {
		return list, nil // its own versions, already in order
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("%s didn't list any Minecraft versions", LoaderName(loader))
	}
	sort.SliceStable(list, func(i, j int) bool { return versionNewer(list[i], list[j]) })
	c.mu.Lock()
	if c.list == nil {
		c.list, c.at = map[string][]string{}, map[string]time.Time{}
	}
	c.list[loader], c.at[loader] = list, time.Now()
	c.mu.Unlock()
	return list, nil
}

func (m *Manager) paperVersions(ctx context.Context) ([]string, error) {
	var project struct {
		Versions map[string][]string `json:"versions"`
	}
	if err := m.getJSON(ctx, m.opts.PaperAPI, &project); err != nil {
		return nil, fmt.Errorf("couldn't ask PaperMC for its versions: %w", err)
	}
	var out []string
	for _, group := range project.Versions {
		for _, v := range group {
			if plainVersion.MatchString(v) {
				out = append(out, v)
			}
		}
	}
	return out, nil
}

func (m *Manager) purpurVersions(ctx context.Context) ([]string, error) {
	var p struct {
		Versions []string `json:"versions"`
	}
	if err := m.getJSON(ctx, m.opts.PurpurAPI, &p); err != nil {
		return nil, fmt.Errorf("couldn't ask Purpur for its versions: %w", err)
	}
	var out []string
	for _, v := range p.Versions {
		if plainVersion.MatchString(v) {
			out = append(out, v)
		}
	}
	return out, nil
}

type mojangManifest struct {
	Latest struct {
		Release string `json:"release"`
	} `json:"latest"`
	Versions []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"versions"`
}

func (m *Manager) mojangManifest(ctx context.Context) (mojangManifest, error) {
	var mf mojangManifest
	if err := m.getJSON(ctx, m.opts.MojangVersions, &mf); err != nil {
		return mf, fmt.Errorf("couldn't ask Mojang for its versions: %w", err)
	}
	return mf, nil
}

// vanillaVersions: releases from 1.7.10 on (older ones have no server
// download, or one too old to be worth running).
func (m *Manager) vanillaVersions(ctx context.Context) ([]string, error) {
	mf, err := m.mojangManifest(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, v := range mf.Versions {
		if v.Type == "release" && plainVersion.MatchString(v.ID) && !versionNewer("1.7.10", v.ID) {
			out = append(out, v.ID)
		}
	}
	return out, nil
}

// vanillaServer finds the official server download of a Minecraft version,
// and the Java it needs.
func (m *Manager) vanillaServer(ctx context.Context, version string) (dl pluginRelease, javaMajor int, err error) {
	mf, err := m.mojangManifest(ctx)
	if err != nil {
		return dl, 0, err
	}
	if version == "" {
		version = mf.Latest.Release
	}
	for _, v := range mf.Versions {
		if v.ID != version {
			continue
		}
		var info struct {
			Downloads struct {
				Server struct {
					SHA1 string `json:"sha1"`
					URL  string `json:"url"`
				} `json:"server"`
			} `json:"downloads"`
			JavaVersion struct {
				MajorVersion int `json:"majorVersion"`
			} `json:"javaVersion"`
		}
		if err := m.getJSON(ctx, v.URL, &info); err != nil {
			return dl, 0, fmt.Errorf("couldn't ask Mojang about %s: %w", version, err)
		}
		if info.Downloads.Server.URL == "" {
			return dl, 0, fmt.Errorf("Mojang has no server download of Minecraft %s", version)
		}
		major := info.JavaVersion.MajorVersion
		if major < 8 {
			major = guessJavaMajor(version)
		}
		return pluginRelease{Version: version, URL: info.Downloads.Server.URL, SHA256: strings.ToLower(info.Downloads.Server.SHA1)}, major, nil
	}
	return dl, 0, fmt.Errorf("Mojang doesn't know Minecraft %s", version)
}

type fabricVersion struct {
	Version string `json:"version"`
	Stable  bool   `json:"stable"`
}

func (m *Manager) fabricList(ctx context.Context, what string) ([]fabricVersion, error) {
	var list []fabricVersion
	if err := m.getJSON(ctx, m.opts.FabricMeta+"/versions/"+what, &list); err != nil {
		return nil, fmt.Errorf("couldn't ask Fabric for its %s versions: %w", what, err)
	}
	return list, nil
}

func (m *Manager) fabricGameVersions(ctx context.Context) ([]string, error) {
	list, err := m.fabricList(ctx, "game")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, v := range list {
		if v.Stable && plainVersion.MatchString(v.Version) && !versionNewer("1.14", v.Version) {
			out = append(out, v.Version)
		}
	}
	return out, nil
}

// newestStable is the first stable entry (Fabric lists newest first).
func newestStable(list []fabricVersion) string {
	for _, v := range list {
		if v.Stable {
			return v.Version
		}
	}
	return ""
}

func (m *Manager) forgePromos(ctx context.Context) (map[string]string, error) {
	var p struct {
		Promos map[string]string `json:"promos"`
	}
	if err := m.getJSON(ctx, m.opts.ForgePromos, &p); err != nil {
		return nil, fmt.Errorf("couldn't ask Forge for its versions: %w", err)
	}
	return p.Promos, nil
}

var reForgePromo = regexp.MustCompile(`^(\d+\.\d+(?:\.\d+)?)-(latest|recommended)$`)

// forgeMCVersions are the Minecraft versions Forge has a build for, from
// 1.12.2 on.
func forgeMCVersions(promos map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for k := range promos {
		mm := reForgePromo.FindStringSubmatch(k)
		if mm == nil || seen[mm[1]] || versionNewer("1.12.2", mm[1]) {
			continue
		}
		seen[mm[1]] = true
		out = append(out, mm[1])
	}
	return out
}

// forgeFor picks the Forge build for a Minecraft version: the recommended
// one, or the latest.
func forgeFor(promos map[string]string, mc string) string {
	if v := promos[mc+"-recommended"]; v != "" {
		return v
	}
	return promos[mc+"-latest"]
}

func (m *Manager) neoForgeVersions(ctx context.Context) ([]string, error) {
	var meta struct {
		Versioning struct {
			Versions []string `xml:"versions>version"`
		} `xml:"versioning"`
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.opts.NeoForgeMaven+"/maven-metadata.xml", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", m.javaUserAgent())
	resp, err := m.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("couldn't ask NeoForge for its versions: %w", plainErr(m.opts.NeoForgeMaven, err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("couldn't ask NeoForge for its versions: %s answered %s", hostOf(m.opts.NeoForgeMaven), resp.Status)
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&meta); err != nil {
		return nil, fmt.Errorf("couldn't read NeoForge's version list: %w", err)
	}
	return meta.Versioning.Versions, nil
}

var reNeoForge = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:\.(\d+))?(-beta)?$`)

// neoForgeMC is the Minecraft version a NeoForge version is for: 21.1.77
// is for 1.21.1, 21.0.5 for 1.21; from Minecraft 26.1 it's 26.1.0.12 (and
// 26.1.1.3 for 26.1.1).
func neoForgeMC(v string) string {
	mm := reNeoForge.FindStringSubmatch(v)
	if mm == nil {
		return ""
	}
	major, _ := strconv.Atoi(mm[1])
	if major >= 26 {
		if mm[4] == "" {
			return ""
		}
		mc := mm[1] + "." + mm[2]
		if mm[3] != "0" {
			mc += "." + mm[3]
		}
		return mc
	}
	if major < 20 || mm[4] != "" {
		return ""
	}
	mc := "1." + mm[1]
	if mm[2] != "0" {
		mc += "." + mm[2]
	}
	return mc
}

func neoForgeMCVersions(versions []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range versions {
		if mc := neoForgeMC(v); mc != "" && !seen[mc] {
			seen[mc] = true
			out = append(out, mc)
		}
	}
	return out
}

// neoForgeFor picks the newest NeoForge for a Minecraft version, a
// finished one over a beta.
func neoForgeFor(versions []string, mc string) string {
	best, bestBeta := "", ""
	for _, v := range versions {
		if neoForgeMC(v) != mc {
			continue
		}
		if strings.HasSuffix(v, "-beta") {
			if bestBeta == "" || versionNewer(strings.TrimSuffix(v, "-beta"), strings.TrimSuffix(bestBeta, "-beta")) {
				bestBeta = v
			}
		} else if best == "" || versionNewer(v, best) {
			best = v
		}
	}
	if best != "" {
		return best
	}
	return bestBeta
}

// guessJavaMajor is the Java a Minecraft version needs when Mojang can't
// say.
func guessJavaMajor(mc string) int {
	switch {
	case !versionNewer("26", mc):
		return 25
	case !versionNewer("1.20.5", mc):
		return 21
	case !versionNewer("1.17", mc):
		return 17
	}
	return 8
}

// ---- installing ----

// installLoader installs or updates a Purpur, Vanilla, Fabric, Forge or
// NeoForge server (Paper is installJava's).
func (s *Server) installLoader(ctx context.Context, update bool) error {
	s.mu.Lock()
	loader, current, loaderVersion, keep := s.loaderLocked(), s.meta.Version, s.meta.LoaderVersion, s.meta.KeepVersion
	launchBefore := s.meta.Launch
	s.mu.Unlock()
	_, statErr := os.Stat(s.javaLaunchPath())
	installed := statErr == nil && launchBefore != ""
	if update && !installed {
		update = false // the files are missing: install afresh, keeping what's there
	}
	// Modded servers and those on a chosen version keep their version.
	target := current
	if !keep && update {
		target = ""
	}
	pinnedLoader := ""
	if !update && loaderVersion != "" {
		pinnedLoader = loaderVersion // a modpack asks for its exact loader
	}
	// A new Purpur, Fabric or NeoForge server with consoles goes on the
	// newest Minecraft Geyser supports.
	if target == "" && s.ConsolesOn() && (loader == LoaderPurpur || loader == LoaderFabric || loader == LoaderNeoForge) {
		if v, err := s.m.newestForGeyser(ctx, loader); err == nil && v != "" {
			target = v
		} else if err != nil {
			s.say("Couldn't check which Minecraft versions Geyser supports (" + err.Error() + "); using the newest.")
		}
	}

	var (
		mc, build, launch string
		major             int
		install           func() error
	)
	switch loader {
	case LoaderPurpur:
		r, err := s.m.purpurRelease(ctx, target)
		if err != nil {
			return err
		}
		mc, build = r.Version, r.build
		if installed && mc == current && build == loaderVersion {
			break
		}
		install = func() error {
			s.say("Installing Purpur " + mc + " (build " + build + "). By downloading it you accept the Minecraft EULA: https://www.minecraft.net/eula")
			return s.fetchInto(ctx, r.pluginRelease, paperJar)
		}
		launch = paperJar
	case LoaderVanilla:
		r, j, err := s.m.vanillaServer(ctx, target)
		if err != nil {
			return err
		}
		mc, major, build = r.Version, j, r.SHA256
		if installed && mc == current && build == loaderVersion {
			break
		}
		install = func() error {
			s.say("Installing Minecraft " + mc + " (Mojang's server). By downloading it you accept the Minecraft EULA: https://www.minecraft.net/eula")
			return s.fetchInto(ctx, r, vanillaJar)
		}
		launch = vanillaJar
	case LoaderFabric:
		if target == "" {
			games, err := s.m.fabricGameVersions(ctx)
			if err != nil {
				return err
			}
			sort.SliceStable(games, func(i, j int) bool { return versionNewer(games[i], games[j]) })
			if len(games) == 0 {
				return errors.New("Fabric didn't list any Minecraft versions")
			}
			target = games[0]
		}
		loaders, err := s.m.fabricList(ctx, "loader")
		if err != nil {
			return err
		}
		installers, err := s.m.fabricList(ctx, "installer")
		if err != nil {
			return err
		}
		lv, iv := newestStable(loaders), newestStable(installers)
		if pinnedLoader != "" {
			lv = pinnedLoader
		}
		if lv == "" || iv == "" {
			return errors.New("Fabric didn't list a stable loader")
		}
		mc, build = target, lv
		if installed && mc == current && build == loaderVersion {
			break
		}
		vr, _, err := s.m.vanillaServer(ctx, mc)
		if err != nil {
			return err
		}
		install = func() error {
			s.say("Installing Fabric loader " + lv + " for Minecraft " + mc + ". By downloading it you accept the Minecraft EULA: https://www.minecraft.net/eula")
			if err := s.fetchInto(ctx, vr, vanillaJar); err != nil {
				return err
			}
			u := fmt.Sprintf("%s/versions/loader/%s/%s/%s/server/jar", s.m.opts.FabricMeta, url.PathEscape(mc), url.PathEscape(lv), url.PathEscape(iv))
			if err := s.fetchInto(ctx, pluginRelease{URL: u}, fabricLaunchJar); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(s.dir(), "fabric-server-launcher.properties"), []byte("serverJar="+vanillaJar+"\n"), 0o644)
		}
		launch = fabricLaunchJar
	case LoaderForge:
		promos, err := s.m.forgePromos(ctx)
		if err != nil {
			return err
		}
		if target == "" {
			list := forgeMCVersions(promos)
			sort.SliceStable(list, func(i, j int) bool { return versionNewer(list[i], list[j]) })
			if len(list) == 0 {
				return errors.New("Forge didn't list any Minecraft versions")
			}
			target = list[0]
		}
		fv := forgeFor(promos, target)
		if pinnedLoader != "" {
			fv = pinnedLoader
		}
		if fv == "" {
			return fmt.Errorf("Forge has no build for Minecraft %s", target)
		}
		mc, build = target, fv
		if installed && mc == current && build == loaderVersion {
			break
		}
		full := mc + "-" + fv
		if mc == "1.7.10" || mc == "1.8.9" {
			full += "-" + mc
		}
		installer := fmt.Sprintf("%s/%s/forge-%s-installer.jar", s.m.opts.ForgeMaven, full, full)
		install = func() error {
			s.say("Installing Forge " + fv + " for Minecraft " + mc + ". By downloading it you accept the Minecraft EULA: https://www.minecraft.net/eula")
			l, err := s.runInstaller(ctx, installer, mc, "libraries/net/minecraftforge/forge", full, "forge-"+full)
			launch = l
			return err
		}
	case LoaderVelocity:
		r, err := s.m.velocityRelease(ctx)
		if err != nil {
			return err
		}
		mc, build, major = r.Version, strconv.Itoa(r.Build), 21
		if installed && mc == current && build == loaderVersion {
			break
		}
		install = func() error {
			s.say("Installing Velocity " + mc + " (build " + build + "), PaperMC's proxy for lobbies.")
			return s.fetchInto(ctx, pluginRelease{Version: mc, URL: r.URL, SHA256: r.SHA256}, velocityJar)
		}
		launch = velocityJar
	case LoaderNeoForge:
		versions, err := s.m.neoForgeVersions(ctx)
		if err != nil {
			return err
		}
		if target == "" {
			list := neoForgeMCVersions(versions)
			sort.SliceStable(list, func(i, j int) bool { return versionNewer(list[i], list[j]) })
			if len(list) == 0 {
				return errors.New("NeoForge didn't list any Minecraft versions")
			}
			// The newest Minecraft with a finished NeoForge (not only betas).
			target = list[0]
			for _, mc := range list {
				if v := neoForgeFor(versions, mc); !strings.HasSuffix(v, "-beta") {
					target = mc
					break
				}
			}
		}
		nv := neoForgeFor(versions, target)
		if pinnedLoader != "" {
			nv = pinnedLoader
		}
		if nv == "" {
			return fmt.Errorf("NeoForge has no build for Minecraft %s", target)
		}
		mc, build = target, nv
		if installed && mc == current && build == loaderVersion {
			break
		}
		installer := fmt.Sprintf("%s/%s/neoforge-%s-installer.jar", s.m.opts.NeoForgeMaven, nv, nv)
		install = func() error {
			s.say("Installing NeoForge " + nv + " for Minecraft " + mc + ". By downloading it you accept the Minecraft EULA: https://www.minecraft.net/eula")
			l, err := s.runInstaller(ctx, installer, mc, "libraries/net/neoforged/neoforge", nv, "neoforge-"+nv)
			launch = l
			return err
		}
	default:
		return fmt.Errorf("unknown server type %q", loader)
	}

	if !update && keep && loader != LoaderVelocity {
		s.consolesForVersion(ctx, mc)
	}
	if major == 0 {
		major = s.m.javaMajorFor(ctx, mc)
	}
	if _, err := s.m.ensureJava(ctx, major, s.say); err != nil {
		return err
	}
	// The Java is needed before the installer runs.
	s.mu.Lock()
	s.meta.JavaMajor = major
	s.mu.Unlock()
	if install == nil {
		_ = s.saveMeta()
		s.say(fmt.Sprintf("Already on the current version (%s %s, Minecraft %s).", LoaderName(loader), shortBuild(build), mc))
		return s.installConsoles(ctx, false)
	}
	if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		return err
	}
	if err := install(); err != nil {
		if update && (loader == LoaderForge || loader == LoaderNeoForge) {
			// The old version's libraries are gone by now.
			return &partialUpdateError{err: err}
		}
		return err
	}
	if err := os.WriteFile(filepath.Join(s.dir(), "eula.txt"), []byte("# Accepted in the Block Party panel when the server was added.\neula=true\n"), 0o644); err != nil {
		return err
	}
	s.mu.Lock()
	s.meta.Version, s.meta.LoaderVersion, s.meta.Launch = mc, build, launch
	if b, err := strconv.Atoi(build); err == nil && loader == LoaderPurpur {
		s.meta.Build = b
	}
	s.mu.Unlock()
	if err := s.saveMeta(); err != nil {
		return err
	}
	s.mu.Lock()
	err := s.applyJavaProperties(!update)
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("couldn't set up server.properties: %w", err)
	}
	_ = os.Remove(s.incompleteMarker())
	if loader == LoaderVelocity {
		s.say(fmt.Sprintf("Velocity %s (build %s) is installed.", mc, build))
	} else {
		s.say(fmt.Sprintf("%s %s for Minecraft %s is installed.", LoaderName(loader), shortBuild(build), mc))
	}
	return s.installConsoles(ctx, false)
}

// newestForGeyser is the newest Minecraft version both a server type and
// Geyser support.
func (m *Manager) newestForGeyser(ctx context.Context, loader string) (string, error) {
	allowed, err := m.geyserVersionsFor(ctx, loader)
	if err != nil {
		return "", err
	}
	list, err := m.JavaVersions(ctx, loader)
	if err != nil {
		return "", err
	}
	if loader == LoaderNeoForge {
		// As for a server without consoles: a finished NeoForge over a beta.
		if versions, err := m.neoForgeVersions(ctx); err == nil {
			for _, v := range list {
				if allowed[v] && !strings.HasSuffix(neoForgeFor(versions, v), "-beta") {
					return v, nil
				}
			}
		}
	}
	for _, v := range list {
		if allowed[v] {
			return v, nil
		}
	}
	return "", nil
}

// consolesForVersion turns console support off for a new server on a
// Minecraft version Geyser doesn't support (an older one chosen by hand):
// it would never start.
func (s *Server) consolesForVersion(ctx context.Context, mc string) {
	s.mu.Lock()
	on, loader := s.consolesOnLocked(), s.loaderLocked()
	s.mu.Unlock()
	if !on {
		return
	}
	allowed, err := s.m.geyserVersionsFor(ctx, loader)
	if err != nil || allowed[mc] {
		return
	}
	s.mu.Lock()
	s.meta.NoConsoles = true
	s.mu.Unlock()
	_ = s.saveMeta()
	s.say("Consoles can't join Minecraft " + mc + ": Geyser only supports the newest versions. Console support is off for this server; Java players can join.")
}

// shortBuild shortens a vanilla server's checksum (its "build").
func shortBuild(b string) string {
	if len(b) == 40 {
		return "(" + b[:7] + ")"
	}
	return b
}

// fetchInto downloads r into the server's folder as name, checking it.
func (s *Server) fetchInto(ctx context.Context, r pluginRelease, name string) error {
	file, err := s.m.downloadVerified(ctx, r.URL, s.m.serversDir(), r.SHA256, s.say, s.m.javaUserAgent())
	if err != nil {
		return err
	}
	if err := os.Rename(file, filepath.Join(s.dir(), name)); err != nil {
		os.Remove(file)
		return err
	}
	return nil
}

// purpurBuild is a Purpur build.
type purpurBuild struct {
	pluginRelease
	build string
}

// purpurRelease finds the newest Purpur build of a Minecraft version (""
// for the newest version).
func (m *Manager) purpurRelease(ctx context.Context, mc string) (purpurBuild, error) {
	if mc == "" {
		list, err := m.purpurVersions(ctx)
		if err != nil {
			return purpurBuild{}, err
		}
		sort.SliceStable(list, func(i, j int) bool { return versionNewer(list[i], list[j]) })
		if len(list) == 0 {
			return purpurBuild{}, errors.New("Purpur didn't list any versions")
		}
		mc = list[0]
	}
	var v struct {
		Builds struct {
			Latest string `json:"latest"`
		} `json:"builds"`
	}
	base := m.opts.PurpurAPI + "/" + url.PathEscape(mc)
	if err := m.getJSON(ctx, base, &v); err != nil {
		return purpurBuild{}, fmt.Errorf("couldn't ask Purpur about Minecraft %s: %w", mc, err)
	}
	if v.Builds.Latest == "" {
		return purpurBuild{}, fmt.Errorf("Purpur has no build of Minecraft %s", mc)
	}
	var b struct {
		MD5 string `json:"md5"`
	}
	if err := m.getJSON(ctx, base+"/"+url.PathEscape(v.Builds.Latest), &b); err != nil {
		return purpurBuild{}, fmt.Errorf("couldn't ask Purpur about build %s: %w", v.Builds.Latest, err)
	}
	return purpurBuild{
		pluginRelease: pluginRelease{Version: mc, URL: base + "/" + url.PathEscape(v.Builds.Latest) + "/download", SHA256: strings.ToLower(b.MD5)},
		build:         v.Builds.Latest,
	}, nil
}

// runInstaller downloads a Forge or NeoForge installer and runs it in the
// server's folder. It returns the launch: "@" and the argument file
// (Minecraft 1.17 and later), or the server jar it made (older ones).
func (s *Server) runInstaller(ctx context.Context, installerURL, mc, libDir, full, jarPrefix string) (string, error) {
	sum := s.m.mavenSHA1(ctx, installerURL)
	file, err := s.m.downloadVerified(ctx, installerURL, s.m.serversDir(), sum, s.say, s.m.javaUserAgent())
	if err != nil {
		return "", err
	}
	installer := filepath.Join(s.dir(), ".bp-installer.jar")
	if err := os.Rename(file, installer); err != nil {
		os.Remove(file)
		return "", err
	}
	defer os.Remove(installer)
	defer os.Remove(installer + ".log")
	// An update: the old version's libraries go, so only one argument file
	// is left to find.
	_ = os.RemoveAll(filepath.Join(s.dir(), filepath.FromSlash(libDir)))
	s.mu.Lock()
	major := s.meta.JavaMajor
	s.mu.Unlock()
	java := s.m.javaBinary(major)
	if java == "" {
		return "", fmt.Errorf("Java %d isn't installed", major)
	}
	s.say("Running the installer (it downloads Minecraft and the libraries it needs; this takes a few minutes)…")
	ictx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ictx, java, "-jar", filepath.Base(installer), "--installServer")
	cmd.Dir = s.dir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) > 5 {
			lines = lines[len(lines)-5:]
		}
		return "", fmt.Errorf("the installer failed (%v): %s", err, strings.Join(lines, " / "))
	}
	for _, junk := range []string{"run.sh", "run.bat", "user_jvm_args.txt", "installer.log"} {
		if junk == "user_jvm_args.txt" {
			continue // the server's own memory settings stay with it; the panel sets memory itself
		}
		_ = os.Remove(filepath.Join(s.dir(), junk))
	}
	args := filepath.ToSlash(filepath.Join(libDir, full, "unix_args.txt"))
	if fileExists(filepath.Join(s.dir(), filepath.FromSlash(args))) {
		return "@" + args, nil
	}
	// Before 1.17: a server jar in the folder.
	matches, _ := filepath.Glob(filepath.Join(s.dir(), jarPrefix+"*.jar"))
	sort.Strings(matches)
	for _, m := range matches {
		base := filepath.Base(m)
		if strings.Contains(base, "installer") {
			continue
		}
		if err := os.Rename(m, filepath.Join(s.dir(), forgeLegacyJar)); err != nil {
			return "", err
		}
		return forgeLegacyJar, nil
	}
	return "", errors.New("the installer finished but left no server to start")
}

// mavenSHA1 fetches a Maven file's .sha1 ("" if there isn't one).
func (m *Manager) mavenSHA1(ctx context.Context, fileURL string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL+".sha1", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", m.javaUserAgent())
	resp, err := m.http().Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
	sum := strings.ToLower(strings.Fields(string(b) + " x")[0])
	if len(sum) != 40 {
		return ""
	}
	return sum
}

// loaderArgs are the arguments that start a server of a type, after the
// memory settings.
func (s *Server) loaderArgs() []string {
	launch := s.meta.Launch
	switch s.loaderLocked() {
	case LoaderPaper, LoaderPurpur:
		return []string{"-jar", paperJar, "--nogui", "--world-container", "worlds"}
	case LoaderVelocity:
		return []string{"-jar", velocityJar}
	}
	var args []string
	if strings.HasPrefix(launch, "@") {
		args = []string{launch}
	} else {
		args = []string{"-jar", launch}
	}
	return append(args, "--nogui", "--universe", "worlds")
}
