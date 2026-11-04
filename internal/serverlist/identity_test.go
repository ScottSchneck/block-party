package serverlist

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
)

const testIssuer = "https://authorization.test/"

// signedToken makes an RS256 token like the one in a NetherNet offer.
func signedToken(t *testing.T, signer *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	body := enc(map[string]any{"alg": "RS256", "typ": "JWT"}) + "." + enc(claims)
	sum := sha256.Sum256([]byte(body))
	sig, err := rsa.SignPKCS1v15(rand.Reader, signer, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return body + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func offerClaims(console *ecdsa.PrivateKey, name string) map[string]any {
	return map[string]any{
		"iss": testIssuer, "aud": "api://auth-minecraft-services/multiplayer",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"cpk": login.MarshalPublicKey(&console.PublicKey), "xid": "2535400000000001", "xname": name,
	}
}

func TestNetherNetIdentityVerifiesASelfSignedLogin(t *testing.T) {
	microsoft, _ := rsa.GenerateKey(rand.Reader, 2048)
	forger, _ := rsa.GenerateKey(rand.Reader, 2048)
	verifier := oidc.NewVerifier(testIssuer, &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&microsoft.PublicKey}},
		&oidc.Config{ClientID: "api://auth-minecraft-services/multiplayer"})

	console, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	key, err := recordOfferIdentity(context.Background(), signedToken(t, microsoft, offerClaims(console, "Alex")), verifier)
	if err != nil || key == nil || !key.Equal(&console.PublicKey) {
		t.Fatalf("offer key: %v, %v", key, err)
	}
	payload := loginPacket(login.EncodeOffline(login.IdentityData{DisplayName: "Someone"}, login.ClientData{}, console, false))

	s := checkSignIn(payload, nil, true)
	if !s.Verified || s.Name != "Alex" || s.XUID != "2535400000000001" {
		t.Fatalf("over NetherNet: got %+v", s)
	}
	if s := checkSignIn(payload, nil, false); s.Verified {
		t.Fatal("RakNet proves no key, so the NetherNet identity must not count there")
	}

	// A Login with another key gets nothing from Alex's offer.
	other, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if s := checkSignIn(loginPacket(login.EncodeOffline(login.IdentityData{DisplayName: "Alex"}, login.ClientData{}, other, false)), nil, true); s.Verified {
		t.Fatal("another key must not be verified")
	}

	// A token not signed by Microsoft is recorded as unverified.
	faked, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if _, err := recordOfferIdentity(context.Background(), signedToken(t, forger, offerClaims(faked, "Mallory")), verifier); err != nil {
		t.Fatal(err)
	}
	if s := checkSignIn(loginPacket(login.EncodeOffline(login.IdentityData{DisplayName: "Mallory"}, login.ClientData{}, faked, false)), nil, true); s.Verified {
		t.Fatal("a forged offer token must not verify")
	}

	// Without the sign-in service nothing is verified, but the key is still returned.
	later, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if key, _ := recordOfferIdentity(context.Background(), signedToken(t, microsoft, offerClaims(later, "Sam")), nil); key == nil {
		t.Fatal("the offer key should still be returned")
	}
	if _, ok := offerIdentity(login.MarshalPublicKey(&later.PublicKey)); ok {
		t.Fatal("unverifiable offer must not count")
	}
}

func TestOfferWithoutKeyLeavesItToTheLibrary(t *testing.T) {
	if key, err := recordOfferIdentity(context.Background(), "not.a.token", nil); key != nil || err != nil {
		t.Fatalf("got %v, %v", key, err)
	}
}
