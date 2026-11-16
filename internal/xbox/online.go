package xbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ScottSchneck/block-party/internal/xbox/signaling"
	"github.com/df-mc/go-playfab/v2"
	"github.com/df-mc/go-xsapi/v2"
	"github.com/df-mc/go-xsapi/v2/mpsd"
	"github.com/df-mc/go-xsapi/v2/presence"
	"github.com/sandertv/gophertunnel/minecraft/auth"
	"github.com/sandertv/gophertunnel/minecraft/service"
	"golang.org/x/oauth2"
)

// liveConfig is the Microsoft sign-in the game itself uses on Android.
var liveConfig = auth.AndroidConfig

// online connects the account and keeps its session up until ctx is done or
// something breaks. It returns nil if the connection ended after working.
func (m *Manager) online(ctx context.Context, a *savedAccount, gen int) error {
	client := m.opts.HTTPClient
	ctx = auth.WithContextClient(ctx, client)
	m.update(gen, func(s *Status) { s.State, s.Error, s.RetryAt = StateStarting, "", time.Time{} })

	// Microsoft sign-in, refreshed as needed and saved when it changes.
	live := &savingTokenSource{
		src:  oauth2.ReuseTokenSource(a.Token, liveConfig.TokenSource(oauth2Context(context.Background(), client), a.Token)),
		last: a.Token, save: func(t *oauth2.Token) {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.gen != gen || m.saved == nil {
				return
			}
			m.saved.Token = t
			if err := m.save(m.saved); err != nil {
				m.log.Warn("couldn't save the refreshed Xbox sign-in", "error", err)
			}
		},
	}
	if _, err := live.Token(); err != nil {
		return err
	}

	// Xbox Live.
	xctx := auth.WithXBLTokenCache(ctx, liveConfig.NewTokenCache())
	xbl, err := xsapi.ClientConfig{HTTPClient: client, Logger: m.log}.New(xctx, auth.ContextSession(xctx, live))
	if err != nil {
		return signInError(fmt.Errorf("Xbox Live: %w", err))
	}
	defer xbl.Close()
	user := xbl.UserInfo()
	m.mu.Lock()
	if m.gen == gen && m.saved != nil && (m.saved.Gamertag != user.GamerTag || m.saved.XUID != user.XUID) {
		m.saved.Gamertag, m.saved.XUID = user.GamerTag, user.XUID
		_ = m.save(m.saved)
	}
	m.mu.Unlock()
	m.update(gen, func(s *Status) { s.Gamertag, s.XUID = user.GamerTag, user.XUID })

	// Minecraft's services (through PlayFab), then the signaling service.
	disc, err := service.Default(ctx)
	if err != nil {
		return fmt.Errorf("Minecraft services: %w", err)
	}
	env := new(service.AuthorizationEnvironment)
	if err := disc.Environment(env); err != nil {
		return fmt.Errorf("Minecraft services: %w", err)
	}
	env.HTTPClient = client
	pf, err := playfab.LoginWithXbox(ctx, env.PlayFabTitleID, xbl, playfab.ClientConfig{HTTPClient: client, Logger: m.log, CreateAccount: true})
	if err != nil {
		return fmt.Errorf("PlayFab sign-in: %w", err)
	}
	defer pf.Close()
	mc := env.TokenSource(pf, service.TokenConfig{})
	sig, err := signaling.Dialer{HTTPClient: client, Log: m.log}.DialContext(ctx, mc)
	if err != nil {
		return fmt.Errorf("Minecraft signaling service: %w", err)
	}
	defer sig.Close()
	if !validNetworkID(sig.NetworkID()) {
		return fmt.Errorf("signaling service: unusable network ID %q", sig.NetworkID())
	}

	// Hand the connection to the server list, replacing an older one.
	select {
	case <-m.links:
	default:
	}
	m.links <- Link{Signaling: sig}

	// The session friends see.
	info := sessionInfo{
		XUID: user.XUID, NetworkID: sig.NetworkID(), PmsgID: sig.PlayerMessagingID(),
		LevelID: newLevelID(), World: m.opts.World(), Nonces: map[string]string{},
	}
	props, err := info.properties()
	if err != nil {
		return err
	}
	session, err := xbl.MPSD().Publish(ctx, mpsd.SessionReference{ServiceConfigID: auth.ServiceConfigID, TemplateName: templateName},
		mpsd.PublishConfig{CustomProperties: props, JoinRestriction: mpsd.SessionRestrictionFollowed, ReadRestriction: mpsd.SessionRestrictionFollowed})
	if err != nil {
		return fmt.Errorf("publish the session: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = session.CloseContext(closeCtx)
	}()
	changed := make(chan struct{}, 1)
	session.Handle(sessionChanges{changed})

	started := time.Now()
	m.update(gen, func(s *Status) { s.State, s.Error, s.RetryAt = StateOnline, "", time.Time{} })
	m.log.Info("friends' consoles can join the server list from the Friends tab", "gamertag", user.GamerTag, "networkID", sig.NetworkID())

	var wg sync.WaitGroup
	defer wg.Wait()
	loopCtx, stop := context.WithCancel(ctx)
	defer stop()
	wg.Add(2)
	go func() { defer wg.Done(); m.keepPresence(loopCtx, xbl.Presence()) }()
	go func() { defer wg.Done(); m.acceptFriends(loopCtx, xbl, gen) }()

	refresh := time.NewTicker(30 * time.Second)
	defer refresh.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sig.Context().Done():
			if time.Since(started) > 2*time.Minute {
				m.log.Info("signaling connection ended; reconnecting", "cause", context.Cause(sig.Context()))
				return nil
			}
			return fmt.Errorf("signaling connection closed: %w", context.Cause(sig.Context()))
		case <-session.Context().Done():
			return errors.New("Xbox Live closed the session")
		case <-changed:
		case <-refresh.C:
		}
		var members []string
		for _, mem := range session.Members() {
			if mem.Constants != nil && mem.Constants.System != nil {
				members = append(members, mem.Constants.System.XUID)
			}
		}
		syncNonces(info.Nonces, members, user.XUID)
		info.Members = len(members)
		info.World = m.opts.World()
		next, err := info.properties()
		if err != nil {
			return err
		}
		m.update(gen, func(s *Status) { s.Members = max(len(members)-1, 0) })
		if bytes.Equal(next, props) {
			continue
		}
		upd, cancel := context.WithTimeout(ctx, 15*time.Second)
		err = session.SetCustomProperties(upd, next)
		cancel()
		if err != nil {
			m.log.Warn("couldn't update the Xbox session", "error", err)
			continue
		}
		props = next
	}
}

// keepPresence keeps the account showing as playing Minecraft, which friends
// need to see its world.
func (m *Manager) keepPresence(ctx context.Context, p *presence.Client) {
	for {
		next := 5 * time.Minute
		upd, cancel := context.WithTimeout(ctx, 15*time.Second)
		res, err := p.Update(upd, presence.TitleRequest{State: presence.StateActive})
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.log.Warn("couldn't update Xbox presence", "error", err)
			next = time.Minute
		} else if res.HeartbeatAfter > 0 {
			next = res.HeartbeatAfter
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(next):
		}
	}
}

// acceptFriends accepts friend requests (from Minecraft 26.30, adding the
// account as a friend sends one) and counts friends.
func (m *Manager) acceptFriends(ctx context.Context, xbl *xsapi.Client, gen int) {
	social := xbl.Social()
	for {
		call, cancel := context.WithTimeout(ctx, 20*time.Second)
		pending, err := social.IncomingFriendRequests(call)
		cancel()
		if err != nil && ctx.Err() == nil {
			m.log.Warn("couldn't read Xbox friend requests", "error", err)
		}
		var xuids, names []string
		for _, u := range pending {
			if u.XUID != "" {
				xuids = append(xuids, u.XUID)
				names = append(names, u.DisplayName)
			}
		}
		left := len(xuids)
		if len(xuids) > 0 {
			call, cancel := context.WithTimeout(ctx, 20*time.Second)
			_, err := social.AddFriends(call, xuids)
			cancel()
			if err != nil {
				m.log.Warn("couldn't accept Xbox friend requests", "error", err, "from", strings.Join(names, ", "))
			} else {
				left = 0
				m.log.Info("accepted Xbox friend requests; they can join the server list from the Friends tab", "from", strings.Join(names, ", "))
			}
		}
		call, cancel = context.WithTimeout(ctx, 20*time.Second)
		friends, err := social.Friends(call)
		cancel()
		if err == nil {
			m.update(gen, func(s *Status) { s.Friends, s.Pending = len(friends), left })
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

// sessionChanges passes on changes to the session (members joining and
// leaving).
type sessionChanges struct{ ch chan struct{} }

func (s sessionChanges) HandleSessionChange(*mpsd.Session) {
	select {
	case s.ch <- struct{}{}:
	default:
	}
}

// savingTokenSource saves the Microsoft sign-in each time it's refreshed.
type savingTokenSource struct {
	src  oauth2.TokenSource
	mu   sync.Mutex
	last *oauth2.Token
	save func(*oauth2.Token)
}

func (s *savingTokenSource) Token() (*oauth2.Token, error) {
	t, err := s.src.Token()
	if err != nil {
		return nil, signInError(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil || t.AccessToken != s.last.AccessToken || t.RefreshToken != s.last.RefreshToken {
		s.last = t
		s.save(t)
	}
	return t, nil
}

// signInError marks errors that mean the saved sign-in is no longer
// accepted, so the owner is asked to sign in again instead of retrying.
func signInError(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) && (re.ErrorCode == "invalid_grant" || re.ErrorCode == "interaction_required") {
		return fmt.Errorf("%w: %v", errSignInAgain, err)
	}
	return err
}
