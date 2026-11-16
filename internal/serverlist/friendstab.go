package serverlist

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/df-mc/go-nethernet"
	"github.com/sandertv/gophertunnel/minecraft"
)

// Friends tab. When the panel is signed in to an Xbox account (Settings →
// Friends outside the house), friends of that account see the list as a
// joinable world under Play → Friends, and their consoles connect through
// Minecraft's signaling service, as to a friend's world. Each connection the
// account makes to the signaling service arrives on cfg.FriendsSignaling,
// and the list listens on it until it closes. Consoles land in the same menu
// as every other way in; those outside the house see only the servers open
// to friends outside.

// serveFriendsTab listens on each signaling connection the Xbox account
// makes, until ctx is done.
func serveFriendsTab(ctx context.Context, cfg *Config, listenCfg minecraft.ListenConfig, conf nethernet.ListenConfig) {
	conf.Log = slog.Default().With("src", "nethernet-xbox")
	// Addresses go as separate signals after the answer, as a game hosting
	// a world sends them; the signaling service carries them.
	conf.DisableTrickleICE = false
	// As over LAN, a console may send no identity with its offer; the
	// offer can't require one (a Switch on LAN was turned away with "not
	// invited", error Door). Sign-in is still read from the login itself
	// (checkSignIn), and the game server a console picks checks it again.
	conf.AllowAnonymous = true
	for {
		var sig nethernet.Signaling
		select {
		case <-ctx.Done():
			return
		case sig = <-cfg.FriendsSignaling:
		}
		if sig == nil {
			continue
		}
		wrapped := &answerIdentity{Signaling: sig}
		l, err := listenCfg.ListenNetwork(minecraft.NetherNet{Signaling: wrapped, ListenConfig: conf}, sig.NetworkID())
		if err != nil {
			slog.Warn("Friends tab: could not listen on the Xbox account's connection", "error", err)
			continue
		}
		slog.Info("Friends tab listener ready: friends of the panel's Xbox account can join the list")
		go acceptLoop(cfg, l, "NetherNet Xbox")
		go func() {
			select {
			case <-sig.Context().Done():
			case <-ctx.Done():
			}
			_ = l.Close()
		}()
	}
}

// answerIdentity answers a console that sent no identity with its offer
// without one (go-nethernet always adds the list's own). A Switch given one
// it didn't ask for never took the answer (seen over LAN; see lan.go).
type answerIdentity struct {
	nethernet.Signaling
	signedOffers sync.Map
}

func (a *answerIdentity) Notify(n nethernet.Notifier) (stop func()) {
	return a.Signaling.Notify(offerWatcher{n, a})
}

func (a *answerIdentity) Signal(ctx context.Context, sig *nethernet.Signal) error {
	if sig.Type == nethernet.SignalTypeAnswer {
		if _, signed := a.signedOffers.LoadAndDelete(sig.ConnectionID); !signed {
			c := *sig
			c.Data = withoutIdentity(sig.Data)
			sig = &c
		}
	}
	return a.Signaling.Signal(ctx, sig)
}

type offerWatcher struct {
	nethernet.Notifier
	a *answerIdentity
}

func (w offerWatcher) NotifySignal(sig *nethernet.Signal) bool {
	if sig.Type == nethernet.SignalTypeOffer {
		if strings.Contains(sig.Data, "a=identity:") {
			w.a.signedOffers.Store(sig.ConnectionID, true)
		}
		slog.Debug("Friends tab: a console asked to join", "connectionID", sig.ConnectionID, "from", sig.NetworkID, "withIdentity", strings.Contains(sig.Data, "a=identity:"))
	}
	return w.Notifier.NotifySignal(sig)
}
