package serverlist

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/df-mc/go-nethernet"
	"github.com/df-mc/go-nethernet/discovery"
	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
)

// LAN Games on newer consoles. From Minecraft 26.60, consoles look for games
// on the home network with NetherNet's LAN discovery, a broadcast on UDP
// 7551, instead of the RakNet ping the list answers on its own port. So the
// list answers there too. A console that picks it sets up the connection over
// the same UDP channel (encrypted with the game's shared LAN key), not with a
// web request, so no certificate is involved: since 26.62, consoles refuse
// the list's own certificate for featured-server names and no longer fall
// back to plain HTTP when turned away.

// lanDiscoveryPort is where consoles broadcast their LAN search.
const lanDiscoveryPort = discovery.DefaultPort

// startLANDiscovery answers LAN searches and accepts the consoles that pick
// the list, into the same menu as every other way in.
func startLANDiscovery(ctx context.Context, cfg *Config, listenCfg minecraft.ListenConfig, conf nethernet.ListenConfig) (*minecraft.Listener, error) {
	d, err := discovery.ListenConfig{Log: slog.Default().With("src", "lan")}.Listen(net.JoinHostPort("", strconv.Itoa(lanDiscoveryPort)))
	if err != nil {
		return nil, err
	}
	sig := &lanSignaling{Listener: d, card: lanServerData(cfg)}
	d.ServerData(sig.card)
	conf.Log = slog.Default().With("src", "nethernet-lan")
	// Consoles joining a LAN game send no Microsoft-issued identity with
	// their connection offer (the featured-server route does), so the
	// offer can't require one. Sign-in is still read from the login itself
	// (checkSignIn), and the game server a console picks checks it again.
	conf.AllowAnonymous = true
	conf.API = lanWebRTCAPI(cfg)
	// Send the list's addresses as separate signals after the answer, as a
	// game hosting a LAN world does. With them only inside the answer, a
	// Switch never connected (the path search timed out). The HTTP route
	// can't do this and keeps its own setting.
	conf.DisableTrickleICE = false
	l, err := listenWithRetry(ctx, "NetherNet LAN", func() (*minecraft.Listener, error) {
		return listenCfg.ListenNetwork(minecraft.NetherNet{Signaling: sig, ListenConfig: conf}, d.NetworkID())
	})
	if err != nil {
		_ = d.Close()
		return nil, err
	}
	slog.Info("LAN Games listener ready (Minecraft 26.60 and later)", "udpPort", lanDiscoveryPort, "name", cfg.ListName)
	go acceptLoop(cfg, l, "NetherNet LAN")
	return l, nil
}

// lanSignaling keeps the list's own LAN card. The Minecraft listener pushes
// its status every few seconds, and the discovery library would rebuild the
// card from it, losing the game version (so consoles would call the list
// incompatible), the nonce and the name order. Only the player count is
// taken from it.
type lanSignaling struct {
	*discovery.Listener
	mu   sync.Mutex
	card *discovery.ServerData
}

// Notify logs the network addresses a console offers (at debug), so a LAN
// join that can't connect shows why.
func (s *lanSignaling) Notify(n nethernet.Notifier) (stop func()) {
	return s.Listener.Notify(candidateLogger{n})
}

type candidateLogger struct{ nethernet.Notifier }

func (c candidateLogger) NotifySignal(sig *nethernet.Signal) bool {
	switch sig.Type {
	case nethernet.SignalTypeCandidate:
		slog.Debug("LAN console offered an address", "connectionID", sig.ConnectionID, "candidate", sig.Data)
	case nethernet.SignalTypeOffer:
		for _, line := range strings.Split(sig.Data, "\n") {
			if line = strings.TrimSpace(line); strings.HasPrefix(line, "a=candidate:") {
				slog.Debug("LAN console offered an address", "connectionID", sig.ConnectionID, "candidate", strings.TrimPrefix(line, "a="))
			}
		}
	}
	return c.Notifier.NotifySignal(sig)
}

// Signal logs what the list sends back to a console (at debug), and when
// sending fails.
func (s *lanSignaling) Signal(ctx context.Context, sig *nethernet.Signal) error {
	err := s.Listener.Signal(ctx, sig)
	if err != nil {
		slog.Warn("could not answer a LAN console", "type", sig.Type, "connectionID", sig.ConnectionID, "error", err)
		return err
	}
	if sig.Type == nethernet.SignalTypeAnswer {
		n := 0
		for _, line := range strings.Split(sig.Data, "\n") {
			if line = strings.TrimSpace(line); strings.HasPrefix(line, "a=candidate:") {
				n++
				slog.Debug("told a LAN console this address", "connectionID", sig.ConnectionID, "candidate", strings.TrimPrefix(line, "a="))
			}
		}
		slog.Debug("answered a LAN console", "connectionID", sig.ConnectionID, "addresses", n)
	} else {
		slog.Debug("sent a LAN console a signal", "type", sig.Type, "connectionID", sig.ConnectionID, "data", sig.Data)
	}
	return nil
}

func (s *lanSignaling) PongData(b []byte) {
	parts := strings.Split(string(b), ";")
	if len(parts) < 6 {
		return
	}
	players, err1 := strconv.Atoi(parts[4])
	max, err2 := strconv.Atoi(parts[5])
	if err1 != nil || err2 != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := *s.card
	c.PlayerCount, c.MaxPlayerCount = int32(players), int32(max)
	s.card = &c
	s.Listener.ServerData(&c)
}

// lanWebRTCAPI is webrtcAPI for LAN joins, with multicast DNS lookups on:
// consoles on the home network may hide their address behind a random
// ".local" name, which can only be looked up that way. The list doesn't
// announce a name of its own (it gives its address).
func lanWebRTCAPI(cfg *Config) *webrtc.API {
	var s webrtc.SettingEngine
	s.SetICEMulticastDNSMode(ice.MulticastDNSModeQueryOnly)
	if err := s.SetEphemeralUDPPortRange(cfg.RTCPortMin, cfg.RTCPortMax); err != nil {
		slog.Warn("could not limit WebRTC ports", "error", err)
	}
	return webrtc.NewAPI(webrtc.WithSettingEngine(s))
}

// lanServerData is the list's card under LAN Games: its name on top, the
// subtitle under it.
func lanServerData(cfg *Config) *discovery.ServerData {
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	return &discovery.ServerData{
		LevelName:      cfg.ListName,
		ServerName:     cfg.ListSubtitle,
		Protocol:       int32(protocol.CurrentProtocol),
		Version:        protocol.CurrentVersion,
		GameType:       discovery.GameTypeAdventure,
		PlayerCount:    0,
		MaxPlayerCount: 100,
		// Xbox sign-in is checked as for every other way in (checkSignIn);
		// consoles signed out can only join when AUTH_OFF is set.
		AcceptsOnlineAuth:     true,
		AcceptsSelfSignedAuth: cfg.AuthOff,
		Nonce:                 hex.EncodeToString(nonce),
		ConnectionType:        4, // LAN signaling, as the game itself announces
	}
}
