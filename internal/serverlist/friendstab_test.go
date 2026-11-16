package serverlist

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/df-mc/go-nethernet"
	"github.com/df-mc/go-nethernet/discovery"
	"github.com/sandertv/gophertunnel/minecraft"
)

// freeUDPPort returns a UDP port on 127.0.0.1 that was free a moment ago.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// The list listens on each connection the Xbox account hands it, and stops
// when that connection closes. A LAN discovery listener stands in for the
// signaling service here: both are NetherNet signaling.
func TestFriendsTabListensOnEachConnection(t *testing.T) {
	cfg := &Config{ListName: "Family Servers", ListSubtitle: "Pick a server", DataDir: t.TempDir(), MenuTitle: "Pick a server"}
	key, err := loadOrCreateIdentityKey(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan nethernet.Signaling, 1)
	cfg.FriendsSignaling = ch
	listenCfg := minecraft.ListenConfig{AuthenticationDisabled: true, StatusProvider: minecraft.NewStatusProvider(cfg.ListName, cfg.ListSubtitle), MaximumPlayers: 100}
	conf := nethernet.ListenConfig{API: webrtcAPI(cfg), DisableTrickleICE: true, IssueServerIdentity: identityIssuer(key), VerifyClientIdentity: verifyOfferIdentity}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	go serveFriendsTab(ctx, cfg, listenCfg, conf)

	port := freeUDPPort(t)
	account, err := discovery.ListenConfig{}.Listen("127.0.0.1:" + strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer account.Close()
	account.ServerData(&discovery.ServerData{ServerName: "x", LevelName: "y"})
	ch <- account

	friend, err := discovery.ListenConfig{BroadcastAddress: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}}.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer friend.Close()
	var id uint64
	for id == 0 && ctx.Err() == nil {
		for nid := range friend.Responses() {
			id = nid
		}
		time.Sleep(100 * time.Millisecond)
	}
	if id == 0 {
		t.Fatal("the stand-in account never answered")
	}
	addr := strconv.FormatUint(id, 10)

	// A console that signs its offer gets the list's identity back, and so
	// connects to a dialer that insists on one.
	signed, err := nethernet.Dialer{Identity: mustSelfSigned(t)}.DialContext(ctx, addr, friend)
	if err != nil {
		t.Fatalf("joining with an identity: %v", err)
	}
	signed.Close()
	// One that doesn't gets none back.
	anon, err := nethernet.Dialer{AllowIdentitylessServer: true}.DialContext(ctx, addr, friend)
	if err != nil {
		t.Fatalf("joining without an identity: %v", err)
	}
	anon.Close()

	// The account's connection closing stops that listener.
	_ = account.Close()
	time.Sleep(500 * time.Millisecond)
	short, cancelShort := context.WithTimeout(ctx, 3*time.Second)
	defer cancelShort()
	if c, err := (nethernet.Dialer{AllowIdentitylessServer: true}).DialContext(short, addr, friend); err == nil {
		c.Close()
		t.Error("still joinable after the account's connection closed")
	}
}

// Answers keep the identity only for consoles whose offer had one.
func TestAnswerIdentityFollowsOffer(t *testing.T) {
	rec := &recordingSignaling{}
	a := &answerIdentity{Signaling: rec}
	w := offerWatcher{nopNotifier{}, a}
	w.NotifySignal(&nethernet.Signal{Type: nethernet.SignalTypeOffer, ConnectionID: 1, Data: "v=0\r\n"})
	w.NotifySignal(&nethernet.Signal{Type: nethernet.SignalTypeOffer, ConnectionID: 2, Data: "v=0\r\na=identity:x\r\n"})
	answer := "v=0\r\na=identity:abc\r\nm=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n"
	for _, id := range []uint64{1, 2} {
		_ = a.Signal(context.Background(), &nethernet.Signal{Type: nethernet.SignalTypeAnswer, ConnectionID: id, Data: answer})
	}
	if len(rec.sent) != 2 || strings.Contains(rec.sent[0].Data, "a=identity") || rec.sent[1].Data != answer {
		t.Errorf("sent: %+v", rec.sent)
	}
}

type recordingSignaling struct {
	nethernet.Signaling
	sent []*nethernet.Signal
}

func (r *recordingSignaling) Signal(_ context.Context, s *nethernet.Signal) error {
	r.sent = append(r.sent, s)
	return nil
}

type nopNotifier struct{}

func (nopNotifier) NotifySignal(*nethernet.Signal) bool { return true }
