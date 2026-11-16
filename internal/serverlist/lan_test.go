package serverlist

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/df-mc/go-nethernet"
	"github.com/df-mc/go-nethernet/discovery"
	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	"github.com/sandertv/gophertunnel/minecraft"
)

// A console on Minecraft 26.60 or later finds the list under LAN Games
// through LAN discovery on UDP 7551, and joins it over the same channel.
func TestLANGamesDiscoveryAndJoin(t *testing.T) {
	cfg := &Config{ListName: "Family Servers", ListSubtitle: "Pick a server", DataDir: t.TempDir(),
		MenuTitle: "Pick a server", RTCPortMin: 0, RTCPortMax: 0}
	key, err := loadOrCreateIdentityKey(cfg)
	if err != nil {
		t.Fatal(err)
	}
	listenCfg := minecraft.ListenConfig{AuthenticationDisabled: true, StatusProvider: minecraft.NewStatusProvider(cfg.ListName, cfg.ListSubtitle), MaximumPlayers: 100}
	conf := nethernet.ListenConfig{API: webrtcAPI(cfg), DisableTrickleICE: true, AllowAnonymous: false,
		IssueServerIdentity: identityIssuer(key), VerifyClientIdentity: verifyOfferIdentity}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	l, err := startLANDiscovery(ctx, cfg, listenCfg, conf)
	if err != nil {
		t.Skipf("can't listen on UDP %d here: %v", lanDiscoveryPort, err)
	}
	defer l.Close()

	// The console's search, sent to this machine instead of broadcast.
	console, err := discovery.ListenConfig{BroadcastAddress: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: lanDiscoveryPort}, Log: slog.Default()}.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer console.Close()
	var id uint64
	var card discovery.ServerData
	for id == 0 && ctx.Err() == nil {
		for nid, data := range console.Responses() {
			if err := card.UnmarshalBinary(data); err == nil {
				id = nid
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if id == 0 {
		t.Fatal("the list didn't answer the LAN search")
	}
	if card.LevelName != "Family Servers" || card.ServerName != "Pick a server" || card.ConnectionType != 4 ||
		!card.AcceptsOnlineAuth || card.Nonce == "" || card.Version == "" || card.Protocol == 0 {
		t.Errorf("LAN card: %+v", card)
	}

	// Picking it: the connection is set up over the LAN channel, with the
	// server identity the HTTP route uses. (The Minecraft login on top is the
	// same as every other way in; gophertunnel's own test client can't do it
	// over LAN because it puts the bare network ID in the login's server
	// address, which gophertunnel's listener rejects.)
	nd := nethernet.Dialer{Identity: mustSelfSigned(t)}
	conn, err := nd.DialContext(ctx, strconv.FormatUint(id, 10), console)
	if err != nil {
		t.Fatalf("joining over LAN: %v", err)
	}
	conn.Close()

	// A real console sends no identity with a LAN offer, even with sign-in
	// checks on (AUTH_OFF unset), and gets none back.
	anon, err := nethernet.Dialer{AllowIdentitylessServer: true}.DialContext(ctx, strconv.FormatUint(id, 10), console)
	if err != nil {
		t.Fatalf("joining over LAN without an identity: %v", err)
	}
	anon.Close()

	// Consoles may hide their address behind a random ".local" name.
	var se webrtc.SettingEngine
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeQueryAndGather)
	hidden, err := nethernet.Dialer{AllowIdentitylessServer: true, API: webrtc.NewAPI(webrtc.WithSettingEngine(se))}.DialContext(ctx, strconv.FormatUint(id, 10), console)
	if err != nil {
		t.Fatalf("joining over LAN with a .local address: %v", err)
	}
	hidden.Close()
}

func mustSelfSigned(t *testing.T) *nethernet.Identity {
	k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id, err := nethernet.SelfSignedIdentity(k, "https://authorization.franchise.minecraft-services.net/")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// The Minecraft listener's status pushes keep the list's card, updating only
// the player count.
func TestLANCardKeepsVersionOnStatusUpdates(t *testing.T) {
	d, err := discovery.ListenConfig{}.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := &lanSignaling{Listener: d, card: lanServerData(&Config{ListName: "Family Servers", ListSubtitle: "Pick"})}
	s.PongData([]byte("MCPE;Family Servers;2225;1.26.60;3;100;123;Pick;Adventure;2;19132;19132;0;"))
	if s.card.PlayerCount != 3 || s.card.Version == "" || s.card.LevelName != "Family Servers" || s.card.Nonce == "" {
		t.Errorf("card after a status push: %+v", s.card)
	}
}

func TestWithoutIdentity(t *testing.T) {
	in := "v=0\r\na=msid-semantic: WMS\r\na=identity:abc\r\nm=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n"
	want := "v=0\r\na=msid-semantic: WMS\r\nm=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n"
	if got := withoutIdentity(in); got != want {
		t.Errorf("got %q", got)
	}
}

// A console that sends no identity gets an answer without one; a console
// that sends one gets the list's back.
func TestLANAnswerIdentityFollowsOffer(t *testing.T) {
	s := &lanSignaling{}
	s.noteOffer(&nethernet.Signal{Type: nethernet.SignalTypeOffer, ConnectionID: 1, Data: "v=0\r\n"})
	s.noteOffer(&nethernet.Signal{Type: nethernet.SignalTypeOffer, ConnectionID: 2, Data: "v=0\r\na=identity:xyz\r\n"})
	answer := "v=0\r\na=identity:abc\r\nm=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n"
	if got := s.outgoing(&nethernet.Signal{Type: nethernet.SignalTypeAnswer, ConnectionID: 1, Data: answer}).Data; strings.Contains(got, "a=identity") {
		t.Errorf("answer to an unsigned offer kept its identity: %q", got)
	}
	if got := s.outgoing(&nethernet.Signal{Type: nethernet.SignalTypeAnswer, ConnectionID: 2, Data: answer}).Data; got != answer {
		t.Errorf("answer to a signed offer changed: %q", got)
	}
}

func TestPionLogsOnlyICE(t *testing.T) {
	var f pionLogs
	if f.NewLogger("ice").(pionLogger).quiet || !f.NewLogger("sctp").(pionLogger).quiet || !f.NewLogger("mdns").(pionLogger).quiet {
		t.Error("only ICE logs should pass")
	}
}
