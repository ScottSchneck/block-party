package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The Xbox account's page reports it signed out to the owner; signing out
// with nothing signed in is fine; nobody else gets in.
func TestXboxAccountRoutes(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.routes()
	do := func(method, path string, signedIn bool) (int, map[string]any) {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("X-Block-Party", "1")
		if signedIn {
			signIn(r, s)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var v map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		return w.Code, v
	}
	if code, v := do("GET", "/api/xbox", true); code != http.StatusOK || v["state"] != "signed-out" {
		t.Errorf("GET /api/xbox: %d %v", code, v)
	}
	if code, v := do("POST", "/api/xbox/sign-out", true); code != http.StatusOK || v["state"] != "signed-out" {
		t.Errorf("sign out: %d %v", code, v)
	}
	if code, v := do("POST", "/api/xbox/cancel", true); code != http.StatusOK || v["state"] != "signed-out" {
		t.Errorf("cancel: %d %v", code, v)
	}
	if code, _ := do("GET", "/api/xbox", false); code != http.StatusUnauthorized {
		t.Errorf("signed out of the panel: %d", code)
	}
}
