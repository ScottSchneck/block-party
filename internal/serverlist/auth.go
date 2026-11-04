package serverlist

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
	"github.com/sandertv/gophertunnel/minecraft/service"
)

// Sign-in checks.
//
// The server list never turns a console away because its sign-in can't be
// checked. It only shows a menu and hands the console on; the game server it
// picks does its own sign-in and allowlist checks. Turning consoles away here
// only breaks joining when Minecraft changes its sign-in format, which has
// happened twice: in September 2026 PlayStation's token was signed with ES384,
// which gophertunnel then rejected outright, and from 26.60 consoles joining
// over NetherNet sign their Login themselves (identity.go covers those).
//
// So the listener runs with gophertunnel's checks off, and this file checks
// the sign-in itself and reports the result in the log (and, later, to the
// panel). REQUIRE_SIGN_IN=true turns away consoles that can't be verified.

// signIn is the result of checking a console's sign-in.
type signIn struct {
	Verified bool
	How      string // what verified it, or why it couldn't be
	XUID     string
	Name     string
	Token    []any // details of an unverified token, for the log
}

func (s signIn) String() string {
	if s.Verified {
		return "verified (" + s.How + ")"
	}
	return "not verified (" + s.How + ")"
}

// authVerifier fetches Microsoft's sign-in keys in the background and keeps
// the verifier. A join never waits for the fetch: until it succeeds, sign-ins
// are reported as "sign-in service unreachable". A failed fetch is retried at
// most once a minute, when a console joins.
type authVerifier struct {
	mu       sync.Mutex
	verifier *oidc.IDTokenVerifier
	fetching bool
	lastTry  time.Time
}

var sharedVerifier authVerifier

// get returns the verifier, or nil if it isn't available yet.
func (a *authVerifier) get() *oidc.IDTokenVerifier {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.verifier == nil && !a.fetching && time.Since(a.lastTry) >= time.Minute {
		a.fetching, a.lastTry = true, time.Now()
		go a.fetch()
	}
	return a.verifier
}

func (a *authVerifier) fetch() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	v, err := fetchVerifier(ctx)
	if err != nil {
		slog.Warn("could not reach Minecraft's sign-in service; sign-ins can't be verified until it's reachable", "error", err)
	} else {
		slog.Info("sign-in service ready; Xbox sign-ins will be verified")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fetching = false
	if err == nil {
		a.verifier = v
	}
}

func fetchVerifier(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	d, err := service.Discover(ctx, service.ApplicationTypeMinecraftPE, protocol.CurrentVersion)
	if err != nil {
		return nil, fmt.Errorf("discover sign-in service: %w", err)
	}
	env := new(service.AuthorizationEnvironment)
	if err := d.Environment(env); err != nil {
		return nil, fmt.Errorf("read sign-in service details: %w", err)
	}
	return env.VerifierContext(ctx)
}

// checkSignIn checks the Login packet payload a console sent. netherNet says
// the console came over NetherNet, whose connection proved it holds the
// Login's key (see identity.go).
func checkSignIn(loginPayload []byte, verifier *oidc.IDTokenVerifier, netherNet bool) signIn {
	req, err := connectionRequest(loginPayload)
	if err != nil {
		return signIn{How: "login packet unreadable: " + err.Error()}
	}

	var tokenErr error
	if verifier != nil {
		id, _, res, err := login.Parse(req, verifier)
		if err == nil && res.XBOXLiveAuthenticated {
			return signIn{Verified: true, How: "Microsoft sign-in token", XUID: id.XUID, Name: id.DisplayName}
		}
		tokenErr = err
	}

	// Try the older certificate chain signed by Mojang, which clients still
	// send alongside the token.
	if chainOnly, err := withoutToken(req); err == nil {
		if id, _, res, err := login.Parse(chainOnly, nil); err == nil && res.XBOXLiveAuthenticated {
			return signIn{Verified: true, How: "Mojang certificate chain", XUID: id.XUID, Name: id.DisplayName}
		}
	}

	// From 26.60 consoles joining over NetherNet sign their Login
	// themselves; their connection offer carried the Microsoft-issued token.
	if netherNet {
		if id, ok := offerIdentity(loginKey(req)); ok {
			return signIn{Verified: true, How: "Microsoft-issued NetherNet identity", XUID: id.XUID, Name: id.Name}
		}
	}

	s := signIn{How: "no verifiable sign-in"}
	switch {
	case verifier == nil:
		s.How = "sign-in service unreachable"
	case tokenErr != nil:
		s.How = shortError(tokenErr)
	}
	s.Token = describeToken(req)
	return s
}

// connectionRequest pulls the connection request out of a Login packet
// payload: a big-endian protocol number, then a length-prefixed byte string.
func connectionRequest(payload []byte) ([]byte, error) {
	if len(payload) < 4 {
		return nil, errors.New("too short")
	}
	r := bytes.NewReader(payload[4:])
	n, err := binary.ReadUvarint(r)
	if err != nil || n > uint64(r.Len()) {
		return nil, errors.New("bad length")
	}
	out := make([]byte, n)
	_, _ = r.Read(out)
	return out, nil
}

// splitRequest splits a connection request into its sign-in JSON and the
// client data that follows it. Both are prefixed with a little-endian length.
func splitRequest(req []byte) (authJSON, rest []byte, err error) {
	if len(req) < 4 {
		return nil, nil, errors.New("too short")
	}
	n := int(int32(binary.LittleEndian.Uint32(req)))
	if n <= 0 || 4+n > len(req) {
		return nil, nil, errors.New("bad sign-in length")
	}
	return req[4 : 4+n], req[4+n:], nil
}

// withoutToken returns the connection request with its sign-in token
// removed, so only the certificate chain is checked.
func withoutToken(req []byte) ([]byte, error) {
	authJSON, rest, err := splitRequest(req)
	if err != nil {
		return nil, err
	}
	var auth map[string]json.RawMessage
	if err := json.Unmarshal(authJSON, &auth); err != nil {
		return nil, err
	}
	if _, ok := auth["Token"]; !ok {
		return nil, errors.New("no token")
	}
	delete(auth, "Token")
	b, err := json.Marshal(auth)
	if err != nil {
		return nil, err
	}
	out := binary.LittleEndian.AppendUint32(nil, uint32(len(b)))
	out = append(out, b...)
	return append(out, rest...), nil
}

// Claims worth logging from an unverified token. Keys and signatures are
// never logged.
var loggedClaims = []string{"iss", "aud", "ipt", "tid", "plat", "atyp", "dtyp", "xname", "exp"}

// describeToken summarises an unverified sign-in token for the log: its
// header (algorithm, key ID) and a few claims, so a format change can be
// understood from a user's log.
func describeToken(req []byte) []any {
	authJSON, _, err := splitRequest(req)
	if err != nil {
		return nil
	}
	var auth struct {
		AuthenticationType int    `json:"AuthenticationType"`
		Token              string `json:"Token"`
		Certificate        string `json:"Certificate"`
	}
	if err := json.Unmarshal(authJSON, &auth); err != nil {
		return nil
	}
	out := []any{"authType", auth.AuthenticationType, "hasChain", auth.Certificate != ""}
	parts := strings.Split(auth.Token, ".")
	if len(parts) != 3 {
		return append(out, "token", "none")
	}
	var header, claims map[string]any
	if b, err := base64.RawURLEncoding.DecodeString(parts[0]); err == nil {
		_ = json.Unmarshal(b, &header)
	}
	if b, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
		_ = json.Unmarshal(b, &claims)
	}
	for _, k := range []string{"alg", "kid", "typ", "jku"} {
		if v, ok := header[k]; ok {
			out = append(out, "token."+k, value(v))
		}
	}
	// A token signed with the console's own key (x5u matches the client key
	// claim) was made by the console itself, not by Microsoft.
	x5u, _ := header["x5u"].(string)
	cpk, _ := claims["cpk"].(string)
	out = append(out, "token.selfSigned", x5u != "" && x5u == cpk)
	if _, ok := header["x5c"]; ok {
		out = append(out, "token.x5c", "present")
	}
	if _, ok := header["jwk"]; ok {
		out = append(out, "token.jwk", "present")
	}
	for _, k := range loggedClaims {
		if v, ok := claims[k]; ok {
			out = append(out, "claim."+k, value(v))
		}
	}
	_, hasXUID := claims["xid"]
	out = append(out, "claim.xid", hasXUID)
	names := make([]string, 0, len(claims))
	for k := range claims {
		names = append(names, k)
	}
	sort.Strings(names)
	return append(out, "claimNames", strings.Join(names, ","))
}

func value(v any) string {
	if f, ok := v.(float64); ok {
		return fmt.Sprintf("%.0f", f)
	}
	return clip(fmt.Sprint(v))
}

func clip(s string) string {
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

func shortError(err error) string {
	s := err.Error()
	s = strings.TrimPrefix(s, "verify ID token: ")
	return clip(s)
}
