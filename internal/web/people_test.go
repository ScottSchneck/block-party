package web

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// addTestServer puts a stopped server with the given ID in the panel.
func addTestServer(t *testing.T, s *Server, dir, id, name string, port int) {
	t.Helper()
	sdir := filepath.Join(dir, "servers", id)
	os.MkdirAll(sdir, 0o755)
	meta, _ := json.Marshal(map[string]any{"id": id, "name": name, "type": "bedrock", "port": port, "portV6": port + 1})
	os.WriteFile(filepath.Join(sdir, ".blockheads.json"), meta, 0o644)
	os.WriteFile(filepath.Join(sdir, "bedrock_server"), []byte("x"), 0o755)
	os.WriteFile(filepath.Join(sdir, "server.properties"), []byte("allow-list=true\n"), 0o644)
	os.WriteFile(filepath.Join(sdir, "allowlist.json"), []byte("[]"), 0o644)
	os.WriteFile(filepath.Join(sdir, "permissions.json"), []byte("[]"), 0o644)
	if err := s.mgr.Load(); err != nil {
		t.Fatal(err)
	}
}

func ownerCookie(s *Server) *http.Cookie {
	c, _ := testCookies.Load(s)
	return c.(*http.Cookie)
}

func TestKidsSeeAndDoOnlyWhatTheyreAllowed(t *testing.T) {
	s, dir := newTestServer(t)
	addTestServer(t, s, dir, "ava", "Ava's Server", 19140)
	addTestServer(t, s, dir, "sam", "Sams Server", 19138)
	addTestServer(t, s, dir, "dad", "Schneck World", 19142)
	h := s.routes()
	owner := ownerCookie(s)

	// The owner makes accounts; nobody else can.
	if w := call(h, "POST", "/api/users", `{"username":"Ava","password":"ava password 1"}`, owner); w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	if w := call(h, "POST", "/api/users", `{"username":"ava","password":"another one 1"}`, owner); w.Code != 400 {
		t.Errorf("duplicate: %d", w.Code)
	}
	if w := call(h, "POST", "/api/users", `{"username":"scott","password":"another one 1"}`, owner); w.Code != 400 {
		t.Errorf("owner's name: %d", w.Code)
	}
	call(h, "POST", "/api/users", `{"username":"Sam","password":"sam password 1","canAdd":true}`, owner)
	if w := call(h, "PUT", "/api/users/Ava", `{"grants":{"ava":"care","sam":"start"}}`, owner); w.Code != 200 {
		t.Fatalf("grants: %d %s", w.Code, w.Body)
	}
	if w := call(h, "PUT", "/api/users/Ava", `{"grants":{"nope":"care"}}`, owner); w.Code != 400 {
		t.Errorf("unknown server: %d", w.Code)
	}
	if w := call(h, "PUT", "/api/users/Ava", `{"grants":{"ava":"god"}}`, owner); w.Code != 400 {
		t.Errorf("unknown role: %d", w.Code)
	}
	call(h, "PUT", "/api/users/Sam", `{"canAdd":false,"grants":{"sam":"run"}}`, owner)

	ava := sessionFrom(t, call(h, "POST", "/api/auth/login", `{"username":"ava","password":"ava password 1"}`))
	sam := sessionFrom(t, call(h, "POST", "/api/auth/login", `{"username":"Sam","password":"sam password 1"}`))

	var st map[string]any
	json.Unmarshal(call(h, "GET", "/api/auth/state", "", ava).Body.Bytes(), &st)
	if st["username"] != "Ava" || st["owner"] != false || st["canAdd"] != false || st["prefs"].(map[string]any)["style"] != "treehouse" {
		t.Errorf("state: %v", st)
	}

	// Ava sees her server and Sam's (which she can only turn on), not Dad's.
	var list []listedServer
	json.Unmarshal(call(h, "GET", "/api/servers", "", ava).Body.Bytes(), &list)
	got := map[string]int{}
	for _, x := range list {
		got[x.ID] = x.Access
	}
	if len(got) != 2 || got["ava"] != 3 || got["sam"] != 1 {
		t.Errorf("Ava's list: %v", got)
	}

	for _, c := range []struct {
		who          *http.Cookie
		method, path string
		body         string
		want         int
	}{
		{ava, "GET", "/api/servers/dad/players", "", 404},
		{ava, "POST", "/api/servers/dad/start", "", 404},
		{ava, "GET", "/api/servers/sam/players", "", 403},
		{ava, "GET", "/api/servers/sam/backups", "", 403},
		{ava, "POST", "/api/servers/sam/stop", "", 403},
		{ava, "GET", "/api/servers/ava/players", "", 200},
		{ava, "GET", "/api/servers/ava/backups", "", 200},
		{ava, "PUT", "/api/servers/ava/backups/plan", `{"every":"6h","keep":3}`, 200},
		{sam, "PUT", "/api/servers/sam/backups/plan", `{"every":"6h","keep":3}`, 403},
		{sam, "POST", "/api/servers/sam/update", "", 403},
		{sam, "GET", "/api/servers/sam/players", "", 200},
		{sam, "GET", "/api/servers/ava/players", "", 404},
		// Owner-only things.
		{ava, "GET", "/api/users", "", 403},
		{ava, "POST", "/api/users", `{"username":"Eve","password":"eve password 1"}`, 403},
		{ava, "PUT", "/api/users/Ava", `{"grants":{"dad":"care"}}`, 403},
		{ava, "GET", "/api/import", "", 403},
		{ava, "GET", "/api/setup-guide", "", 403},
		{ava, "POST", "/api/setup-guide/gamertag", `{"gamertag":"Ava"}`, 403},
		{ava, "POST", "/api/servers", `{"name":"Mine","acceptEula":true}`, 403},
	} {
		if w := call(h, c.method, c.path, c.body, c.who); w.Code != c.want {
			t.Errorf("%s %s: %d, want %d (%s)", c.method, c.path, w.Code, c.want, w.Body)
		}
	}

	// Taking Ava off Sam's server hides it from her.
	call(h, "PUT", "/api/users/Ava", `{"grants":{"sam":""}}`, owner)
	if w := call(h, "POST", "/api/servers/sam/start", "", ava); w.Code != 404 {
		t.Errorf("start after removal: %d", w.Code)
	}

	// Prefs are per account.
	call(h, "PUT", "/api/auth/prefs", `{"style":"control"}`, owner)
	var prefs map[string]string
	json.Unmarshal(call(h, "GET", "/api/auth/prefs", "", ava).Body.Bytes(), &prefs)
	if prefs["style"] != "treehouse" {
		t.Errorf("Ava's prefs changed with the owner's: %v", prefs)
	}

	// The activity log: Ava sees what happened on her server only.
	var acts []map[string]any
	json.Unmarshal(call(h, "GET", "/api/activity", "", ava).Body.Bytes(), &acts)
	if len(acts) == 0 {
		t.Fatal("no activity for Ava")
	}
	for _, a := range acts {
		if a["serverId"] != "ava" {
			t.Errorf("Ava sees %v", a)
		}
	}
	json.Unmarshal(call(h, "GET", "/api/activity", "", owner).Body.Bytes(), &acts)
	text := ""
	for _, a := range acts {
		text += a["user"].(string) + " " + a["text"].(string) + "\n"
	}
	for _, want := range []string{"Scott made an account for Ava", "Scott let Ava take care of it (Ava's Server)", "Ava changed the backup schedule", "Scott took Ava off Sams Server", "Scott stopped Sam adding servers"} {
		if !strings.Contains(text, want) {
			t.Errorf("activity lacks %q:\n%s", want, text)
		}
	}

	// A new password signs Ava out; removing her account does too.
	if w := call(h, "POST", "/api/users/Ava/password", `{"password":"new ava pass 2"}`, owner); w.Code != 200 {
		t.Fatalf("password: %d %s", w.Code, w.Body)
	}
	if w := call(h, "GET", "/api/servers", "", ava); w.Code != 401 {
		t.Errorf("still signed in after a new password: %d", w.Code)
	}
	ava = sessionFrom(t, call(h, "POST", "/api/auth/login", `{"username":"Ava","password":"new ava pass 2"}`))
	if w := call(h, "DELETE", "/api/users/ava", "", owner); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if w := call(h, "GET", "/api/servers", "", ava); w.Code != 401 {
		t.Errorf("signed in after the account was removed: %d", w.Code)
	}
	if w := call(h, "DELETE", "/api/users/ava", "", owner); w.Code != 404 {
		t.Errorf("delete twice: %d", w.Code)
	}
	if w := call(h, "DELETE", "/api/users/Scott", "", owner); w.Code != 404 {
		t.Errorf("deleting the owner: %d", w.Code)
	}
	// Signing out everywhere only signs Sam out, not the owner.
	call(h, "POST", "/api/auth/logout-all", "", sam)
	if w := call(h, "GET", "/api/servers", "", owner); w.Code != 200 {
		t.Errorf("owner signed out by Sam: %d", w.Code)
	}

	// Everything survives a restart.
	s2, err := New(Options{DataDir: dir, TLS: true}, s.mgr)
	if err != nil {
		t.Fatal(err)
	}
	var users map[string]any
	json.Unmarshal(call(s2.routes(), "GET", "/api/users", "", owner).Body.Bytes(), &users)
	if u := users["users"].([]any); len(u) != 1 || u[0].(map[string]any)["username"] != "Sam" {
		t.Errorf("users after restart: %v", users)
	}
	json.Unmarshal(call(s2.routes(), "GET", "/api/activity?limit=500", "", owner).Body.Bytes(), &acts)
	if len(acts) < 5 {
		t.Errorf("activity after restart: %d", len(acts))
	}
}

func TestAKidWhoAddsAServerTakesCareOfIt(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.routes()
	owner := ownerCookie(s)
	call(h, "POST", "/api/users", `{"username":"Sam","password":"sam password 1","canAdd":true}`, owner)
	sam := sessionFrom(t, call(h, "POST", "/api/auth/login", `{"username":"Sam","password":"sam password 1"}`))
	w := call(h, "POST", "/api/servers", `{"name":"Sam's Castle","acceptEula":true,"ownerGamertag":"SamTag"}`, sam)
	if w.Code != 202 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var st listedServer
	json.Unmarshal(w.Body.Bytes(), &st)
	var list []listedServer
	json.Unmarshal(call(h, "GET", "/api/servers", "", sam).Body.Bytes(), &list)
	if len(list) != 1 || list[0].ID != st.ID || list[0].Access != 3 {
		t.Errorf("Sam's list: %+v", list)
	}
	if s.mgr.Settings().OwnerGamertag != "" {
		t.Error("Sam's gamertag became the owner's")
	}
	if w := call(h, "POST", "/api/servers", `{"name":"Bad","acceptEula":true,"ownerGamertag":"bad\"tag"}`, sam); w.Code != 400 {
		t.Errorf("bad gamertag: %d", w.Code)
	}
	// Nobody but the owner can delete servers.
	if w := call(h, "DELETE", "/api/servers/"+st.ID, "", sam); w.Code != 403 {
		t.Errorf("delete: %d", w.Code)
	}
	waitFor(t, func() bool { return s.mgr.List()[0].State != "installing" })
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func TestRawEditorKeepsTheWorldInsideTheServer(t *testing.T) {
	s, dir := newTestServer(t)
	addTestServer(t, s, dir, "ava", "Ava's Server", 19140)
	w := call(s.routes(), "PUT", "/api/servers/ava/properties", `{"text":"level-name=../../dad/worlds/Bedrock level\n"}`, ownerCookie(s))
	if w.Code != 400 {
		t.Errorf("world outside the server: %d %s", w.Code, w.Body)
	}
}

func TestOutsideAccessPermissions(t *testing.T) {
	s, dir := newTestServer(t)
	addTestServer(t, s, dir, "ava", "Ava's Server", 19140)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.out.Run(ctx)
	h := s.routes()
	owner := ownerCookie(s)
	call(h, "POST", "/api/users", `{"username":"Ava","password":"ava password 1"}`, owner)
	call(h, "POST", "/api/users", `{"username":"Sam","password":"sam password 1"}`, owner)
	call(h, "PUT", "/api/users/Ava", `{"grants":{"ava":"care"}}`, owner)
	call(h, "PUT", "/api/users/Sam", `{"grants":{"ava":"run"}}`, owner)
	ava := sessionFrom(t, call(h, "POST", "/api/auth/login", `{"username":"Ava","password":"ava password 1"}`))
	sam := sessionFrom(t, call(h, "POST", "/api/auth/login", `{"username":"Sam","password":"sam password 1"}`))

	if w := call(h, "GET", "/api/outside", "", ava); w.Code != 403 {
		t.Errorf("kid reads outside settings: %d", w.Code)
	}
	if w := call(h, "PUT", "/api/outside", `{"enabled":true,"address":"192.168.1.9"}`, owner); w.Code != 400 {
		t.Errorf("home address accepted: %d %s", w.Code, w.Body)
	}
	if w := call(h, "PUT", "/api/outside", `{"enabled":true,"address":"203.0.113.5"}`, owner); w.Code != 200 {
		t.Fatalf("settings: %d %s", w.Code, w.Body)
	}
	if w := call(h, "POST", "/api/servers/ava/outside", `{"open":true}`, sam); w.Code != 403 {
		t.Errorf("run-level kid opened it: %d", w.Code)
	}
	w := call(h, "POST", "/api/servers/ava/outside", `{"open":true}`, ava)
	if w.Code != 200 {
		t.Fatalf("open: %d %s", w.Code, w.Body)
	}
	var v serverOutsideView
	json.Unmarshal(w.Body.Bytes(), &v)
	if !v.Open || !v.Reached || v.Address != "203.0.113.5" || v.Port != 19140 {
		t.Errorf("view: %+v", v)
	}
	json.Unmarshal(call(h, "GET", "/api/servers/ava/outside", "", sam).Body.Bytes(), &v)
	if !v.Open || v.Address != "203.0.113.5" {
		t.Errorf("Sam's view: %+v", v)
	}
	if w := call(h, "POST", "/api/servers/ava/allowlist-enabled", `{"enabled":false}`, owner); w.Code == 200 {
		t.Error("allowlist turned off while open")
	}
}

func TestAddonRoutes(t *testing.T) {
	s, dir := newTestServer(t)
	addTestServer(t, s, dir, "ava", "Ava's Server", 19140)
	wd := filepath.Join(dir, "servers", "ava", "worlds", "Bedrock level")
	os.MkdirAll(filepath.Join(wd, "db"), 0o755)
	os.WriteFile(filepath.Join(wd, "level.dat"), []byte("x"), 0o644)
	h := s.routes()
	owner := ownerCookie(s)
	call(h, "POST", "/api/users", `{"username":"Ava","password":"ava password 1"}`, owner)
	call(h, "POST", "/api/users", `{"username":"Sam","password":"sam password 1"}`, owner)
	call(h, "PUT", "/api/users/Ava", `{"grants":{"ava":"care"}}`, owner)
	call(h, "PUT", "/api/users/Sam", `{"grants":{"ava":"run"}}`, owner)
	ava := sessionFrom(t, call(h, "POST", "/api/auth/login", `{"username":"Ava","password":"ava password 1"}`))
	sam := sessionFrom(t, call(h, "POST", "/api/auth/login", `{"username":"Sam","password":"sam password 1"}`))

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("manifest.json")
	f.Write([]byte(`{"header":{"name":"Blocks","uuid":"66666666-2222-3333-4444-555555555555","version":[1,0,0]},"modules":[{"type":"resources"}]}`))
	zw.Close()
	pack := buf.String()

	upload := func(path, body string, c *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("X-Block-Party", "1")
		r.Header.Set("X-File-Name", "blocks.mcpack")
		r.AddCookie(c)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := upload("/api/servers/ava/packs", pack, sam); w.Code != 403 {
		t.Errorf("run-level install: %d", w.Code)
	}
	if w := upload("/api/servers/ava/packs", pack, ava); w.Code != 200 {
		t.Fatalf("install: %d %s", w.Code, w.Body)
	}
	if w := call(h, "GET", "/api/servers/ava/packs", "", sam); w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"Blocks"`) {
		t.Errorf("list: %d %s", w.Code, w.Body)
	}
	for _, c := range []struct {
		who          *http.Cookie
		method, path string
		want         int
	}{
		{sam, "POST", "/api/servers/ava/packs/66666666-2222-3333-4444-555555555555", 403},
		{sam, "DELETE", "/api/servers/ava/packs/66666666-2222-3333-4444-555555555555", 403},
		{sam, "GET", "/api/servers/ava/worlds/Bedrock%20level/download", 403},
		{ava, "DELETE", "/api/servers/ava/worlds/Other", 403},
		{sam, "GET", "/api/servers/ava/worlds", 200},
	} {
		if w := call(h, c.method, c.path, `{"enabled":false}`, c.who); w.Code != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, w.Code, c.want)
		}
	}
	w := call(h, "GET", "/api/servers/ava/worlds/Bedrock%20level/download", "", ava)
	if w.Code != 200 || !strings.HasSuffix(w.Header().Get("Content-Disposition"), `.mcworld"`) {
		t.Fatalf("download: %d %v %s", w.Code, w.Header(), w.Body)
	}
	if zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len())); err != nil || len(zr.File) == 0 {
		t.Errorf("download isn't a world: %v", err)
	}
	if w := call(h, "GET", "/api/servers/ava/worlds/..%2F..%2Fx/download", "", ava); w.Code == 200 {
		t.Error("downloaded a path")
	}
}

func TestOnlyTheOwnerDeletesServers(t *testing.T) {
	s, dir := newTestServer(t)
	addTestServer(t, s, dir, "ava", "Ava's Server", 19140)
	h := s.routes()
	owner := ownerCookie(s)
	call(h, "POST", "/api/users", `{"username":"Ava","password":"ava password 1","canAdd":true}`, owner)
	call(h, "PUT", "/api/users/Ava", `{"grants":{"ava":"care"}}`, owner)
	ava := sessionFrom(t, call(h, "POST", "/api/auth/login", `{"username":"Ava","password":"ava password 1"}`))
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"DELETE", "/api/servers/ava", "", 403},
		{"GET", "/api/deleted-servers", "", 403},
		{"PUT", "/api/nightly", `{"autoUpdate":true,"at":"04:00"}`, 403},
		{"POST", "/api/servers/ava/hold-version", `{"hold":true}`, 200},
		{"POST", "/api/servers/ava/consoles", `{"on":true}`, 409},                 // a Bedrock server
		{"POST", "/api/servers/ava/plugins", `{"source":"hangar","id":"x"}`, 403}, // owner only
		{"GET", "/api/servers/ava/plugins/search?q=x", "", 403},
		{"DELETE", "/api/servers/ava/plugins/x.jar", "", 403},
		{"POST", "/api/servers/ava/plugins/x.jar/update", "", 403},
		{"GET", "/api/servers/ava/plugins", "", 400},                                 // care may look (a Bedrock server has none)
		{"POST", "/api/servers/ava/plugins/x.jar/enabled", `{"enabled":false}`, 409}, // and turn them off and on
	} {
		if w := call(h, c.method, c.path, c.body, ava); w.Code != c.want {
			t.Errorf("%s %s: %d, want %d (%s)", c.method, c.path, w.Code, c.want, w.Body)
		}
	}
	if w := call(h, "PUT", "/api/nightly", `{"autoUpdate":true,"at":"4:05"}`, owner); w.Code != 200 || !strings.Contains(w.Body.String(), `"at":"04:05"`) {
		t.Errorf("nightly: %d %s", w.Code, w.Body)
	}
	if w := call(h, "DELETE", "/api/servers/ava", "", owner); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if w := call(h, "GET", "/api/servers", "", ava); strings.Contains(w.Body.String(), `"ava"`) {
		t.Errorf("still listed: %s", w.Body)
	}
	if w := call(h, "GET", "/api/deleted-servers", "", owner); !strings.Contains(w.Body.String(), `"name":"Ava's Server"`) {
		t.Errorf("deleted list: %s", w.Body)
	}
}

func TestNotificationSettingsAreTheOwners(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.routes()
	owner := ownerCookie(s)
	call(h, "POST", "/api/users", `{"username":"Ava","password":"ava password 1"}`, owner)
	ava := sessionFrom(t, call(h, "POST", "/api/auth/login", `{"username":"Ava","password":"ava password 1"}`))
	if w := call(h, "GET", "/api/notifications", "", ava); w.Code != 403 {
		t.Errorf("kid reads notifications: %d", w.Code)
	}
	body := `{"settings":{"discord":{"on":true,"webhook":"https://discord.com/api/webhooks/1/secret","events":["crash"]}}}`
	w := call(h, "PUT", "/api/notifications", body, owner)
	if w.Code != 200 || strings.Contains(w.Body.String(), "1/secret") || !strings.Contains(w.Body.String(), `"discord.webhook":true`) {
		t.Errorf("put: %d %s", w.Code, w.Body)
	}
	if w := call(h, "GET", "/api/notifications", "", owner); strings.Contains(w.Body.String(), "1/secret") {
		t.Errorf("secret shown: %s", w.Body)
	}
	if w := call(h, "POST", "/api/notifications/test/email", "", owner); w.Code != 502 || !strings.Contains(w.Body.String(), "save first") {
		t.Errorf("test without settings: %d %s", w.Code, w.Body)
	}
}

// Play time is the owner's: kids only see their own time left, and can't
// start a server outside its hours.
func TestPlayTimeIsTheOwners(t *testing.T) {
	s, dir := newTestServer(t)
	addTestServer(t, s, dir, "ava", "Ava's Server", 19140)
	h := s.routes()
	owner := ownerCookie(s)
	call(h, "POST", "/api/users", `{"username":"Ava","password":"ava password 1"}`, owner)
	call(h, "PUT", "/api/users/Ava", `{"grants":{"ava":"care"}}`, owner)
	ava := sessionFrom(t, call(h, "POST", "/api/auth/login", `{"username":"Ava","password":"ava password 1"}`))

	kid := `{"old":"","kid":{"name":"Ava","players":["Ava Plays"],"limit":{"on":true,"school":60,"weekend":90}}}`
	if w := call(h, "POST", "/api/playtime/kids", kid, ava); w.Code != 403 {
		t.Errorf("a kid set limits: %d", w.Code)
	}
	if w := call(h, "POST", "/api/playtime/kids", kid, owner); w.Code != 200 {
		t.Fatalf("owner: %d %s", w.Code, w.Body)
	}
	for _, path := range []string{"/api/playtime"} {
		if w := call(h, "GET", path, "", ava); w.Code != 403 {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
	if w := call(h, "POST", "/api/playtime/kids/extra", `{"name":"Ava","minutes":30}`, ava); w.Code != 403 {
		t.Errorf("a kid gave herself time: %d", w.Code)
	}
	var mine map[string]any
	json.Unmarshal(call(h, "GET", "/api/me/playtime", "", ava).Body.Bytes(), &mine)
	if mine["left"] == nil || mine["why"] != "limit" {
		t.Errorf("her own time: %v", mine)
	}

	// Hours that don't include now.
	now := time.Now()
	span := `{"from":"00:00","until":"01:00"}`
	if now.Hour() < 2 {
		span = `{"from":"12:00","until":"13:00"}`
	}
	hours := `{"on":true,"school":` + span + `,"weekend":` + span + `}`
	if w := call(h, "POST", "/api/servers/ava/hours", hours, ava); w.Code != 403 {
		t.Errorf("a kid changed hours: %d", w.Code)
	}
	if w := call(h, "POST", "/api/servers/ava/hours", hours, owner); w.Code != 200 {
		t.Fatalf("hours: %d %s", w.Code, w.Body)
	}
	if w := call(h, "POST", "/api/servers/ava/start", "", ava); w.Code != 409 || !strings.Contains(w.Body.String(), "outside Ava's Server's hours") {
		t.Errorf("a kid started it outside its hours: %d %s", w.Code, w.Body)
	}
	if w := call(h, "POST", "/api/servers/ava/open-late", `{"until":"bad"}`, owner); w.Code != 400 {
		t.Errorf("bad time: %d", w.Code)
	}
	late := now.Add(2 * time.Hour).Format("15:04")
	var st map[string]any
	w := call(h, "POST", "/api/servers/ava/open-late", `{"until":"`+late+`"}`, owner)
	json.Unmarshal(w.Body.Bytes(), &st)
	if w.Code != 200 || st["hours"].(map[string]any)["open"] != true || st["hours"].(map[string]any)["late"] == nil {
		t.Errorf("open late: %d %s", w.Code, w.Body)
	}
	if w := call(h, "GET", "/api/servers/ava/played", "", ava); w.Code != 200 {
		t.Errorf("played: %d", w.Code)
	}
}
