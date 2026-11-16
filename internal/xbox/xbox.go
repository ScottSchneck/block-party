// Package xbox lets consoles outside the house join the server list from the
// Friends tab. The panel signs in as an Xbox account (a spare one, not a
// player's), shows the list to that account's friends as a joinable world,
// keeps the account online and accepts friend requests. A friend who joins
// it connects through Minecraft's signaling service straight to the list's
// NetherNet listener, as they would to a friend's world, with Microsoft's
// relay servers when a direct path doesn't work.
//
// This is the route BedrockConnect and MCXboxBroadcast use. It replaces DNS
// for consoles outside: from Minecraft 26.62, consoles refuse the list's own
// certificate for featured-server names, so that route is closed.
package xbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/df-mc/go-nethernet"
	"golang.org/x/oauth2"
)

// Options configures a Manager.
type Options struct {
	// DataDir is where the sign-in is kept (xbox-account.json).
	DataDir string
	// HTTPClient is used for every request. If nil, http.DefaultClient.
	HTTPClient *http.Client
	// World is what friends see for the list, asked for each update.
	World func() World
	// Log is the logger. If nil, slog.Default().
	Log *slog.Logger
}

// State is where the account is.
type State string

const (
	StateSignedOut State = "signed-out" // no account
	StateCode      State = "code"       // waiting for the owner to enter the code
	StateStarting  State = "starting"   // signed in, connecting
	StateOnline    State = "online"     // friends can see and join the list
	StateRetrying  State = "retrying"   // connecting failed; trying again soon
)

// Status is what the panel shows about the account.
type Status struct {
	State State `json:"state"`
	// Gamertag and XUID are the signed-in account's.
	Gamertag string `json:"gamertag,omitempty"`
	XUID     string `json:"xuid,omitempty"`
	// Code and URL are where the owner signs in, while State is StateCode.
	Code        string    `json:"code,omitempty"`
	URL         string    `json:"url,omitempty"`
	CodeExpires time.Time `json:"codeExpires,omitzero"`
	// Error is the last thing that went wrong, if anything.
	Error string `json:"error,omitempty"`
	// Friends is how many friends the account has; Pending, how many
	// friend requests are waiting (accepted on the next pass).
	Friends int `json:"friends"`
	Pending int `json:"pending"`
	// Members is how many people are in the list's session right now.
	Members int `json:"members"`
	// RetryAt is when connecting is tried again, while State is StateRetrying.
	RetryAt time.Time `json:"retryAt,omitzero"`
	// SignupURL is where to finish making the account's Xbox profile, when
	// the Microsoft account doesn't have one yet.
	SignupURL string `json:"signupURL,omitempty"`
}

// Link is the account's connection to the signaling service, for the server
// list to listen on. It's done when Signaling.Context() is.
type Link struct {
	Signaling nethernet.Signaling
}

// Manager runs the account.
type Manager struct {
	opts  Options
	log   *slog.Logger
	links chan Link

	mu       sync.Mutex
	st       Status
	saved    *savedAccount
	wake     chan struct{}      // something changed: sign-in finished, sign-out
	stopCode context.CancelFunc // cancels a sign-in waiting for its code
	gen      int                // bumped on sign-out, so an old connection doesn't report in
	// beforeCode is the status from before a sign-in code was shown, put
	// back if that sign-in is cancelled or turned down; latest keeps the
	// running account's updates meanwhile.
	beforeCode Status
}

// savedAccount is what's kept on disk.
type savedAccount struct {
	Token    *oauth2.Token `json:"token"`
	Gamertag string        `json:"gamertag,omitempty"`
	XUID     string        `json:"xuid,omitempty"`
}

// New returns a Manager with the saved sign-in, if any.
func New(opts Options) *Manager {
	if opts.HTTPClient == nil {
		opts.HTTPClient = http.DefaultClient
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.World == nil {
		opts.World = func() World { return World{} }
	}
	m := &Manager{
		opts:  opts,
		log:   opts.Log.With("src", "xbox"),
		links: make(chan Link, 1),
		wake:  make(chan struct{}, 1),
		st:    Status{State: StateSignedOut},
	}
	if a, err := m.load(); err != nil {
		m.log.Warn("couldn't read the saved Xbox sign-in; sign in again", "error", err)
	} else if a != nil && a.Token != nil {
		m.saved = a
		m.st = Status{State: StateStarting, Gamertag: a.Gamertag, XUID: a.XUID}
	}
	return m
}

// Links delivers each new connection to the signaling service.
func (m *Manager) Links() <-chan Link { return m.links }

// Status returns where the account is.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st
}

func (m *Manager) path() string { return filepath.Join(m.opts.DataDir, "xbox-account.json") }

func (m *Manager) load() (*savedAccount, error) {
	b, err := os.ReadFile(m.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var a savedAccount
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (m *Manager) save(a *savedAccount) error {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path())
}

func (m *Manager) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// SignIn starts signing in: it asks Microsoft for a code, which the owner
// enters at the returned URL. Signing in finishes on its own once they do.
func (m *Manager) SignIn(ctx context.Context) (Status, error) {
	m.mu.Lock()
	if m.st.State == StateCode && time.Now().Before(m.st.CodeExpires) {
		st := m.st
		m.mu.Unlock()
		return st, nil
	}
	m.mu.Unlock()

	d, err := liveConfig.DeviceAuth(oauth2Context(ctx, m.opts.HTTPClient))
	if err != nil {
		return m.Status(), fmt.Errorf("couldn't get a sign-in code from Microsoft: %w", err)
	}
	expires := d.Expiry
	if expires.IsZero() {
		expires = time.Now().Add(15 * time.Minute)
	}
	waitCtx, cancel := context.WithDeadline(context.Background(), expires)

	m.mu.Lock()
	if m.stopCode != nil {
		m.stopCode()
	}
	m.stopCode = cancel
	if m.st.State != StateCode {
		// Signing in again while signed in keeps the current account
		// running until the new one is in.
		m.beforeCode = m.st
		m.beforeCode.Code, m.beforeCode.URL, m.beforeCode.CodeExpires = "", "", time.Time{}
	}
	m.st = Status{State: StateCode, Code: d.UserCode, URL: d.VerificationURI, CodeExpires: expires,
		Gamertag: m.beforeCode.Gamertag, XUID: m.beforeCode.XUID}
	st := m.st
	m.mu.Unlock()

	go func() {
		defer cancel()
		tok, err := liveConfig.DeviceAccessToken(oauth2Context(waitCtx, m.opts.HTTPClient), d)
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.st.State != StateCode || m.st.Code != d.UserCode {
			return // cancelled, or a newer code
		}
		m.stopCode = nil
		if err != nil {
			m.log.Info("Xbox sign-in wasn't finished", "error", err)
			m.st = m.beforeCode
			if m.saved == nil {
				m.st = Status{State: StateSignedOut, Error: "The sign-in code ran out or was turned down. Try again."}
			}
			return
		}
		a := &savedAccount{Token: tok}
		if err := m.save(a); err != nil {
			m.log.Error("couldn't save the Xbox sign-in", "error", err)
			m.st = Status{State: StateSignedOut, Error: "Signed in, but the panel couldn't save it: " + err.Error()}
			return
		}
		m.saved = a
		m.gen++
		m.st = Status{State: StateStarting}
		m.log.Info("signed in to Xbox for friends' consoles")
		m.poke()
	}()
	return st, nil
}

// CancelSignIn stops waiting for a sign-in code.
func (m *Manager) CancelSignIn() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.st.State != StateCode {
		return m.st
	}
	if m.stopCode != nil {
		m.stopCode()
		m.stopCode = nil
	}
	m.st = m.beforeCode
	if m.saved == nil {
		m.st = Status{State: StateSignedOut}
	}
	return m.st
}

// RetryNow tries connecting again right away, while waiting to retry.
func (m *Manager) RetryNow() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.st.State == StateRetrying {
		m.st.State, m.st.RetryAt = StateStarting, time.Time{}
		m.poke()
	}
	return m.st
}

// SignOut forgets the account and takes the list off its friends' list.
func (m *Manager) SignOut() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopCode != nil {
		m.stopCode()
		m.stopCode = nil
	}
	m.saved = nil
	m.gen++
	m.st = Status{State: StateSignedOut}
	m.poke()
	if err := os.Remove(m.path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	m.log.Info("signed out of Xbox; consoles outside can't join from the Friends tab")
	return nil
}

// Run keeps the account online while it's signed in, until ctx is done.
func (m *Manager) Run(ctx context.Context) {
	delay := 30 * time.Second
	for ctx.Err() == nil {
		m.mu.Lock()
		a, gen := m.saved, m.gen
		m.mu.Unlock()
		if a == nil {
			select {
			case <-ctx.Done():
				return
			case <-m.wake:
			}
			continue
		}

		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- m.online(runCtx, a, gen) }()
		var err error
		select {
		case err = <-done:
		case <-m.wake:
			// Signed out, or in again as someone else: start over.
			cancel()
			<-done
			cancel()
			delay = 30 * time.Second
			continue
		}
		cancel()
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errSignInAgain) {
			m.mu.Lock()
			if m.gen == gen {
				m.saved = nil
				_ = os.Remove(m.path())
				out := Status{State: StateSignedOut, Error: "Microsoft no longer accepts this sign-in. Sign in again."}
				if m.st.State == StateCode {
					m.beforeCode = out
				} else {
					m.st = out
				}
			}
			m.mu.Unlock()
			continue
		}
		if err == nil {
			// The connection ended after working for a while: back online
			// soon.
			delay = 30 * time.Second
		}
		m.update(gen, func(s *Status) {
			s.State, s.RetryAt = StateRetrying, time.Now().Add(delay)
			if err != nil {
				s.Error = err.Error()
			}
		})
		if err != nil {
			m.log.Warn("Xbox account for friends' consoles couldn't stay online; trying again", "error", err, "in", delay)
		}
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-time.After(delay):
		}
		delay = min(delay*2, 10*time.Minute)
	}
}

// update changes the status, unless the account it's about was signed out.
// While a sign-in code is showing, it changes the status to go back to.
func (m *Manager) update(gen int, f func(*Status)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gen != gen {
		return
	}
	if m.st.State == StateCode {
		f(&m.beforeCode)
		return
	}
	f(&m.st)
}

// errSignInAgain means the saved sign-in no longer works.
var errSignInAgain = errors.New("sign in again")

// oauth2Context makes the oauth2 package use client.
func oauth2Context(ctx context.Context, client *http.Client) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, client)
}
