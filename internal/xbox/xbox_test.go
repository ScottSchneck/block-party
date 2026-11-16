package xbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	xsocial "github.com/df-mc/go-xsapi/v2/social"
	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"golang.org/x/oauth2"
)

// The session's custom properties carry what the game needs to show the
// world and join it, with the field names and types it reads.
func TestSessionProperties(t *testing.T) {
	pmsg := uuid.MustParse("6f1e2d3c-4b5a-4978-8a9b-0c1d2e3f4a5b")
	b, err := sessionInfo{
		XUID: "2535400000000001", NetworkID: "12345678901234567890", PmsgID: pmsg, LevelID: "AAAAAAAAAAA=",
		World:   World{Name: "Smith Family Servers", Subtitle: "Pick a server", MaxPlayers: 30},
		Members: 3, Nonces: map[string]string{"2535400000000002": "0011223344556677"},
	}.properties()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"Joinability": "joinable_by_friends", "worldName": "Smith Family Servers", "hostName": "Pick a server",
		"ownerId": "2535400000000001", "version": protocol.CurrentVersion, "protocol": float64(protocol.CurrentProtocol),
		"BroadcastSetting": float64(2), "TransportLayer": float64(2), "LanGame": false, "OnlineCrossPlatformGame": true,
		"MemberCount": float64(3), "MaxMemberCount": float64(30), "worldType": "Survival", "rakNetGUID": "",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %#v, want %#v", k, got[k], v)
		}
	}
	conns, _ := got["SupportedConnections"].([]any)
	if len(conns) != 1 {
		t.Fatalf("SupportedConnections = %v", got["SupportedConnections"])
	}
	c := conns[0].(map[string]any)
	if c["ConnectionType"] != float64(7) || c["PmsgId"] != pmsg.String() {
		t.Errorf("connection = %v", c)
	}
	// The NetherNet ID is a JSON number, not a string.
	if !strings.Contains(string(b), `"NetherNetId":12345678901234567890`) {
		t.Errorf("NetherNetId not a number: %s", b)
	}
	if n, _ := got["nonces"].(map[string]any); n["2535400000000002"] != "0011223344556677" {
		t.Errorf("nonces = %v", got["nonces"])
	}
}

// With nothing set, the world still has a name, a member, room for one more,
// and an empty (not null) nonce map.
func TestSessionPropertiesDefaults(t *testing.T) {
	b, err := sessionInfo{XUID: "1", NetworkID: "42"}.properties()
	if err != nil {
		t.Fatal(err)
	}
	var got sessionProperties
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.WorldName != "Server List" || got.HostName != "Server List" || got.MemberCount != 1 || got.MaxMemberCount != 2 || got.Nonces == nil {
		t.Errorf("defaults: %+v", got)
	}
	if !strings.Contains(string(b), `"nonces":{}`) {
		t.Errorf("nonces should be an empty object: %s", b)
	}
}

func TestSyncNonces(t *testing.T) {
	n := map[string]string{"gone": "aa"}
	if !syncNonces(n, []string{"owner", "a", " b ", ""}, "owner") {
		t.Fatal("expected a change")
	}
	if _, ok := n["gone"]; ok || len(n) != 2 || len(n["a"]) != 16 || len(n["b"]) != 16 || n["a"] == n["b"] {
		t.Fatalf("nonces = %v", n)
	}
	a := n["a"]
	if syncNonces(n, []string{"a", "b", "owner"}, "owner") || n["a"] != a {
		t.Error("an unchanged session changed the nonces")
	}
}

func TestValidNetworkID(t *testing.T) {
	for id, want := range map[string]bool{"42": true, "18446744073709551615": true, "0": false, "": false, "042": false, "abc": false, "-1": false} {
		if validNetworkID(id) != want {
			t.Errorf("validNetworkID(%q) != %v", id, want)
		}
	}
}

// A saved sign-in comes back on restart; signing out forgets it.
func TestSavedAccount(t *testing.T) {
	dir := t.TempDir()
	m := New(Options{DataDir: dir})
	if st := m.Status(); st.State != StateSignedOut {
		t.Fatalf("new panel: %+v", st)
	}
	if err := m.save(&savedAccount{Token: &oauth2.Token{AccessToken: "a", RefreshToken: "r"}, Gamertag: "FamilyList", XUID: "1"}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "xbox-account.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("saved file: %v %v", fi, err)
	}
	m = New(Options{DataDir: dir})
	if st := m.Status(); st.State != StateStarting || st.Gamertag != "FamilyList" {
		t.Fatalf("after restart: %+v", st)
	}
	if err := m.SignOut(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "xbox-account.json")); !os.IsNotExist(err) {
		t.Errorf("sign-in still saved: %v", err)
	}
	if st := m.Status(); st.State != StateSignedOut {
		t.Errorf("after sign-out: %+v", st)
	}
}

// fakeMicrosoft answers the device-code sign-in like login.live.com.
type fakeMicrosoft struct {
	polls    atomic.Int32
	approve  int32 // polls before the code is entered
	deny     bool
	refreshT string
}

func (f *fakeMicrosoft) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "login.live.com" {
		return nil, fmt.Errorf("unexpected request to %s", r.URL)
	}
	body, _ := io.ReadAll(r.Body)
	reply := func(status int, v any) (*http.Response, error) {
		b, _ := json.Marshal(v)
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(b))), Request: r}, nil
	}
	switch r.URL.Path {
	case "/oauth20_connect.srf":
		return reply(200, map[string]any{"device_code": "dev", "user_code": "ABCD1234", "verification_uri": "https://www.microsoft.com/link", "expires_in": 900, "interval": 1})
	case "/oauth20_token.srf":
		if !strings.Contains(string(body), "device_code=dev") {
			return reply(400, map[string]any{"error": "invalid_request"})
		}
		if f.deny {
			return reply(400, map[string]any{"error": "access_denied"})
		}
		if f.polls.Add(1) <= f.approve {
			return reply(400, map[string]any{"error": "authorization_pending"})
		}
		return reply(200, map[string]any{"access_token": "live-access", "refresh_token": "live-refresh", "token_type": "bearer", "expires_in": 3600})
	}
	return reply(404, map[string]any{})
}

// Signing in shows a code, then saves the sign-in once it's entered and
// wakes the account up.
func TestSignInWithCode(t *testing.T) {
	dir := t.TempDir()
	m := New(Options{DataDir: dir, HTTPClient: &http.Client{Transport: &fakeMicrosoft{approve: 1}}})
	st, err := m.SignIn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateCode || st.Code != "ABCD1234" || st.URL != "https://www.microsoft.com/link" || time.Until(st.CodeExpires) < 10*time.Minute {
		t.Fatalf("code: %+v", st)
	}
	// Asking again while the code works gives the same code.
	if again, _ := m.SignIn(context.Background()); again.Code != "ABCD1234" {
		t.Errorf("second sign-in: %+v", again)
	}
	deadline := time.Now().Add(15 * time.Second)
	for m.Status().State == StateCode && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if st := m.Status(); st.State != StateStarting {
		t.Fatalf("after the code was entered: %+v", st)
	}
	a, err := m.load()
	if err != nil || a == nil || a.Token.RefreshToken != "live-refresh" {
		t.Fatalf("saved: %+v %v", a, err)
	}
	select {
	case <-m.wake:
	default:
		t.Error("the account wasn't woken to connect")
	}
}

// A code that's turned down leaves the panel signed out with a reason;
// cancelling goes back to signed out.
func TestSignInDeniedAndCancel(t *testing.T) {
	m := New(Options{DataDir: t.TempDir(), HTTPClient: &http.Client{Transport: &fakeMicrosoft{deny: true}}})
	if _, err := m.SignIn(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for m.Status().State == StateCode && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if st := m.Status(); st.State != StateSignedOut || st.Error == "" {
		t.Fatalf("denied: %+v", st)
	}

	m = New(Options{DataDir: t.TempDir(), HTTPClient: &http.Client{Transport: &fakeMicrosoft{approve: 1000}}})
	if _, err := m.SignIn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := m.CancelSignIn(); st.State != StateSignedOut {
		t.Fatalf("cancelled: %+v", st)
	}
	time.Sleep(1500 * time.Millisecond)
	if st := m.Status(); st.State != StateSignedOut {
		t.Errorf("a cancelled sign-in came back: %+v", st)
	}
}

func TestSignInError(t *testing.T) {
	if !errors.Is(signInError(&oauth2.RetrieveError{ErrorCode: "invalid_grant"}), errSignInAgain) {
		t.Error("invalid_grant should ask to sign in again")
	}
	if errors.Is(signInError(errors.New("network down")), errSignInAgain) {
		t.Error("a network error shouldn't ask to sign in again")
	}
}

// With no account, Run waits quietly until it's stopped.
func TestRunSignedOut(t *testing.T) {
	m := New(Options{DataDir: t.TempDir()})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run didn't stop")
	}
}

// Cancelling a sign-in while an account is online puts back where it was,
// and the running account's updates meanwhile aren't lost or shown over the
// code.
func TestSignInWhileOnlineKeepsTheAccount(t *testing.T) {
	m := New(Options{DataDir: t.TempDir(), HTTPClient: &http.Client{Transport: &fakeMicrosoft{approve: 1000}}})
	m.saved = &savedAccount{Token: &oauth2.Token{AccessToken: "a"}, Gamertag: "FamilyList"}
	m.st = Status{State: StateOnline, Gamertag: "FamilyList", Friends: 3}
	st, err := m.SignIn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateCode || st.Gamertag != "FamilyList" {
		t.Fatalf("code: %+v", st)
	}
	m.update(m.gen, func(s *Status) { s.Friends = 4 })
	if st := m.Status(); st.State != StateCode || st.Friends != 0 {
		t.Fatalf("an update replaced the code: %+v", st)
	}
	if st := m.CancelSignIn(); st.State != StateOnline || st.Friends != 4 || st.Gamertag != "FamilyList" {
		t.Fatalf("after cancelling: %+v", st)
	}
}

// Try now wakes a waiting retry, and does nothing otherwise.
func TestRetryNow(t *testing.T) {
	m := New(Options{DataDir: t.TempDir()})
	if st := m.RetryNow(); st.State != StateSignedOut {
		t.Fatalf("signed out: %+v", st)
	}
	select {
	case <-m.wake:
		t.Fatal("woke while signed out")
	default:
	}
	m.st = Status{State: StateRetrying, RetryAt: time.Now().Add(time.Minute), SignupURL: "https://www.xbox.com"}
	if st := m.RetryNow(); st.State != StateStarting || !st.RetryAt.IsZero() {
		t.Fatalf("retrying: %+v", st)
	}
	select {
	case <-m.wake:
	default:
		t.Fatal("not woken")
	}
}

func TestRetryAfter(t *testing.T) {
	if d := retryAfter(fmt.Errorf("wrapped: %w", &xsocial.ResponseError{StatusCode: 429, RetryAfter: 5 * time.Minute})); d < 5*time.Minute {
		t.Errorf("retryAfter = %s", d)
	}
	if retryAfter(errors.New("other")) != 0 {
		t.Error("an error without Retry-After should give 0")
	}
}

// A notice of more friend requests nudges the loop once; fewer don't.
func TestFriendRequestNotices(t *testing.T) {
	ch := make(chan struct{}, 1)
	n := friendRequestNotices{ch}
	n.HandleIncomingFriendRequestCountChange(0)
	select {
	case <-ch:
		t.Fatal("nudged for no requests")
	default:
	}
	n.HandleIncomingFriendRequestCountChange(1)
	n.HandleIncomingFriendRequestCountChange(2)
	if len(ch) != 1 {
		t.Fatalf("nudges queued: %d", len(ch))
	}
}
