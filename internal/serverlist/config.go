package serverlist

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// Config holds every setting for the console server list. LoadConfig reads it
// from environment variables so a container can be configured from its
// template without editing files; the panel will later fill it from its own
// settings instead.
type Config struct {
	// ListIP is the address the built-in DNS hands to devices on the home
	// network for the featured-server names. Normally this container's own IP.
	ListIP netip.Addr
	// ListIPFound is set when LIST_IP wasn't given and ListIP is the
	// container's own address (see LogListIP).
	ListIPFound bool
	// PublicIP is handed to devices outside the home network, and advertised
	// in WebRTC candidates. Optional; empty means home-only.
	PublicIP netip.Addr

	DNSEnabled     bool
	DNSPort        int
	DNSUpstream    string
	DNSAnswerWorld bool // answer the redirect names for internet clients (friends mode)
	RedirectNames  []string

	ListPort int
	// Connection is what the server list offers consoles:
	//   raknet    - the older connection type only (what works today)
	//   nethernet - the newer connection type only
	//   both      - both; NetherNetNames can limit NetherNet to some names
	Connection string
	// NetherNetNames limits NetherNet to these featured-server names when
	// Connection is "both". Consoles joining any other name are turned away
	// from NetherNet and fall back to RakNet. Empty means every name.
	NetherNetNames map[string]bool
	// Signaling is how the NetherNet join endpoint answers: auto (HTTPS and
	// plain HTTP on the same port), http, or tls.
	Signaling string
	// ExtraSignalingPorts are more TCP ports that serve the NetherNet join
	// endpoint and log every connection, to see where consoles try next.
	ExtraSignalingPorts []int
	RakNet              bool
	NetherNet           bool

	RTCPortMin uint16
	RTCPortMax uint16

	// Load settings control how the empty menu world is built, to find what
	// makes consoles finish loading quickly. LOAD_PRESET=bedrockconnect copies
	// BedrockConnect's choices; the individual settings override the preset.
	LoadGameMode   int32   // 0 survival, 1 creative, 6 spectator
	LoadSpawnY     float32 // height the player appears at
	LoadChunksAt   string  // "radius": after the view distance is agreed; "startgame": right after the game starts
	LoadChunkStyle string  // "standard" or "bedrockconnect"
	LoadChunkRange int32   // chunks sent in each direction from the spawn

	ServersFile string
	DataDir     string
	AuthOff     bool // testing only: skip Xbox sign-in checks
	// RequireSignIn turns away consoles whose Xbox sign-in can't be verified.
	// Off by default: the game server does its own checks.
	RequireSignIn bool
	// PlayerServers lets players connect to a server by address from the
	// console menu and save it to their own list.
	PlayerServers bool

	players *playerStore

	// PanelServers, when set, returns the servers the panel itself runs.
	// They're listed in the menu after the ones in SERVERS_FILE, at this
	// container's address.
	PanelServers func() []PanelServer

	// OutsideInfo, when set, says how friends outside the house may join
	// (the panel's outside access settings). It's asked each time, so
	// changes apply straight away.
	OutsideInfo func() Outside

	// OnPick, when set, is called when a player picks a server from the
	// menu, just before they're sent there. verified says whether their
	// Xbox sign-in was checked.
	OnPick func(gamertag, xuid string, verified bool, address string, port uint16)

	MenuTitle string
	// ListName and ListSubtitle are what consoles show for the list under
	// Friends → LAN Games (and on the featured-server tile).
	ListName     string
	ListSubtitle string
	LogLevel     slog.Level
}

// Outside is how friends outside the house may join.
type Outside struct {
	Enabled  bool           // outside access is on
	Host     string         // the address consoles outside are sent to
	IP       netip.Addr     // the home's internet address, for DNS answers
	Consoles bool           // answer consoles' DNS lookups from the internet
	Open     func(int) bool // whether the panel server on that port is open to them
}

func (c *Config) outside() Outside {
	if c.OutsideInfo == nil {
		return Outside{}
	}
	return c.OutsideInfo()
}

// Server is one entry in the console server list. The JSON shape matches
// BedrockConnect's custom_servers.json so an existing file can be reused.
type Server struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Port    uint16 `json:"port"`
	IconURL string `json:"iconUrl,omitempty"`
	// NetherNetPort: see PanelServer (not read from servers.json).
	NetherNetPort uint16 `json:"-"`
}

// defaultRedirectNames are the featured-server hostnames consoles look up.
// Taken from BedrockConnect's "Using your own DNS server" wiki page.
var defaultRedirectNames = []string{
	"geo.hivebedrock.network",
	"hivebedrock.network",
	"play.inpvp.net",
	"mco.lbsg.net",
	"play.galaxite.net",
	"play.enchanted.gg",
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(env(key, strconv.FormatBool(def)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func envInt(key string, def int) (int, error) {
	v := env(key, strconv.Itoa(def))
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number, got %q", key, v)
	}
	return n, nil
}

// LoadConfig reads the settings from environment variables. See the README
// for the list.
func LoadConfig() (*Config, error) {
	c := &Config{
		DNSUpstream: env("DNS_UPSTREAM", "1.1.1.1:53"),
		// Plain HTTP by default: a console asking for HTTPS by IP address
		// (LAN Games, Minecraft 26.60) refuses the list's self-made
		// certificate and gives up with error Terracotta, while one turned
		// away falls back to plain HTTP and asks the player to trust the
		// server. Nobody can get a trusted certificate for a home IP or for a
		// featured server's name, so HTTPS only helps experiments.
		Signaling:      strings.ToLower(env("SIGNALING", "http")),
		ServersFile:    env("SERVERS_FILE", "/config/servers.json"),
		DataDir:        env("DATA_DIR", "/data"),
		MenuTitle:      env("MENU_TITLE", "Pick a server"),
		ListName:       env("LIST_NAME", "Server List"),
		ListSubtitle:   env("LIST_SUBTITLE", "Pick a server"),
		DNSEnabled:     envBool("DNS_ENABLED", true),
		DNSAnswerWorld: envBool("DNS_ANSWER_INTERNET", false),
		AuthOff:        envBool("AUTH_OFF", false),
		RequireSignIn:  envBool("REQUIRE_SIGN_IN", false),
		PlayerServers:  envBool("PLAYER_SERVERS", true),
	}

	var err error
	listIP := env("LIST_IP", "")
	if listIP == "" {
		ip, err := ownIPv4()
		if err != nil {
			return nil, err
		}
		c.ListIP, c.ListIPFound = ip, true
	} else {
		ip, err := netip.ParseAddr(listIP)
		if err != nil || !ip.Is4() {
			return nil, fmt.Errorf("LIST_IP must be an IPv4 address, got %q", listIP)
		}
		c.ListIP = ip
	}

	if pub := env("PUBLIC_IP", ""); pub != "" {
		ip, err := netip.ParseAddr(pub)
		if err != nil || !ip.Is4() {
			return nil, fmt.Errorf("PUBLIC_IP must be an IPv4 address, got %q", pub)
		}
		c.PublicIP = ip
	}

	if c.DNSPort, err = envInt("DNS_PORT", 53); err != nil {
		return nil, err
	}
	if c.ListPort, err = envInt("LIST_PORT", 19132); err != nil {
		return nil, err
	}
	minP, err := envInt("RTC_PORT_MIN", 19300)
	if err != nil {
		return nil, err
	}
	maxP, err := envInt("RTC_PORT_MAX", 19319)
	if err != nil {
		return nil, err
	}
	if minP < 1024 || maxP > 65535 || minP > maxP {
		return nil, fmt.Errorf("RTC_PORT_MIN/RTC_PORT_MAX must be a range between 1024 and 65535")
	}
	c.RTCPortMin, c.RTCPortMax = uint16(minP), uint16(maxP)

	// CONNECTION picks what consoles are offered. Both by default: Minecraft
	// 26.60 (27 October 2026) stops connecting over RakNet, and consoles on
	// older versions still use it. The first build used SIGNALING=off and
	// RAKNET=false for this, so those still work.
	c.Connection = strings.ToLower(env("CONNECTION", ""))
	if c.Connection == "" {
		switch {
		case c.Signaling == "off":
			c.Connection = "raknet"
		case !envBool("RAKNET", true):
			c.Connection = "nethernet"
		default:
			c.Connection = "both"
		}
	}
	switch c.Connection {
	case "raknet":
		c.RakNet = true
	case "nethernet":
		c.NetherNet = true
	case "both":
		c.RakNet, c.NetherNet = true, true
	default:
		return nil, fmt.Errorf("CONNECTION must be raknet, nethernet or both, got %q", c.Connection)
	}
	if c.Signaling == "off" {
		c.Signaling = "http"
	}
	switch c.Signaling {
	case "auto", "http", "tls":
	default:
		return nil, fmt.Errorf("SIGNALING must be auto, http or tls, got %q", c.Signaling)
	}

	c.NetherNetNames = map[string]bool{}
	for _, n := range splitList(env("NETHERNET_NAMES", "")) {
		c.NetherNetNames[strings.ToLower(n)] = true
	}
	if len(c.NetherNetNames) > 0 && c.Connection != "both" {
		slog.Warn("NETHERNET_NAMES only applies when CONNECTION=both; ignoring it")
		c.NetherNetNames = map[string]bool{}
	}

	extra := env("EXTRA_SIGNALING_PORTS", "80,443")
	if v := strings.ToLower(extra); v == "none" || v == "off" {
		extra = "" // "none" turns the extra ports off; an empty value means the default
	}
	for _, p := range splitList(extra) {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("EXTRA_SIGNALING_PORTS must be port numbers separated by commas, got %q", p)
		}
		if n != c.ListPort {
			c.ExtraSignalingPorts = append(c.ExtraSignalingPorts, n)
		}
	}

	c.RedirectNames = defaultRedirectNames
	if names := splitList(env("REDIRECT_NAMES", "")); len(names) > 0 {
		c.RedirectNames = names
	}

	if err := loadLoadSettings(c); err != nil {
		return nil, err
	}

	switch strings.ToLower(env("LOG_LEVEL", "info")) {
	case "debug":
		c.LogLevel = slog.LevelDebug
	case "warn":
		c.LogLevel = slog.LevelWarn
	default:
		c.LogLevel = slog.LevelInfo
	}
	return c, nil
}

func loadLoadSettings(c *Config) error {
	preset := strings.ToLower(env("LOAD_PRESET", "standard"))
	switch preset {
	case "standard":
		c.LoadGameMode, c.LoadSpawnY, c.LoadChunksAt, c.LoadChunkStyle, c.LoadChunkRange = 6, 100, "radius", "standard", 4
	case "bedrockconnect":
		c.LoadGameMode, c.LoadSpawnY, c.LoadChunksAt, c.LoadChunkStyle, c.LoadChunkRange = 0, 0, "startgame", "bedrockconnect", 3
	default:
		return fmt.Errorf("LOAD_PRESET must be standard or bedrockconnect, got %q", preset)
	}
	switch strings.ToLower(env("LOAD_GAMEMODE", "")) {
	case "":
	case "survival":
		c.LoadGameMode = 0
	case "creative":
		c.LoadGameMode = 1
	case "spectator":
		c.LoadGameMode = 6
	default:
		return fmt.Errorf("LOAD_GAMEMODE must be survival, creative or spectator")
	}
	if v := env("LOAD_SPAWN_Y", ""); v != "" {
		f, err := strconv.ParseFloat(v, 32)
		if err != nil {
			return fmt.Errorf("LOAD_SPAWN_Y must be a number, got %q", v)
		}
		c.LoadSpawnY = float32(f)
	}
	if v := strings.ToLower(env("LOAD_CHUNKS_AT", "")); v != "" {
		if v != "radius" && v != "startgame" {
			return fmt.Errorf("LOAD_CHUNKS_AT must be radius or startgame, got %q", v)
		}
		c.LoadChunksAt = v
	}
	if v := strings.ToLower(env("LOAD_CHUNK_STYLE", "")); v != "" {
		if v != "standard" && v != "bedrockconnect" {
			return fmt.Errorf("LOAD_CHUNK_STYLE must be standard or bedrockconnect, got %q", v)
		}
		c.LoadChunkStyle = v
	}
	if v := env("LOAD_CHUNK_RANGE", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 8 {
			return fmt.Errorf("LOAD_CHUNK_RANGE must be a number from 1 to 8, got %q", v)
		}
		c.LoadChunkRange = int32(n)
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// allowsNetherNet reports whether a console joining the given name (a Host
// header or TLS server name, with or without a port) should be offered
// NetherNet.
func (c *Config) allowsNetherNet(host string) bool {
	if len(c.NetherNetNames) == 0 {
		return true
	}
	h := strings.ToLower(host)
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	return c.NetherNetNames[strings.TrimSuffix(h, ".")]
}

// loadServers reads the server list fresh each time a console opens the menu,
// so edits to the file show up without restarting the container.
// PanelServer is a server run by the panel. Only running ones are listed;
// the others still hide entries of the same name from SERVERS_FILE.
type PanelServer struct {
	Name    string
	Port    int
	Running bool
	// NetherNetPort, when not 0, is where consoles that reached the list
	// over NetherNet are sent instead of Port (a Java server's Geyser can't
	// take NetherNet on the Java server's own TCP port).
	NetherNetPort int
}

func loadServers(path string) ([]Server, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil // optional now that the panel lists its own servers
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var servers []Server
	if err := json.Unmarshal(b, &servers); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	valid := servers[:0]
	for _, s := range servers {
		if s.Name == "" || s.Address == "" || s.Port == 0 {
			slog.Warn("skipping server entry with a missing name, address or port", "entry", s)
			continue
		}
		valid = append(valid, s)
	}
	return valid, nil
}

// LogListIP says where the list's address came from, once logging is set up.
func (c *Config) LogListIP() {
	switch {
	case !c.ListIPFound:
	case dockerBridge.Contains(c.ListIP):
		slog.Warn("this container is on Docker's own bridge network, which consoles can't reach; give it its own IP on your home network (Unraid: Network Type br0 with a fixed IP)", "ip", c.ListIP)
	default:
		slog.Info("LIST_IP isn't set; using this container's own address", "ip", c.ListIP)
	}
}

// interfaceAddrs is net.InterfaceAddrs (tests replace it).
var interfaceAddrs = net.InterfaceAddrs

// dockerBridge is Docker's default bridge network.
var dockerBridge = netip.MustParsePrefix("172.17.0.0/16")

// ownIPv4 finds the container's address when LIST_IP isn't set: its one
// IPv4 address that isn't loopback. With more than one it can't tell which
// network the consoles are on, so LIST_IP is needed.
func ownIPv4() (netip.Addr, error) {
	addrs, err := interfaceAddrs()
	if err != nil {
		return netip.Addr{}, fmt.Errorf("LIST_IP isn't set and this container's address can't be read (%v): set LIST_IP to its IP on your home network", err)
	}
	var found []netip.Addr
	for _, a := range addrs {
		p, err := netip.ParsePrefix(a.String())
		if err != nil {
			continue
		}
		ip := p.Addr().Unmap()
		if ip.Is4() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			found = append(found, ip)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return netip.Addr{}, errors.New("LIST_IP isn't set and this container has no IPv4 address: set LIST_IP to its IP on your home network (for example 10.0.1.116)")
	}
	return netip.Addr{}, fmt.Errorf("LIST_IP isn't set and this container has several addresses (%v): set LIST_IP to the one on your home network", found)
}
