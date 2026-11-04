package serverlist

import (
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
)

// NetherNet identities.
//
// From Minecraft 26.60 a console joining over NetherNet by address (LAN
// Games, Add Server, the featured-server redirect) sends a Login whose
// sign-in token it signed itself, which proves nothing. But its NetherNet
// connection offer carries an identity token issued by Minecraft's sign-in
// service, with the console's key ("cpk"), gamertag and Xbox ID, and the
// offer is signed with that key. gophertunnel then requires the Login to use
// the same key the connection proved it holds.
//
// So the list checks the offer's token against Microsoft's keys and
// remembers who each key belongs to; checkSignIn trusts a self-signed Login
// whose key matches a verified offer. That's only sound over NetherNet: a
// RakNet connection proves no key, so checkSignIn never does it there.

// netIdentity is what an offer's identity token said.
type netIdentity struct {
	Verified bool
	How      string // why it isn't verified
	XUID     string
	Name     string
	At       time.Time
}

// identityTTL is how long an offer's identity is kept for its Login.
const identityTTL = 2 * time.Minute

var identities = struct {
	mu sync.Mutex
	m  map[string]netIdentity // by the token's cpk claim
}{m: map[string]netIdentity{}}

// tokenClaims decodes a JWT's claims without checking it.
func tokenClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(b, &claims) != nil {
		return nil
	}
	return claims
}

// verifyOfferIdentity is the NetherNet listener's VerifyClientToken. It
// never turns a console away (the game server it picks checks sign-in
// itself); it records whether Microsoft issued the token, and returns the
// key the offer must be signed with.
func verifyOfferIdentity(ctx context.Context, token string) (*ecdsa.PublicKey, error) {
	return recordOfferIdentity(ctx, token, sharedVerifier.get())
}

func recordOfferIdentity(ctx context.Context, token string, v *oidc.IDTokenVerifier) (*ecdsa.PublicKey, error) {
	claims := tokenClaims(token)
	cpk, _ := claims["cpk"].(string)
	if cpk == "" {
		return nil, nil // the library reads (or rejects) the key itself
	}
	var key ecdsa.PublicKey
	if err := login.ParsePublicKey(cpk, &key); err != nil {
		return nil, nil
	}
	id := netIdentity{At: time.Now(), How: "sign-in service unreachable"}
	id.XUID, _ = claims["xid"].(string)
	id.Name, _ = claims["xname"].(string)
	if v != nil {
		vctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		t, err := v.Verify(vctx, token)
		cancel()
		if err == nil {
			var c struct {
				XUID string `json:"xid"`
				Name string `json:"xname"`
			}
			if err := t.Claims(&c); err == nil && c.XUID != "" {
				id = netIdentity{Verified: true, XUID: c.XUID, Name: c.Name, At: time.Now()}
			} else {
				id.How = "token has no Xbox ID"
			}
		} else {
			id.How = shortError(err)
		}
	}
	identities.mu.Lock()
	for k, x := range identities.m {
		if time.Since(x.At) > identityTTL {
			delete(identities.m, k)
		}
	}
	identities.m[login.MarshalPublicKey(&key)] = id
	identities.mu.Unlock()
	iss, _ := claims["iss"].(string)
	slog.Info("NetherNet connection identity", "gamertag", id.Name, "verified", id.Verified, "detail", id.How,
		"issuer", clip(iss), "audience", value(claims["aud"]))
	return &key, nil
}

// offerIdentity returns the verified identity for a Login's key, if its
// connection offer carried one.
func offerIdentity(cpk string) (netIdentity, bool) {
	var key ecdsa.PublicKey
	if cpk == "" || login.ParsePublicKey(cpk, &key) != nil {
		return netIdentity{}, false
	}
	cpk = login.MarshalPublicKey(&key)
	identities.mu.Lock()
	defer identities.mu.Unlock()
	id, ok := identities.m[cpk]
	if !ok || !id.Verified || time.Since(id.At) > identityTTL {
		return netIdentity{}, false
	}
	return id, true
}

// loginKey is the cpk claim of a connection request's sign-in token.
func loginKey(req []byte) string {
	authJSON, _, err := splitRequest(req)
	if err != nil {
		return ""
	}
	var auth struct {
		Token string `json:"Token"`
	}
	if json.Unmarshal(authJSON, &auth) != nil {
		return ""
	}
	cpk, _ := tokenClaims(auth.Token)["cpk"].(string)
	return cpk
}
