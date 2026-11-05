package servers

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Settings: the everyday server.properties options as labelled controls.
//
// Only the options below can be changed from the Settings tab; everything
// else stays reachable in the raw file editor. Saving rewrites only the
// lines that changed, so comments and unknown keys are kept. Bedrock reads
// server.properties when it starts, so changes apply after a restart.

// Option is one choice in a select field.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Field is one setting on the Settings tab.
type Field struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Help     string   `json:"help"`
	Type     string   `json:"type"` // "select", "bool", "int", "text" or "readonly"
	Options  []Option `json:"options,omitempty"`
	Min      int      `json:"min,omitempty"`
	Max      int      `json:"max,omitempty"`
	Value    string   `json:"value"`
	InFile   bool     `json:"inFile"`         // false: not in server.properties, Value is Bedrock's default
	WarnWhen string   `json:"warnWhen"`       // show Warning when the value is changed to this ("*": any change)
	Warning  string   `json:"warning"`        // shown when WarnWhen matches
	Link     string   `json:"link,omitempty"` // for readonly fields: which tab changes it
}

// Group is a titled set of fields.
type Group struct {
	Title  string  `json:"title"`
	Fields []Field `json:"fields"`
}

// SettingsView is the Settings tab for one server.
type SettingsView struct {
	Name    string  `json:"name"` // the panel's name for the server
	Groups  []Group `json:"groups"`
	Running bool    `json:"running"`
	// Lobby is a lobby's (Velocity's) servers, instead of Groups.
	Lobby *LobbyView `json:"lobby,omitempty"`
}

type fieldDef struct {
	Field
	def      string // Bedrock's default when the key is missing
	managed  bool   // set by the panel itself; shown read-only
	validate func(string) error
}

func opts(pairs ...string) []Option {
	var o []Option
	for i := 0; i+1 < len(pairs); i += 2 {
		o = append(o, Option{Value: pairs[i], Label: pairs[i+1]})
	}
	return o
}

type schemaGroup struct {
	title  string
	fields []fieldDef
}

// schema returns the settings a server of this type shows.
func (s *Server) schema() []schemaGroup {
	if s.isJava() {
		return javaSchema
	}
	return settingsSchema
}

var settingsSchema = []schemaGroup{
	{"Game", []fieldDef{
		{Field: Field{Key: "gamemode", Label: "Game mode", Type: "select",
			Options: opts("survival", "Survival", "creative", "Creative", "adventure", "Adventure"),
			Help:    "The game mode players get when they join for the first time. Players who've played before keep theirs unless “Force game mode” is on."}, def: "survival"},
		{Field: Field{Key: "force-gamemode", Label: "Force game mode", Type: "bool",
			Help: "Put everyone in the game mode above every time they join, even players who've played before."}, def: "false"},
		{Field: Field{Key: "difficulty", Label: "Difficulty", Type: "select",
			Options: opts("peaceful", "Peaceful: no hostile mobs", "easy", "Easy", "normal", "Normal", "hard", "Hard")}, def: "easy"},
		{Field: Field{Key: "allow-cheats", Label: "Cheats", Type: "bool",
			Help:     "Lets operators use commands like /time set day and /give. Worlds with cheats on don't earn achievements.",
			WarnWhen: "true", Warning: "Turning cheats on permanently turns off achievements for this world."}, def: "false"},
		{Field: Field{Key: "default-player-permission-level", Label: "New players are", Type: "select",
			Options:  opts("visitor", "Visitors: can look around but not build", "member", "Members: can build and play normally", "operator", "Operators: can use commands (not recommended)"),
			Help:     "What someone can do the first time they join. You can make individual players operators on the Players tab.",
			WarnWhen: "operator", Warning: "Everyone who joins will be able to use commands, change the time and give themselves items."}, def: "member"},
	}},
	{"Players and chat", []fieldDef{
		{Field: Field{Key: "max-players", Label: "Maximum players", Type: "int", Min: 1, Max: 200,
			Help: "How many people can play at once."}, def: "10"},
		{Field: Field{Key: "player-idle-timeout", Label: "Kick idle players after (minutes)", Type: "int", Min: 0, Max: 1440,
			Help: "Players who don't move for this long are disconnected, freeing their spot. 0 means never."}, def: "30"},
		{Field: Field{Key: "chat-restriction", Label: "Chat", Type: "select",
			Options: opts("None", "On", "Dropped", "Off: messages aren't sent, and players are told", "Disabled", "Hidden: only operators see the chat box"),
			Help:    "Turn chat off to keep things friendly with younger players."}, def: "None"},
		{Field: Field{Key: "disable-custom-skins", Label: "Block custom skins", Type: "bool",
			Help: "Only allow skins from the Minecraft store and the game itself, which keeps out skins players made elsewhere."}, def: "false"},
		{Field: Field{Key: "disable-player-interaction", Label: "Players ignore each other", Type: "bool",
			Help: "Tells games to ignore other players when interacting with the world, for example so players can't block each other."}, def: "false"},
		{Field: Field{Key: "texturepack-required", Label: "Require the world's resource packs", Type: "bool",
			Help: "Players must download the world's resource packs to join."}, def: "false"},
	}},
	{"World", []fieldDef{
		{Field: Field{Key: "server-name", Label: "Name shown in the game", Type: "text", Max: 64,
			Help: "Shown to players on the pause screen and in the friends list."}, def: "Dedicated Server",
			validate: func(v string) error { return textRule(v, ";") }},
		{Field: Field{Key: "level-name", Label: "World folder", Type: "text", Max: 64,
			Help:     "Which world the server loads, by its folder name in worlds/.",
			WarnWhen: "*", Warning: "Changing this makes the server load, or create, a different world. Your current world isn't deleted: change it back to return to it."},
			def: "Bedrock level", validate: worldFolderRule},
		{Field: Field{Key: "level-seed", Label: "World seed", Type: "readonly",
			Help: "Only used when a world is first created, so it can't be changed here."}},
		{Field: Field{Key: "view-distance", Label: "View distance (chunks)", Type: "int", Min: 5, Max: 64,
			Help: "How far players can see. Higher values use more memory and processor on the server."}, def: "32"},
		{Field: Field{Key: "tick-distance", Label: "Active distance (chunks)", Type: "int", Min: 4, Max: 12,
			Help: "How far from each player the world keeps running: crops growing, mobs moving, redstone. Higher values use more processor."}, def: "4"},
	}},
	{"Security", []fieldDef{
		{Field: Field{Key: "online-mode", Label: "Check Xbox sign-in", Type: "bool",
			Help:     "Players must be signed in to a Microsoft account. Keep this on.",
			WarnWhen: "false", Warning: "With sign-in checks off, players joining over your home network, including through the console server list, can use any gamertag, including yours or an operator's, and the allowlist can't tell. (Players connecting over the internet are always checked.)"}, def: "true"},
		{Field: Field{Key: "allow-list", Label: "Allowlist", Type: "readonly", Link: "players",
			Help: "Turned on and off, and edited, on the Players tab."}, def: "false", managed: true},
	}},
	{"Performance", []fieldDef{
		{Field: Field{Key: "max-threads", Label: "Processor threads", Type: "int", Min: 0, Max: 64,
			Help: "How many processor threads the server may use. 0 means as many as it likes."}, def: "0"},
	}},
	{"Set by the panel", []fieldDef{
		{Field: Field{Key: "server-port", Label: "Port", Type: "readonly",
			Help: "Given out by the panel so servers never clash."}, managed: true},
		{Field: Field{Key: "transport", Label: "Connection type", Type: "readonly",
			Help: "How consoles connect: NetherNet (newer) or RakNet. Change it in the advanced editor if you need to."}, managed: true},
		{Field: Field{Key: "enable-lan-visibility", Label: "LAN visibility", Type: "readonly",
			Help: "Kept off: the console server list answers LAN searches for all the panel's servers."}, managed: true},
	}},
}

// javaSchema is the Settings tab of a Java (Paper) server.
var javaSchema = []schemaGroup{
	{"Game", []fieldDef{
		{Field: Field{Key: "gamemode", Label: "Game mode", Type: "select",
			Options: opts("survival", "Survival", "creative", "Creative", "adventure", "Adventure", "spectator", "Spectator"),
			Help:    "The game mode players get when they join for the first time."}, def: "survival"},
		{Field: Field{Key: "force-gamemode", Label: "Force game mode", Type: "bool",
			Help: "Put everyone in the game mode above every time they join."}, def: "false"},
		{Field: Field{Key: "difficulty", Label: "Difficulty", Type: "select",
			Options: opts("peaceful", "Peaceful: no hostile mobs", "easy", "Easy", "normal", "Normal", "hard", "Hard")}, def: "easy"},
		{Field: Field{Key: "hardcore", Label: "Hardcore", Type: "bool",
			Help:     "One life: players who die can only watch.",
			WarnWhen: "true", Warning: "In hardcore, anyone who dies becomes a spectator for good."}, def: "false"},
		{Field: Field{Key: "allow-flight", Label: "Allow flying (mods and some plugins)", Type: "bool",
			Help: "Off kicks players the server thinks are flying without being allowed to."}, def: "false"},
		{Field: Field{Key: "spawn-protection", Label: "Protected area around spawn (blocks)", Type: "int", Min: 0, Max: 64,
			Help: "Only operators can build this close to the world spawn. 0 means no protected area."}, def: "16"},
	}},
	{"Players and chat", []fieldDef{
		{Field: Field{Key: "max-players", Label: "Maximum players", Type: "int", Min: 1, Max: 200,
			Help: "How many people can play at once."}, def: "20"},
		{Field: Field{Key: "player-idle-timeout", Label: "Kick idle players after (minutes)", Type: "int", Min: 0, Max: 1440,
			Help: "0 means never."}, def: "0"},
		{Field: Field{Key: "motd", Label: "Message in the server list", Type: "text", Max: 59,
			Help: "Shown under the server's name in Minecraft's multiplayer list."}, def: "A Minecraft Server",
			validate: func(v string) error { return textRule(v, "") }},
	}},
	{"World", []fieldDef{
		{Field: Field{Key: "level-name", Label: "World folder", Type: "text", Max: 64,
			Help:     "Which world the server loads, by its folder name in worlds/.",
			WarnWhen: "*", Warning: "Changing this makes the server load, or create, a different world. Your current world isn't deleted: change it back to return to it."},
			def: javaWorldDefault, validate: worldFolderRule},
		{Field: Field{Key: "level-seed", Label: "World seed", Type: "readonly",
			Help: "Only used when a world is first created, so it can't be changed here."}},
		{Field: Field{Key: "view-distance", Label: "View distance (chunks)", Type: "int", Min: 3, Max: 32,
			Help: "How far players can see. Higher values use more memory and processor."}, def: "10"},
		{Field: Field{Key: "simulation-distance", Label: "Active distance (chunks)", Type: "int", Min: 3, Max: 32,
			Help: "How far from each player the world keeps running: crops, mobs, redstone."}, def: "10"},
	}},
	{"Security", []fieldDef{
		{Field: Field{Key: "online-mode", Label: "Check Minecraft accounts", Type: "bool",
			Help:     "Players must sign in with a real Minecraft account. Keep this on.",
			WarnWhen: "false", Warning: "With account checks off, anyone can join with any name, including yours or an operator's, and the allowlist can't tell."}, def: "true"},
		{Field: Field{Key: "white-list", Label: "Allowlist", Type: "readonly", Link: "players",
			Help: "Turned on and off, and edited, on the Players tab."}, def: "false", managed: true},
	}},
	{"Set by the panel", []fieldDef{
		{Field: Field{Key: "server-port", Label: "Port", Type: "readonly",
			Help: "Given out by the panel so servers never clash."}, managed: true},
	}},
}

func textRule(v, forbidden string) error {
	switch {
	case strings.TrimSpace(v) == "":
		return errors.New("can't be empty")
	case v != strings.TrimSpace(v):
		return errors.New("can't start or end with a space")
	case utf8.RuneCountInString(v) > 64:
		return errors.New("is too long (64 characters at most)")
	case strings.IndexFunc(v, unicode.IsControl) >= 0:
		return errors.New("must be one line of ordinary characters")
	case strings.ContainsAny(v, forbidden):
		return fmt.Errorf("can't contain any of %s", forbidden)
	}
	return nil
}

// worldFolderRule checks a level-name: it's a folder name inside worlds/.
func worldFolderRule(v string) error {
	if err := textRule(v, ";/\\`?*<>|\":"); err != nil {
		return err
	}
	if v == "." || v == ".." || strings.HasPrefix(v, ".") || strings.HasSuffix(v, ".") {
		return errors.New("can't start or end with a dot")
	}
	return nil
}

func findField(key string) (*fieldDef, bool) { return findIn(settingsSchema, key) }

func findIn(schema []schemaGroup, key string) (*fieldDef, bool) {
	for gi := range schema {
		for fi := range schema[gi].fields {
			if f := &schema[gi].fields[fi]; f.Key == key {
				return f, true
			}
		}
	}
	return nil, false
}

// check validates a new value for an editable field.
func (f *fieldDef) check(v string) error {
	switch f.Type {
	case "select":
		for _, o := range f.Options {
			if o.Value == v {
				return nil
			}
		}
		return errors.New("isn't one of the choices")
	case "bool":
		if v != "true" && v != "false" {
			return errors.New("must be on or off")
		}
	case "int":
		n, err := strconv.Atoi(v)
		if err != nil || strconv.Itoa(n) != v {
			return errors.New("must be a whole number")
		}
		if n < f.Min || n > f.Max {
			return fmt.Errorf("must be between %d and %d", f.Min, f.Max)
		}
	case "text":
		if f.validate != nil {
			return f.validate(v)
		}
		return textRule(v, "")
	default:
		return errors.New("can't be changed here")
	}
	return nil
}

func (s *Server) propertiesPath() string { return filepath.Join(s.dir(), "server.properties") }

// Settings returns the Settings tab for this server.
func (s *Server) Settings() (SettingsView, error) {
	if s.isHytale() {
		return s.hytaleSettings()
	}
	if s.isJava() && s.isVelocity() {
		st := s.Status()
		lv := s.m.lobbyView(s)
		return SettingsView{Name: st.Name, Groups: []Group{}, Running: st.State == StateRunning || st.State == StateStarting, Lobby: &lv}, nil
	}
	s.filesMu.Lock()
	p, err := readProperties(s.propertiesPath())
	s.filesMu.Unlock()
	if err != nil {
		return SettingsView{}, fmt.Errorf("couldn't read server.properties: %w", err)
	}
	st := s.Status()
	v := SettingsView{Name: st.Name, Running: st.State == StateRunning || st.State == StateStarting}
	for _, g := range s.schema() {
		group := Group{Title: g.title}
		for _, f := range g.fields {
			field := f.Field
			if val, ok := p.get(f.Key); ok {
				field.Value, field.InFile = val, true
			} else {
				field.Value = f.def
			}
			if f.Key == "allow-list" {
				if _, ok := p.get("allow-list"); !ok {
					if val, ok := p.get("white-list"); ok {
						field.Value, field.InFile = val, true
					}
				}
			}
			group.Fields = append(group.Fields, field)
		}
		v.Groups = append(v.Groups, group)
	}
	return v, nil
}

// SettingsChange is one changed setting, for the console and the page.
type SettingsChange struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// SaveSettings validates and writes changed settings. Either all are saved
// or none are. It returns what changed.
func (s *Server) SaveSettings(values map[string]string) ([]SettingsChange, error) {
	if s.isHytale() {
		return s.saveHytaleSettings(values)
	}
	if s.isJava() && s.isVelocity() {
		return nil, errors.New("a lobby's settings are its servers (Settings, Servers behind this lobby)")
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		f, ok := findIn(s.schema(), k)
		if !ok || f.managed || f.Type == "readonly" {
			return nil, fmt.Errorf("%q can't be changed on the Settings tab", k)
		}
		switch f.Type {
		case "text": // phones like to add a space after the last word
			values[k] = strings.TrimSpace(values[k])
		case "int":
			if n, err := strconv.Atoi(strings.TrimSpace(values[k])); err == nil {
				values[k] = strconv.Itoa(n) // "+05" → "5"
			}
		}
		if err := f.check(values[k]); err != nil {
			return nil, fmt.Errorf("%s %s", f.Label, err)
		}
	}

	// server.properties is also written under s.mu when the server starts;
	// take both locks in the usual order.
	s.filesMu.Lock()
	s.mu.Lock()
	if v, ok := values["online-mode"]; ok && v != "true" && s.meta.Outside {
		s.mu.Unlock()
		s.filesMu.Unlock()
		return nil, ErrOutsideOpen
	}
	var changes []SettingsChange
	p, err := readProperties(s.propertiesPath())
	if err == nil {
		for _, k := range keys {
			f, _ := findIn(s.schema(), k)
			old, ok := p.get(k)
			if !ok {
				old = f.def
			}
			if old == values[k] {
				continue
			}
			p.set(k, values[k])
			changes = append(changes, SettingsChange{Key: k, Label: f.Label, From: old, To: values[k]})
		}
		if len(changes) > 0 {
			err = p.write(s.propertiesPath())
		}
	}
	if err == nil {
		for _, c := range changes {
			s.addLineLocked(fmt.Sprintf("[Panel] Setting changed: %s: %s → %s", c.Label, c.From, c.To))
		}
		if len(changes) > 0 && s.cmd != nil {
			s.addLineLocked("[Panel] Restart the server to apply the new settings.")
		}
	}
	s.mu.Unlock()
	s.filesMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("couldn't save server.properties: %w", err)
	}
	return changes, nil
}

const maxRawProperties = 256 << 10

// RawProperties returns server.properties as text, for the advanced editor.
func (s *Server) RawProperties() (string, error) {
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	path := s.propertiesPath()
	if s.isHytale() {
		path = s.hytaleConfigPath()
	}
	b, err := os.ReadFile(path)
	if s.isHytale() && errors.Is(err, os.ErrNotExist) {
		return "", errors.New("config.json is made when the server first starts")
	}
	return string(b), err
}

// SaveRawProperties replaces server.properties with text from the advanced
// editor. The settings the panel manages (ports, LAN visibility) are put
// back as the panel needs them.
func (s *Server) SaveRawProperties(text string) error {
	if len(text) > maxRawProperties {
		return errors.New("that's too long for a server.properties file")
	}
	if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return errors.New("the file contains characters that aren't text")
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if s.isHytale() {
		return s.saveHytaleRawConfig(text)
	}
	p := &properties{lines: strings.Split(strings.TrimRight(text, "\n"), "\n")}
	if dups := p.duplicates(); len(dups) > 0 {
		return fmt.Errorf("these settings appear more than once, so it isn't clear which value counts: %s", strings.Join(dups, ", "))
	}
	// The world must be a folder inside this server's worlds/, never
	// somewhere else (such as another server's world).
	if v, ok := p.get("level-name"); ok {
		if err := worldFolderRule(v); err != nil {
			return fmt.Errorf("level-name: %w", err)
		}
	}
	s.filesMu.Lock()
	s.mu.Lock()
	if problem := outsideProblemFor(p, s.isJava()); s.meta.Outside && problem != "" {
		s.mu.Unlock()
		s.filesMu.Unlock()
		return fmt.Errorf("%w (with this file, %s)", ErrOutsideOpen, problem)
	}
	err := p.write(s.propertiesPath())
	if err == nil {
		if aerr := s.applyOwnProperties(); aerr != nil {
			err = fmt.Errorf("the file was saved, but the panel couldn't put back its port settings: %w", aerr)
		}
		s.addLineLocked("[Panel] server.properties was edited in the advanced editor.")
		if s.cmd != nil {
			s.addLineLocked("[Panel] Restart the server to apply the changes.")
		}
	}
	s.mu.Unlock()
	s.filesMu.Unlock()
	return err
}

// Rename changes the server's name in the panel and the console menu.
func (s *Server) Rename(name string) error {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 40 || strings.ContainsAny(name, "\r\n\t") {
		return errNameInvalid
	}
	// Check and change under the manager lock so two renames (or a rename
	// and a new server) can't end up with the same name.
	s.m.mu.Lock()
	for _, other := range s.m.servers {
		if other != s && strings.EqualFold(other.displayName(), name) {
			s.m.mu.Unlock()
			return fmt.Errorf("there's already a server called %q", name)
		}
	}
	s.mu.Lock()
	if s.state == StateImporting {
		s.mu.Unlock()
		s.m.mu.Unlock()
		return ErrImporting
	}
	old := s.meta.Name
	s.meta.Name = name
	s.addLineLocked(fmt.Sprintf("[Panel] Renamed from %q to %q.", old, name))
	s.mu.Unlock()
	s.m.mu.Unlock()
	return s.saveMeta()
}
