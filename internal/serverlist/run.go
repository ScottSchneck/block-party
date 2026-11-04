// Package serverlist lets game consoles join your Bedrock servers. It is the
// panel's built-in replacement for BedrockConnect and has two parts:
//
//   - a tiny DNS server that answers the featured-server names consoles look
//     up (The Hive, Lifeboat and others) with this server's address, and
//     forwards every other lookup for devices on the home network;
//   - a server-list "server" that consoles join instead of the featured
//     server. It shows a menu of your servers and transfers the console to
//     the one picked.
//
// Consoles reach the list over RakNet today. NetherNet support is built in
// but off by default; see docs/design.md for the test results behind that.
package serverlist

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"syscall"
	"time"

	"github.com/df-mc/go-nethernet"
	"github.com/df-mc/go-nethernet/endpoint"
	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
)

// Run starts the built-in DNS and the server list and blocks until ctx is
// cancelled. It returns an error only for problems that stop it from
// starting, such as a port already being in use.
func Run(ctx context.Context, cfg *Config) error {
	slog.Info("starting console server list",
		"minecraftVersion", protocol.CurrentVersion, "protocol", protocol.CurrentProtocol,
		"listIP", cfg.ListIP, "publicIP", cfg.PublicIP, "connection", cfg.Connection,
		"raknet", cfg.RakNet, "nethernet", cfg.NetherNet, "authOff", cfg.AuthOff, "requireSignIn", cfg.RequireSignIn, "playerServers", cfg.PlayerServers)
	slog.Info("menu world settings", "gameMode", cfg.LoadGameMode, "spawnY", cfg.LoadSpawnY,
		"chunksAt", cfg.LoadChunksAt, "chunkStyle", cfg.LoadChunkStyle, "chunkRange", cfg.LoadChunkRange)
	if cfg.AuthOff {
		slog.Warn("AUTH_OFF is on: Xbox sign-in is not checked. Use this only for local testing.")
	} else {
		sharedVerifier.get() // start fetching the sign-in keys before the first console arrives
	}

	cfg.players = newPlayerStore(cfg.DataDir)

	if servers, err := loadServers(cfg.ServersFile); err != nil {
		slog.Error("server list file problem (consoles will see an error until it is fixed)", "error", err)
	} else {
		for _, s := range servers {
			slog.Info("server in list", "name", s.Name, "address", s.Address, "port", s.Port)
		}
	}

	if cfg.DNSEnabled {
		if err := newDNSServer(cfg).start(); err != nil {
			return fmt.Errorf("built-in DNS could not start; is something else using port %d? %w", cfg.DNSPort, err)
		}
	}

	// Shown under Friends → LAN Games: consoles on the home network find the
	// list there without any DNS change (seen on a PS5).
	status := minecraft.NewStatusProvider(cfg.ListName, cfg.ListSubtitle)
	listenCfg := minecraft.ListenConfig{
		// Sign-in is checked in checkSignIn instead, which never turns a
		// console away over a sign-in format it doesn't recognise.
		AuthenticationDisabled: true,
		StatusProvider:         status,
		MaximumPlayers:         100,
		ErrorLog:               slog.Default().With("src", "minecraft"),
		PacketFunc:             recordPacket,
	}

	if cfg.RakNet {
		l, err := listenWithRetry(ctx, "RakNet", func() (*minecraft.Listener, error) {
			return listenCfg.Listen("raknet", net.JoinHostPort("", strconv.Itoa(cfg.ListPort)))
		})
		if err != nil {
			return err
		}
		defer l.Close()
		slog.Info("RakNet listener ready", "udpPort", cfg.ListPort)
		go acceptLoop(cfg, l, "RakNet")
	}

	if cfg.NetherNet {
		key, err := loadOrCreateIdentityKey(cfg)
		if err != nil {
			return fmt.Errorf("could not load the NetherNet identity key: %w", err)
		}
		handler := endpoint.HandlerConfig{Logger: slog.Default().With("src", "signaling")}.New()
		if err := startSignaling(cfg, handler); err != nil {
			return fmt.Errorf("NetherNet join endpoint could not start: %w", err)
		}
		n := minecraft.NetherNet{
			Signaling: handler,
			ListenConfig: nethernet.ListenConfig{
				Log:                 slog.Default().With("src", "nethernet"),
				API:                 webrtcAPI(cfg),
				DisableTrickleICE:   true,
				AllowAnonymous:      cfg.AuthOff,
				IssueServerIdentity: identityIssuer(key),
			},
		}
		l, err := listenWithRetry(ctx, "NetherNet", func() (*minecraft.Listener, error) {
			return listenCfg.ListenNetwork(n, handler.NetworkID())
		})
		if err != nil {
			return err
		}
		defer l.Close()
		slog.Info("NetherNet listener ready", "webrtcUDPPorts", fmt.Sprintf("%d-%d", cfg.RTCPortMin, cfg.RTCPortMax))
		go acceptLoop(cfg, l, "NetherNet")
	}

	<-ctx.Done()
	slog.Info("console server list stopping")
	return nil
}

// listenWithRetry starts a listener. A port already in use is fatal. Anything
// else, usually Mojang's sign-in service being unreachable (the listener
// fetches its keys at startup to check Xbox sign-ins), is retried every 15
// seconds so a short internet outage doesn't stop the container.
func listenWithRetry(ctx context.Context, name string, listen func() (*minecraft.Listener, error)) (*minecraft.Listener, error) {
	for attempt := 1; ; attempt++ {
		l, err := listen()
		if err == nil {
			return l, nil
		}
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, fmt.Errorf("%s listener could not start: the port is already in use by something else on this IP: %w", name, err)
		}
		slog.Error(name+" listener could not start; retrying in 15 seconds. If this repeats, check that the container can reach the internet (it needs Mojang's sign-in service to verify Xbox accounts)",
			"attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}

func acceptLoop(cfg *Config, l *minecraft.Listener, transport string) {
	for {
		c, err := l.Accept()
		if err != nil {
			slog.Warn(transport+" listener stopped", "error", err)
			return
		}
		go handleSession(cfg, l, c.(*minecraft.Conn), transport)
	}
}

func identityIssuer(key *ecdsa.PrivateKey) func(context.Context) (*nethernet.Identity, error) {
	return func(context.Context) (*nethernet.Identity, error) {
		return nethernet.GenerateServerIdentity(key, "self")
	}
}

// webrtcAPI pins NetherNet's gameplay traffic to a known UDP port range so it
// can be forwarded on a router, and advertises the public IP when one is set.
func webrtcAPI(cfg *Config) *webrtc.API {
	var s webrtc.SettingEngine
	// Multicast DNS isn't needed (consoles reach us by IP) and would bind 5353.
	s.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	if err := s.SetEphemeralUDPPortRange(cfg.RTCPortMin, cfg.RTCPortMax); err != nil {
		slog.Warn("could not limit WebRTC ports", "error", err)
	}
	if ip := advertisedIP(cfg); ip.IsValid() {
		// Advertise the public IP as an extra (server-reflexive) candidate so
		// home devices still use the LAN address and friends use the public one.
		s.SetNAT1To1IPs([]string{ip.String()}, webrtc.ICECandidateTypeSrflx)
		slog.Info("NetherNet tells consoles outside the house to connect to this internet address", "ip", ip)
	}
	return webrtc.NewAPI(webrtc.WithSettingEngine(s))
}

// advertisedIP is the internet address consoles outside the house connect
// to over NetherNet: PUBLIC_IP, or else the one Friends outside the house
// found (while it's on). Without it, consoles outside on Minecraft 26.60,
// which only join over NetherNet, are only told the home address and can't
// connect. It's decided when the list starts: if the home's address changes,
// the panel needs a restart for consoles outside.
func advertisedIP(cfg *Config) netip.Addr {
	if cfg.PublicIP.IsValid() {
		return cfg.PublicIP
	}
	if o := cfg.outside(); o.Enabled && o.IP.IsValid() && o.IP.Is4() && !o.IP.IsPrivate() {
		return o.IP
	}
	return netip.Addr{}
}
