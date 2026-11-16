package xbox

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
)

// The session the account publishes to Xbox Live's Multiplayer Session
// Directory (MPSD) is what friends see under Play → Friends as a joinable
// world. Its custom properties are the same as a game hosting a world sends
// (field names and types matter: the game hides worlds it can't read). The
// layout follows gophertunnel's room package and MCXboxBroadcast.

const (
	// templateName is the MPSD session template Minecraft uses for worlds.
	templateName = "MinecraftLobby"
	// broadcastFriendsOnly shows the world to the account's friends only
	// (not to friends of friends).
	broadcastFriendsOnly = 2
	// transportNetherNet is the TransportLayer value for NetherNet.
	transportNetherNet = 2
	// connectionJSONRPC is the SupportedConnections type for joining through
	// the signaling service (JSON-RPC over WebSocket).
	connectionJSONRPC = 7
	// joinableByFriends is the only Joinability value the game shows; any
	// other one hides the world.
	joinableByFriends = "joinable_by_friends"
)

// World is what friends see for the list: its name, the line under it, and
// how many are in it.
type World struct {
	Name       string
	Subtitle   string
	Players    int
	MaxPlayers int
}

// connection is one way into the world.
type connection struct {
	Type          int         `json:"ConnectionType"`
	HostIPAddress string      `json:"HostIpAddress"`
	HostPort      uint16      `json:"HostPort"`
	NetherNetID   json.Number `json:"NetherNetId"`
	PmsgID        string      `json:"PmsgId"`
}

// sessionProperties are the custom properties of the session.
type sessionProperties struct {
	Joinability             string            `json:"Joinability"`
	HostName                string            `json:"hostName"`
	OwnerID                 string            `json:"ownerId"`
	RakNetGUID              string            `json:"rakNetGUID"`
	Version                 string            `json:"version"`
	LevelID                 string            `json:"levelId"`
	WorldName               string            `json:"worldName"`
	WorldType               string            `json:"worldType"`
	Protocol                int32             `json:"protocol"`
	MemberCount             int               `json:"MemberCount"`
	MaxMemberCount          int               `json:"MaxMemberCount"`
	BroadcastSetting        int               `json:"BroadcastSetting"`
	LanGame                 bool              `json:"LanGame"`
	IsEditorWorld           bool              `json:"isEditorWorld"`
	IsHardcore              bool              `json:"isHardcore"`
	TransportLayer          int               `json:"TransportLayer"`
	OnlineCrossPlatformGame bool              `json:"OnlineCrossPlatformGame"`
	CrossPlayDisabled       bool              `json:"CrossPlayDisabled"`
	TitleID                 int64             `json:"TitleId"`
	SupportedConnections    []connection      `json:"SupportedConnections"`
	Nonces                  map[string]string `json:"nonces"`
}

// sessionInfo is what's needed to describe the session: the account, how to
// reach the list, and the world shown.
type sessionInfo struct {
	XUID      string
	NetworkID string    // the list's NetherNet network ID on the signaling service
	PmsgID    uuid.UUID // the account's player messaging ID on the signaling service
	LevelID   string
	World     World
	Members   int
	Nonces    map[string]string
}

// properties encodes the session's custom properties.
func (s sessionInfo) properties() ([]byte, error) {
	w := s.World
	name := strings.TrimSpace(w.Name)
	if name == "" {
		name = "Server List"
	}
	sub := strings.TrimSpace(w.Subtitle)
	if sub == "" {
		sub = name
	}
	members := max(s.Members, w.Players, 1)
	maxMembers := max(w.MaxPlayers, members+1)
	nonces := s.Nonces
	if nonces == nil {
		nonces = map[string]string{}
	}
	return json.Marshal(sessionProperties{
		Joinability:             joinableByFriends,
		HostName:                sub,
		OwnerID:                 s.XUID,
		Version:                 protocol.CurrentVersion,
		LevelID:                 s.LevelID,
		WorldName:               name,
		WorldType:               "Survival",
		Protocol:                protocol.CurrentProtocol,
		MemberCount:             members,
		MaxMemberCount:          maxMembers,
		BroadcastSetting:        broadcastFriendsOnly,
		TransportLayer:          transportNetherNet,
		OnlineCrossPlatformGame: true,
		SupportedConnections: []connection{{
			Type:        connectionJSONRPC,
			NetherNetID: json.Number(s.NetworkID),
			PmsgID:      s.PmsgID.String(),
		}},
		Nonces: nonces,
	})
}

// newLevelID is a random level ID for the session.
func newLevelID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// syncNonces gives each member of the session (other than the account
// itself) a nonce and drops those of members who left. The game reads its
// own nonce from the session before it joins. It reports whether anything
// changed.
func syncNonces(nonces map[string]string, members []string, owner string) bool {
	active := make(map[string]bool, len(members))
	for _, x := range members {
		if x = strings.TrimSpace(x); x != "" && x != owner {
			active[x] = true
		}
	}
	changed := false
	for x := range nonces {
		if !active[x] {
			delete(nonces, x)
			changed = true
		}
	}
	for x := range active {
		if _, ok := nonces[x]; !ok {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			nonces[x] = hex.EncodeToString(b)
			changed = true
		}
	}
	return changed
}

// validNetworkID reports whether id is a NetherNet network ID the session can
// carry (a non-zero decimal number).
func validNetworkID(id string) bool {
	n, err := strconv.ParseUint(id, 10, 64)
	return err == nil && n != 0 && strconv.FormatUint(n, 10) == id
}
