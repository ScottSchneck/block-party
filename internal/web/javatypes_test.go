package web

import (
	"strings"
	"testing"
)

func TestModpacksAreTheOwners(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.routes()
	owner := ownerCookie(s)
	call(h, "POST", "/api/users", `{"username":"Sam","password":"sam password 1","canAdd":true}`, owner)
	sam := sessionFrom(t, call(h, "POST", "/api/auth/login", `{"username":"sam","password":"sam password 1"}`))
	if w := call(h, "POST", "/api/servers", `{"name":"Pack","edition":"java","acceptEula":true,"modpack":"abc"}`, sam); w.Code != 403 {
		t.Errorf("a kid adding a modpack: %d %s", w.Code, w.Body)
	}
	if w := call(h, "GET", "/api/modpacks?q=x", "", sam); w.Code != 403 {
		t.Errorf("a kid searching modpacks: %d", w.Code)
	}
	if w := upload(h, "/api/modpacks/upload?name=x", "pack.mrpack", "zip", sam); w.Code != 403 {
		t.Errorf("a kid uploading a modpack: %d", w.Code)
	}
	if w := upload(h, "/api/modpacks/upload?name=x", "pack.zip", "zip", owner); w.Code != 400 || !strings.Contains(w.Body.String(), "mrpack") {
		t.Errorf("only .mrpack: %d %s", w.Code, w.Body)
	}
	if w := call(h, "POST", "/api/servers", `{"name":"Odd","edition":"java","acceptEula":true,"loader":"bukkit"}`, sam); w.Code != 400 {
		t.Errorf("an unknown type: %d %s", w.Code, w.Body)
	}
	if w := call(h, "GET", "/api/java/versions?loader=nope", "", sam); w.Code != 502 && w.Code != 400 {
		t.Errorf("versions of an unknown type: %d", w.Code)
	}
}
