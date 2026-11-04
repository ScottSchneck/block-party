package web

import (
	"strings"
	"testing"
)

func TestCurseForgeKeyIsTheOwners(t *testing.T) {
	s, dir := newTestServer(t)
	addTestServer(t, s, dir, "ava", "Ava's Server", 19140)
	h := s.routes()
	owner := ownerCookie(s)
	call(h, "POST", "/api/users", `{"username":"Ava","password":"ava password 1"}`, owner)
	call(h, "PUT", "/api/users/Ava", `{"grants":{"ava":"care"}}`, owner)
	ava := sessionFrom(t, call(h, "POST", "/api/auth/login", `{"username":"ava","password":"ava password 1"}`))
	if w := call(h, "PUT", "/api/curseforge", `{"key":"x"}`, ava); w.Code != 403 {
		t.Errorf("a kid setting the key: %d", w.Code)
	}
	if w := call(h, "GET", "/api/curseforge", "", owner); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"set":false}` {
		t.Errorf("state: %d %s", w.Code, w.Body)
	}
	if w := call(h, "GET", "/api/curseforge/search?kind=modpacks", "", ava); w.Code != 403 {
		t.Errorf("a kid searching modpacks: %d", w.Code)
	}
	if w := call(h, "GET", "/api/curseforge/search?kind=addons", "", ava); w.Code != 400 || !strings.Contains(w.Body.String(), "CurseForge API key") {
		t.Errorf("add-ons without a key: %d %s", w.Code, w.Body)
	}
	if w := call(h, "GET", "/api/curseforge/search?kind=nope", "", owner); w.Code != 400 {
		t.Errorf("an unknown kind: %d", w.Code)
	}
	if w := call(h, "GET", "/api/info", "", owner); !strings.Contains(w.Body.String(), `"curseforge":false`) {
		t.Errorf("info: %s", w.Body)
	}
}
