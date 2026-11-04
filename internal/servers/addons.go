package servers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Add-ons are behavior packs (what things do) and resource packs (how they
// look). They're installed into the world's own folder
// (worlds/<world>/behavior_packs and resource_packs) and turned on in its
// world_behavior_packs.json and world_resource_packs.json, the same as a
// world exported from the game. So they go along with the world's backups,
// survive updates, and stay with the world they were added to.

// errJavaAddons: the Add-ons tab is for Bedrock servers.
var errJavaAddons = errors.New("add-ons and worlds are for Bedrock servers")

// Pack is one add-on pack in a world.
type Pack struct {
	UUID        string `json:"uuid"`
	Version     string `json:"version"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Kind        string `json:"kind"`   // "behavior" or "resource"
	Folder      string `json:"folder"` // inside <kind>_packs, "" when it's missing
	Enabled     bool   `json:"enabled"`
	// NeedsBeta is true for a pack that uses scripting features that only
	// work with the world's Beta APIs experiment on.
	NeedsBeta bool  `json:"needsBeta,omitempty"`
	Size      int64 `json:"size"`
	Missing   bool  `json:"missing,omitempty"` // turned on, but its files aren't in the world
}

// PacksView is the add-ons of the server's current world.
type PacksView struct {
	World   string `json:"world"`
	Packs   []Pack `json:"packs"`
	Running bool   `json:"running"`
	// Required is texturepack-required: players must take the resource packs.
	Required bool `json:"required"`
}

var packKinds = []string{"behavior", "resource"}

// manifest is the part of a pack's manifest.json the panel reads.
type manifest struct {
	Header struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		UUID        string          `json:"uuid"`
		Version     json.RawMessage `json:"version"`
		MinEngine   json.RawMessage `json:"min_engine_version"`
	} `json:"header"`
	Modules []struct {
		Type string `json:"type"`
	} `json:"modules"`
	Dependencies []struct {
		UUID       string          `json:"uuid"`
		ModuleName string          `json:"module_name"`
		Version    json.RawMessage `json:"version"`
	} `json:"dependencies"`
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// readManifest reads a manifest.json. Packs made by hand often have
// comments or trailing commas, which the game accepts, so those are allowed.
func readManifest(path string) (*manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		if err2 := json.Unmarshal(looseJSON(b), &m); err2 != nil {
			return nil, fmt.Errorf("its manifest.json can't be read: %v", err)
		}
	}
	if !uuidPattern.MatchString(m.Header.UUID) {
		return nil, errors.New("its manifest.json has no valid pack ID")
	}
	m.Header.UUID = strings.ToLower(m.Header.UUID)
	return &m, nil
}

// looseJSON removes // and /* */ comments and trailing commas, outside
// strings.
func looseJSON(b []byte) []byte {
	var out []byte
	inStr, esc := false, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inStr {
			out = append(out, c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch {
		case c == '"':
			inStr = true
			out = append(out, c)
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			for i < len(b) && b[i] != '\n' {
				i++
			}
			out = append(out, '\n')
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			i += 2
			for i+1 < len(b) && !(b[i] == '*' && b[i+1] == '/') {
				i++
			}
			i++
		case c == ']' || c == '}':
			// Drop a comma just before (ignoring spaces).
			j := len(out) - 1
			for j >= 0 && (out[j] == ' ' || out[j] == '\n' || out[j] == '\r' || out[j] == '\t') {
				j--
			}
			if j >= 0 && out[j] == ',' {
				out = append(out[:j], out[j+1:]...)
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
}

// kind says whether a manifest is for a behavior or resource pack.
func (m *manifest) kind() (string, error) {
	for _, mod := range m.Modules {
		switch strings.ToLower(mod.Type) {
		case "data", "script", "client_data", "javascript":
			return "behavior", nil
		case "resources":
			return "resource", nil
		case "skin_pack":
			return "", errors.New("it's a skin pack; skins are chosen in the game, not on the server")
		case "world_template":
			return "", errors.New("it's a world template; upload it as a world instead")
		}
	}
	return "", errors.New("its manifest.json doesn't say what kind of pack it is")
}

// version returns the pack's version as three numbers, from [1,0,0] or
// "1.0.0" (newer manifests).
func parseVersion(raw json.RawMessage) ([3]int, bool) {
	var v [3]int
	var arr []int
	if json.Unmarshal(raw, &arr) == nil && len(arr) >= 1 {
		for i := 0; i < 3 && i < len(arr); i++ {
			v[i] = arr[i]
		}
		return v, true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		core, _, _ := strings.Cut(s, "-")
		parts := strings.Split(core, ".")
		for i := 0; i < 3 && i < len(parts); i++ {
			n, err := strconv.Atoi(parts[i])
			if err != nil {
				return v, false
			}
			v[i] = n
		}
		return v, true
	}
	return v, false
}

func versionText(v [3]int) string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

// tooNewFor is the Minecraft version the pack needs (its
// min_engine_version), when that's newer than server's; a server that
// tries to load it would leave it out.
func (m *manifest) tooNewFor(server string) string {
	have := bedrockVer(server)
	need, ok := parseVersion(m.Header.MinEngine)
	if have == nil || !ok {
		return ""
	}
	text := versionText(need)
	if need[0] >= 26 && need[2] == 0 {
		text = fmt.Sprintf("%d.%d", need[0], need[1]) // as Minecraft writes it since 2026
	}
	if v := bedrockVer(versionText(need)); v != nil && cmpVer(v, have) > 0 {
		return text
	}
	return ""
}

// needsBeta reports a dependency on a beta version of the scripting API.
func (m *manifest) needsBeta() bool {
	for _, d := range m.Dependencies {
		if !strings.HasPrefix(d.ModuleName, "@minecraft/") {
			continue
		}
		var s string
		if json.Unmarshal(d.Version, &s) == nil && strings.Contains(strings.ToLower(s), "beta") {
			return true
		}
	}
	return false
}

// langText looks up a pack's name or description when the manifest gives a
// translation key (such as "pack.name") instead of the text.
func langText(packDir, key string) string {
	if key == "" || strings.Contains(key, " ") || !strings.Contains(key, ".") {
		return key
	}
	for _, lang := range []string{"en_US.lang", "en_GB.lang"} {
		f, err := os.Open(filepath.Join(packDir, "texts", lang))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == key {
				v, _, _ = strings.Cut(v, "\t#")
				f.Close()
				return strings.TrimSpace(v)
			}
		}
		f.Close()
	}
	return key
}

// minecraftText removes the game's § colour codes.
var colourCodes = regexp.MustCompile(`§.`)

func cleanText(s string, max int) string {
	s = strings.TrimSpace(colourCodes.ReplaceAllString(s, ""))
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

// worldPackEntry is one line of world_behavior_packs.json.
type worldPackEntry struct {
	PackID  string `json:"pack_id"`
	Version [3]int `json:"version"`
}

func (s *Server) currentWorld() (string, *properties, error) {
	p, err := readProperties(s.propertiesPath())
	if err != nil {
		return "", nil, err
	}
	name, ok := p.get("level-name")
	if !ok || strings.TrimSpace(name) == "" {
		name = "Bedrock level"
	}
	if err := worldFolderRule(name); err != nil {
		return "", nil, fmt.Errorf("the world folder in server.properties isn't usable: %w", err)
	}
	return name, p, nil
}

func (s *Server) worldDir(name string) string { return filepath.Join(s.dir(), "worlds", name) }

func readWorldPacks(worldDir, kind string) ([]worldPackEntry, error) {
	b, err := os.ReadFile(filepath.Join(worldDir, "world_"+kind+"_packs.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []worldPackEntry
	if err := json.Unmarshal(b, &list); err != nil {
		if err2 := json.Unmarshal(looseJSON(b), &list); err2 != nil {
			return nil, fmt.Errorf("world_%s_packs.json can't be read: %v", kind, err)
		}
	}
	return list, nil
}

func writeWorldPacks(worldDir, kind string, list []worldPackEntry) error {
	if list == nil {
		list = []worldPackEntry{}
	}
	b, _ := json.MarshalIndent(list, "", "  ")
	path := filepath.Join(worldDir, "world_"+kind+"_packs.json")
	if err := os.WriteFile(path+".tmp", b, 0o644); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// installedPacks reads the packs in a world's folder, by kind and ID.
func installedPacks(worldDir string) map[string]map[string]Pack {
	out := map[string]map[string]Pack{}
	for _, kind := range packKinds {
		out[kind] = map[string]Pack{}
		dir := filepath.Join(worldDir, kind+"_packs")
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			pd := filepath.Join(dir, e.Name())
			m, err := readManifest(filepath.Join(pd, "manifest.json"))
			if err != nil {
				continue
			}
			v, _ := parseVersion(m.Header.Version)
			out[kind][m.Header.UUID] = Pack{
				UUID: m.Header.UUID, Version: versionText(v), Kind: kind, Folder: e.Name(),
				Name:        cleanText(langText(pd, m.Header.Name), 80),
				Description: cleanText(langText(pd, m.Header.Description), 300),
				NeedsBeta:   m.needsBeta(), Size: dirSize(pd),
			}
		}
	}
	return out
}

// Packs lists the add-ons of the server's current world.
func (s *Server) Packs() (PacksView, error) {
	if !s.isBedrock() {
		return PacksView{}, errJavaAddons
	}
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	world, p, err := s.currentWorld()
	if err != nil {
		return PacksView{}, err
	}
	v := PacksView{World: world, Packs: []Pack{}}
	req, _ := p.get("texturepack-required")
	v.Required = strings.EqualFold(req, "true")
	st := s.Status()
	v.Running = st.State == StateRunning || st.State == StateStarting
	wd := s.worldDir(world)
	found := installedPacks(wd)
	for _, kind := range packKinds {
		on, _ := readWorldPacks(wd, kind)
		for _, e := range on {
			id := strings.ToLower(e.PackID)
			if pk, ok := found[kind][id]; ok {
				pk.Enabled = true
				found[kind][id] = pk
			} else {
				v.Packs = append(v.Packs, Pack{UUID: id, Version: versionText(e.Version), Kind: kind, Enabled: true, Missing: true,
					Name: "Unknown pack " + id[:min(8, len(id))]})
			}
		}
		for _, pk := range found[kind] {
			v.Packs = append(v.Packs, pk)
		}
	}
	sort.SliceStable(v.Packs, func(i, j int) bool {
		a, b := v.Packs[i], v.Packs[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return v, nil
}

// packDirName is a folder name for a pack: its name, tidied, and the start
// of its ID so two packs with one name don't clash.
func packDirName(name, uuid string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == ' ', r == '-', r == '_':
			b.WriteRune(r)
		}
		if b.Len() >= 40 {
			break
		}
	}
	n := strings.TrimSpace(b.String())
	if n == "" {
		n = "pack"
	}
	return n + " " + uuid[:8]
}

// findPackRoots finds folders with a manifest.json under dir (not looking
// inside a pack once found).
func findPackRoots(dir string) []string {
	var roots []string
	var walk func(d string, depth int)
	walk = func(d string, depth int) {
		if _, err := os.Stat(filepath.Join(d, "manifest.json")); err == nil {
			roots = append(roots, d)
			return
		}
		if depth >= 4 {
			return
		}
		entries, _ := os.ReadDir(d)
		for _, e := range entries {
			if e.IsDir() && e.Name() != "__MACOSX" {
				walk(filepath.Join(d, e.Name()), depth+1)
			}
		}
	}
	walk(dir, 0)
	return roots
}

// unpackNested unpacks .mcpack, .mcaddon and .zip files found inside an
// unpacked add-on (an .mcaddon is usually a zip of .mcpack files).
// Archives inside a pack (a folder with manifest.json) are part of it and
// left alone. Archives can be nested a few deep (a .zip of a .mcaddon of
// .mcpack files).
func unpackNested(ctx context.Context, dir string, lim *unzipLimits) error {
	n := 0
	for round := 0; round < 4; round++ {
		var nested []string
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if _, err := os.Stat(filepath.Join(p, "manifest.json")); err == nil {
					return filepath.SkipDir // a pack
				}
				return nil
			}
			if d.Type().IsRegular() {
				switch strings.ToLower(filepath.Ext(p)) {
				case ".mcpack", ".mcaddon", ".zip":
					nested = append(nested, p)
				}
			}
			return nil
		})
		if len(nested) == 0 {
			return nil
		}
		for _, p := range nested {
			n++
			sub := filepath.Join(dir, ".nested-"+strconv.Itoa(n))
			if err := unzipFile(ctx, p, sub, lim); err != nil {
				return fmt.Errorf("%s: %w", filepath.Base(p), err)
			}
			os.Remove(p)
		}
	}
	return errors.New("it has archives inside archives too many levels deep")
}

// InstallResult says what an upload installed.
type InstallResult struct {
	Installed []Pack   `json:"installed"`
	Skipped   []string `json:"skipped"` // packs that couldn't be installed, and why
	World     string   `json:"world"`
	Running   bool     `json:"running"`
}

// InstallPacks installs the add-on in the uploaded file (a .mcpack or
// .mcaddon) into the current world and turns its packs on. A pack that's
// already there (the same ID) is replaced, as an update.
func (s *Server) InstallPacks(ctx context.Context, upload string) (res InstallResult, err error) {
	if !s.isBedrock() {
		return InstallResult{}, errJavaAddons
	}
	if err := s.EditBlocked(); err != nil {
		return InstallResult{}, err
	}
	staging, err := os.MkdirTemp(s.m.serversDir(), ".upload-"+s.meta.ID+"-")
	if err != nil {
		return InstallResult{}, err
	}
	defer os.RemoveAll(staging)
	lim := &unzipLimits{}
	if err := unzipFile(ctx, upload, staging, lim); err != nil {
		return InstallResult{}, err
	}
	if err := unpackNested(ctx, staging, lim); err != nil {
		return InstallResult{}, err
	}
	roots := findPackRoots(staging)
	if len(roots) == 0 {
		if _, err := findWorldRoot(staging); err == nil {
			return InstallResult{}, errors.New("that's a world, not an add-on; upload it under Worlds instead")
		}
		return InstallResult{}, errors.New("there's no add-on in it (no manifest.json)")
	}

	// Wait for a backup that's copying the world, then keep the files still.
	s.backupMu.Lock()
	defer s.backupMu.Unlock()
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	if err := s.EditBlocked(); err != nil {
		return InstallResult{}, err
	}
	world, _, err := s.currentWorld()
	if err != nil {
		return InstallResult{}, err
	}
	wd := s.worldDir(world)
	if err := os.MkdirAll(wd, 0o755); err != nil {
		return InstallResult{}, err
	}
	res = InstallResult{World: world, Installed: []Pack{}, Skipped: []string{}}
	have := installedPacks(wd)
	lists := map[string][]worldPackEntry{}
	for _, kind := range packKinds {
		if lists[kind], err = readWorldPacks(wd, kind); err != nil {
			return InstallResult{}, err
		}
	}
	seen := map[string]bool{}
	serverVersion := s.Status().Version
	// Whatever was installed is turned on, even if a later pack fails.
	defer func() {
		for _, kind := range packKinds {
			if werr := writeWorldPacks(wd, kind, lists[kind]); werr != nil && err == nil {
				err = werr
			}
		}
	}()
	for _, root := range roots {
		m, err := readManifest(filepath.Join(root, "manifest.json"))
		label := filepath.Base(root)
		if err == nil {
			label = cleanText(langText(root, m.Header.Name), 80)
		}
		if err != nil {
			res.Skipped = append(res.Skipped, label+": "+err.Error())
			continue
		}
		kind, err := m.kind()
		if err != nil {
			res.Skipped = append(res.Skipped, label+": "+err.Error())
			continue
		}
		if seen[m.Header.UUID] {
			continue
		}
		seen[m.Header.UUID] = true
		v, ok := parseVersion(m.Header.Version)
		if !ok {
			res.Skipped = append(res.Skipped, label+": its manifest.json has no version")
			continue
		}
		if need := m.tooNewFor(serverVersion); need != "" {
			res.Skipped = append(res.Skipped, fmt.Sprintf("%s: it needs Minecraft %s or newer, and this server has %s", label, need, serverVersion))
			continue
		}
		packsDir := filepath.Join(wd, kind+"_packs")
		if err := os.MkdirAll(packsDir, 0o755); err != nil {
			return res, err
		}
		folder := packDirName(label, m.Header.UUID)
		target := filepath.Join(packsDir, folder)
		// An older copy of the same pack is moved aside, and only removed
		// once the new one is in place.
		var aside []string
		for _, old := range []string{filepath.Join(packsDir, have[kind][m.Header.UUID].Folder), target} {
			if filepath.Base(old) == kind+"_packs" {
				continue // no older copy
			}
			if _, err := os.Lstat(old); err == nil {
				tmp := filepath.Join(staging, ".old-"+strconv.Itoa(len(aside))+"-"+m.Header.UUID)
				if err := os.Rename(old, tmp); err != nil {
					return res, fmt.Errorf("couldn't replace %s: %w", label, err)
				}
				aside = append(aside, old, tmp)
			}
		}
		if err := os.Rename(root, target); err != nil {
			for i := 0; i+1 < len(aside); i += 2 {
				_ = os.Rename(aside[i+1], aside[i])
			}
			return res, fmt.Errorf("couldn't install %s: %w", label, err)
		}
		// Turn it on (at the top, so it wins over older packs).
		list := []worldPackEntry{{PackID: m.Header.UUID, Version: v}}
		for _, e := range lists[kind] {
			if !strings.EqualFold(e.PackID, m.Header.UUID) {
				list = append(list, e)
			}
		}
		lists[kind] = list
		res.Installed = append(res.Installed, Pack{
			UUID: m.Header.UUID, Version: versionText(v), Kind: kind, Folder: folder, Enabled: true,
			Name: label, NeedsBeta: m.needsBeta(), Size: dirSize(target),
		})
	}
	st := s.Status()
	res.Running = st.State == StateRunning || st.State == StateStarting
	for _, p := range res.Installed {
		s.say(fmt.Sprintf("Installed the %s pack %s %s in %s.", p.Kind, p.Name, p.Version, world))
	}
	if len(res.Installed) > 0 && res.Running {
		s.say("Restart the server to use the new add-ons.")
	}
	if len(res.Installed) == 0 {
		return res, fmt.Errorf("nothing was installed: %s", strings.Join(res.Skipped, "; "))
	}
	return res, nil
}

// findPack returns a pack of the current world by ID.
func (s *Server) findPackLocked(uuid string) (world, wd string, pk Pack, err error) {
	world, _, err = s.currentWorld()
	if err != nil {
		return
	}
	wd = s.worldDir(world)
	uuid = strings.ToLower(uuid)
	for kind, packs := range installedPacks(wd) {
		if p, ok := packs[uuid]; ok {
			p.Kind = kind
			return world, wd, p, nil
		}
	}
	for _, kind := range packKinds {
		on, _ := readWorldPacks(wd, kind)
		for _, e := range on {
			if strings.EqualFold(e.PackID, uuid) {
				return world, wd, Pack{UUID: uuid, Kind: kind, Missing: true, Name: uuid, Version: versionText(e.Version)}, nil
			}
		}
	}
	return world, wd, Pack{}, errors.New("there's no such add-on in this world")
}

// SetPackEnabled turns an add-on of the current world on or off.
func (s *Server) SetPackEnabled(uuid string, on bool) (Pack, error) {
	if err := s.EditBlocked(); err != nil {
		return Pack{}, err
	}
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	_, wd, pk, err := s.findPackLocked(uuid)
	if err != nil {
		return Pack{}, err
	}
	if on && pk.Missing {
		return Pack{}, errors.New("its files aren't in the world; install it again")
	}
	list, err := readWorldPacks(wd, pk.Kind)
	if err != nil {
		return Pack{}, err
	}
	var out []worldPackEntry
	for _, e := range list {
		if !strings.EqualFold(e.PackID, pk.UUID) {
			out = append(out, e)
		}
	}
	if on {
		m, err := readManifest(filepath.Join(wd, pk.Kind+"_packs", pk.Folder, "manifest.json"))
		if err != nil {
			return Pack{}, err
		}
		v, _ := parseVersion(m.Header.Version)
		out = append([]worldPackEntry{{PackID: pk.UUID, Version: v}}, out...)
	}
	if err := writeWorldPacks(wd, pk.Kind, out); err != nil {
		return Pack{}, err
	}
	pk.Enabled = on
	if on {
		s.say("Turned on the add-on " + pk.Name + ". Restart to apply.")
	} else {
		s.say("Turned off the add-on " + pk.Name + ". Restart to apply.")
	}
	return pk, nil
}

// RemovePack deletes an add-on from the current world.
func (s *Server) RemovePack(uuid string) (Pack, error) {
	if err := s.EditBlocked(); err != nil {
		return Pack{}, err
	}
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	_, wd, pk, err := s.findPackLocked(uuid)
	if err != nil {
		return Pack{}, err
	}
	list, err := readWorldPacks(wd, pk.Kind)
	if err != nil {
		return Pack{}, err
	}
	var out []worldPackEntry
	for _, e := range list {
		if !strings.EqualFold(e.PackID, pk.UUID) {
			out = append(out, e)
		}
	}
	if err := writeWorldPacks(wd, pk.Kind, out); err != nil {
		return Pack{}, err
	}
	if pk.Folder != "" {
		if err := os.RemoveAll(filepath.Join(wd, pk.Kind+"_packs", pk.Folder)); err != nil {
			return Pack{}, err
		}
	}
	s.say("Removed the add-on " + pk.Name + ". Restart to apply.")
	return pk, nil
}
