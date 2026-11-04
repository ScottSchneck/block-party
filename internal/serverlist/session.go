package serverlist

import (
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/go-gl/mathgl/mgl32"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

const sessionTimeout = 10 * time.Minute

var sessionCounter atomic.Uint64

// emptyChunkPayload is an all-air overworld chunk: no sub-chunks, 24 biome
// sections (the first a single plains biome, the rest "same as previous"),
// and a zero border-block count. The player floats in an empty void while the
// menu is open.
var emptyChunkPayload = func() []byte {
	b := []byte{0x01, 0x02} // palette header for 0 bits per value, then plains (id 1, zigzag-encoded)
	for i := 0; i < 23; i++ {
		b = append(b, 0xFF) // copy the previous section
	}
	return append(b, 0x00) // no border blocks
}()

// handleSession runs one console's visit: spawn it into an empty world, show
// the server menu, and transfer it to whatever it picks.
func handleSession(cfg *Config, l *minecraft.Listener, conn *minecraft.Conn, transport string) {
	id := sessionCounter.Add(1)
	ident, client := conn.IdentityData(), conn.ClientData()
	log := slog.With("session", id, "gamertag", ident.DisplayName, "via", transport)
	signedIn := "not checked (AUTH_OFF)"
	var check signIn
	if !cfg.AuthOff {
		check = checkSignIn(loginPayload(conn.LocalAddr(), conn.RemoteAddr()), sharedVerifier.get(), transport == "NetherNet")
		signedIn = check.String()
	}
	log.Info("console connected to the server list",
		"from", remoteIP(conn.RemoteAddr()),
		"xuid", ident.XUID,
		"signedIn", signedIn,
		"device", deviceName(client.DeviceOS),
		"gameVersion", client.GameVersion,
		"joinedAddress", client.ServerAddress,
	)
	if !cfg.AuthOff && !check.Verified {
		log.Warn("could not verify this console's Xbox sign-in; letting it pick a server anyway (the game server checks sign-in itself)",
			append([]any{"reason", check.How}, check.Token...)...)
		if cfg.RequireSignIn {
			_ = l.Disconnect(conn, "Sign in to your Microsoft account to join.")
			return
		}
	}
	defer log.Info("console left the server list")
	defer conn.Close()
	defer forgetTimeline(conn.LocalAddr(), conn.RemoteAddr())
	connected := time.Now()
	logTimeline := func(outcome string) {
		if steps := takeTimeline(conn.LocalAddr(), conn.RemoteAddr()); steps != "" {
			log.Info("load timeline", "outcome", outcome, "steps", steps)
		}
	}

	// Send the empty terrain as soon as the console asks for its view
	// distance. The console waits for terrain before it finishes spawning, and
	// StartGame waits for the console to finish spawning, so sending it any
	// later leaves both sides waiting until the console gives up (about 18s).
	// The view distance is answered with three packets in a row:
	// ChunkRadiusUpdated, PlayStatus (spawn) and CreativeContent. Waiting for
	// the last one keeps terrain from landing between the first two.
	trigger := uint32(packet.IDCreativeContent)
	if cfg.LoadChunksAt == "startgame" {
		trigger = packet.IDItemRegistry // the last packet sent together with StartGame
	}
	chunkRequested, stopWaiting := waitForPacket(conn.LocalAddr(), conn.RemoteAddr(), trigger)
	defer stopWaiting()
	go func() {
		select {
		case <-chunkRequested:
			sendEmptyWorld(conn, cfg)
			log.Debug("empty terrain sent")
		case <-time.After(30 * time.Second):
		}
	}()

	finishRescue := rescueViewDistance(conn, cfg, log)

	err := conn.StartGameTimeout(minecraft.GameData{
		WorldName:        "Server list",
		EntityUniqueID:   1,
		EntityRuntimeID:  1,
		PlayerGameMode:   cfg.LoadGameMode,
		WorldGameMode:    1,
		Difficulty:       0,
		BaseGameVersion:  "*",
		PlayerPosition:   mgl32.Vec3{0.5, cfg.LoadSpawnY, 0.5},
		WorldSpawn:       protocol.BlockPos{0, int32(cfg.LoadSpawnY), 0},
		Time:             6000,
		DayCycleLockTime: 6000,
		GameRules: []protocol.GameRule{
			{Name: "dodaylightcycle", Value: false},
			{Name: "doweathercycle", Value: false},
		},
		PlayerPermissions:       1,
		ChunkRadius:             cfg.LoadChunkRange,
		UseBlockNetworkIDHashes: true,
	}, 30*time.Second)
	finishRescue()
	if err != nil {
		log.Warn("could not start the menu world", "error", err)
		logTimeline("menu world did not load")
		return
	}
	log.Info("menu world loaded", "took", time.Since(connected).Round(100*time.Millisecond))

	m := &menu{
		cfg:       cfg,
		conn:      conn,
		l:         l,
		log:       log,
		store:     cfg.players,
		key:       playerKey(ident.XUID, ident.DisplayName),
		gamertag:  ident.DisplayName,
		host:      transferHost(cfg, conn.RemoteAddr()),
		outside:   fromOutside(conn.RemoteAddr()),
		connected: connected,
		onFirst:   func() { logTimeline("menu shown") },
	}

	// The console has spawned (StartGame waits for that), so show the menu
	// after a moment for its loading screen to clear.
	fallback := time.AfterFunc(750*time.Millisecond, func() { m.showMain("spawned", true) })
	defer fallback.Stop()

	_ = conn.SetReadDeadline(time.Now().Add(sessionTimeout))
	for {
		pk, err := conn.ReadPacket()
		if err != nil {
			var disc minecraft.DisconnectError
			if !errors.As(err, &disc) {
				log.Debug("connection ended", "error", err)
			}
			if !m.wasShown() {
				logTimeline("left before the menu appeared")
			}
			return
		}
		switch p := pk.(type) {
		case *packet.SetLocalPlayerAsInitialised:
			m.showMain("spawned", true)
		case *packet.ModalFormResponse:
			target := m.handle(p)
			if target == nil {
				continue
			}
			// A console that came over NetherNet joins over NetherNet, which
			// some servers take on another port.
			port := target.Port
			if transport == "NetherNet" && target.NetherNetPort != 0 {
				port = target.NetherNetPort
			}
			log.Info("player picked a server; sending the console there",
				"server", target.Name, "address", target.Address, "port", port)
			if cfg.OnPick != nil {
				// Pass the identity that was actually checked when there is one.
				name, xuid := ident.DisplayName, ident.XUID
				if check.Verified {
					name, xuid = check.Name, check.XUID
				}
				cfg.OnPick(name, xuid, check.Verified, target.Address, target.Port)
			}
			if err := conn.WritePacket(&packet.Transfer{Address: target.Address, Port: port}); err != nil {
				log.Warn("could not send the transfer", "error", err)
			}
			_ = conn.Flush()
			// Give the console a moment to act on the transfer before closing.
			time.Sleep(2 * time.Second)
			return
		}
	}
}

// rescueViewDistance works around a timing gap in gophertunnel v1.62.0.
// Conn.StartGame sends StartGame and only afterwards marks the console's
// view-distance request as expected. A request that arrives inside that gap is
// set aside for ReadPacket instead of being answered, and both sides then wait
// until the join times out. Over loopback that happened in about 1 of 10 joins;
// consoles answer more slowly, but a busy host can still widen the gap.
//
// This waits for the request to arrive. If the library hasn't answered it
// shortly after, the request must be the one set aside, so it is taken with
// ReadPacket (which returns set-aside packets straight away) and answered here
// the same way the library would. Nothing else reads the connection until
// StartGame returns, and StartGame can't return before the answer is sent.
//
// Call it just before StartGame: it starts watching straight away and does the
// rest on its own goroutine. Call the returned function once StartGame has
// returned; it waits for the rescue to finish, so the session can then read
// the connection and set its deadline without racing it.
func rescueViewDistance(conn *minecraft.Conn, cfg *Config, log *slog.Logger) (finish func()) {
	local, remote := conn.LocalAddr(), conn.RemoteAddr()
	requested, stop1 := waitForPacket(remote, local, packet.IDRequestChunkRadius)
	answered, stop2 := waitForPacket(local, remote, packet.IDChunkRadiusUpdated)
	started, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		defer stop1()
		defer stop2()
		rescue(conn, cfg, log, requested, answered, started)
	}()
	return func() {
		close(started)
		<-done
	}
}

func rescue(conn *minecraft.Conn, cfg *Config, log *slog.Logger, requested, answered, started <-chan struct{}) {
	select {
	case <-requested:
	case <-answered:
		return
	case <-started:
		return
	}
	select {
	case <-answered:
		return // the normal case
	case <-started:
		return
	case <-time.After(500 * time.Millisecond):
	}
	// The session sets its own deadline after this returns.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		pk, err := conn.ReadPacket()
		if err != nil {
			log.Warn("could not recover the console's view-distance request", "error", err)
			return
		}
		if p, ok := pk.(*packet.RequestChunkRadius); ok {
			radius := p.ChunkRadius
			if cfg.LoadChunkRange != 0 {
				radius = cfg.LoadChunkRange
			}
			_ = conn.WritePacket(&packet.ChunkRadiusUpdated{ChunkRadius: radius})
			_ = conn.WritePacket(&packet.PlayStatus{Status: packet.PlayStatusPlayerSpawn})
			_ = conn.WritePacket(&packet.CreativeContent{})
			_ = conn.Flush()
			log.Debug("answered a view-distance request the library set aside")
			return
		}
		// Anything else set aside before spawning isn't needed by the menu.
	}
}

func sendEmptyWorld(conn *minecraft.Conn, cfg *Config) {
	r := cfg.LoadChunkRange
	payload := emptyChunkPayload
	lo, hi := -r, r // standard: a full square around the spawn
	if cfg.LoadChunkStyle == "bedrockconnect" {
		payload = bedrockConnectChunkPayload
		lo, hi = -r, r-1 // BedrockConnect sends -3..2
	}
	_ = conn.WritePacket(&packet.NetworkChunkPublisherUpdate{
		Position: protocol.BlockPos{0, int32(cfg.LoadSpawnY), 0},
		Radius:   uint32(r * 16),
	})
	for x := lo; x <= hi; x++ {
		for z := lo; z <= hi; z++ {
			_ = conn.WritePacket(&packet.LevelChunk{
				Position:      protocol.ChunkPos{x, z},
				SubChunkCount: 0,
				RawPayload:    payload,
			})
		}
	}
	_ = conn.Flush()
}

// bedrockConnectChunkPayload copies BedrockConnect's empty chunk: 258 zero
// bytes followed by an empty network NBT compound tag.
var bedrockConnectChunkPayload = append(make([]byte, 258), 0x0a, 0x00, 0x00)

// transferHost is the address consoles use to reach servers in this
// container: the home address, or the public one for consoles outside.
func transferHost(cfg *Config, remote net.Addr) string {
	if fromOutside(remote) {
		// The address itself, not the hostname: a console whose DNS is
		// this panel can't look the hostname up (only the featured names
		// are answered for consoles outside).
		if o := cfg.outside(); o.Enabled && o.IP.IsValid() {
			return o.IP.String()
		} else if o.Enabled && o.Host != "" {
			return o.Host
		}
		if cfg.PublicIP.IsValid() {
			return cfg.PublicIP.String()
		}
	}
	return cfg.ListIP.String()
}

// fromOutside reports whether a console is joining from outside the house.
func fromOutside(remote net.Addr) bool {
	ap, err := netip.ParseAddrPort(remoteIP(remote))
	return err == nil && !isHomeAddr(ap.Addr().Unmap())
}

// remoteIP shortens NetherNet's long peer description ("<id> (<id>) (udp4
// host 10.0.1.20:5000 ...)") to just the console's address.
var addrPattern = regexp.MustCompile(`(\d{1,3}(?:\.\d{1,3}){3}:\d+)`)

func remoteIP(a net.Addr) string {
	s := a.String()
	if m := addrPattern.FindString(s); m != "" {
		return m
	}
	return s
}

func deviceName(os protocol.DeviceOS) string {
	switch os {
	case protocol.DeviceAndroid:
		return "Android"
	case protocol.DeviceIOS:
		return "iOS"
	case protocol.DeviceWin10, protocol.DeviceWin32:
		return "Windows"
	case protocol.DeviceOrbis:
		return "PlayStation"
	case protocol.DeviceNX:
		return "Switch"
	case protocol.DeviceXBOX:
		return "Xbox"
	default:
		return "other (" + strconv.Itoa(int(os)) + ")"
	}
}
