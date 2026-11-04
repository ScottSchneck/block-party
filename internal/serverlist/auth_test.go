package serverlist

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
)

// loginPacket wraps a connection request the way a Login packet payload
// carries it.
func loginPacket(req []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(protocol.CurrentProtocol))
	out = binary.AppendUvarint(out, uint64(len(req)))
	return append(out, req...)
}

func offlineLogin(t *testing.T, legacy bool) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := login.IdentityData{DisplayName: "Steve", Identity: "5c1f7b3e-0000-4000-8000-000000000000", XUID: "2535400000000000"}
	return loginPacket(login.EncodeOffline(id, login.ClientData{}, key, legacy))
}

func TestSelfSignedTokenIsNotVerifiedButDescribed(t *testing.T) {
	s := checkSignIn(offlineLogin(t, false), nil, false)
	if s.Verified {
		t.Fatal("a self-signed token must not count as verified")
	}
	desc := fmt.Sprintln(s.Token...)
	for _, want := range []string{"token.alg ES384", "authType 2", "claim.xid true", "claim.xname Steve", "token.selfSigned true"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description %q is missing %q", desc, want)
		}
	}
	if strings.Contains(desc, "MHYw") {
		t.Error("keys must not be logged")
	}
}

func TestSelfSignedChainIsNotVerified(t *testing.T) {
	if s := checkSignIn(offlineLogin(t, true), nil, false); s.Verified {
		t.Fatal("a self-signed certificate chain must not count as verified")
	}
}

func TestUnreadableLogin(t *testing.T) {
	for _, payload := range [][]byte{nil, {0, 0, 0, 1}, {0, 0, 0, 1, 200}} {
		if s := checkSignIn(payload, nil, false); s.Verified || !strings.Contains(s.How, "unreadable") {
			t.Errorf("payload %v: got %v", payload, s)
		}
	}
}

func TestWithoutTokenKeepsTheRest(t *testing.T) {
	req, err := connectionRequest(offlineLogin(t, false))
	if err != nil {
		t.Fatal(err)
	}
	stripped, err := withoutToken(req)
	if err != nil {
		t.Fatal(err)
	}
	auth, rest, err := splitRequest(stripped)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(auth), `"Token"`) {
		t.Error("token still present")
	}
	_, origRest, _ := splitRequest(req)
	if string(rest) != string(origRest) {
		t.Error("client data changed")
	}
}
