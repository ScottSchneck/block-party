package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func upload(h http.Handler, path, name, content string, c *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(content))
	r.Header.Set("X-Block-Party", "1")
	r.Header.Set("X-File-Name", url.QueryEscape(name))
	r.AddCookie(c)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestFileManagerRoutes(t *testing.T) {
	s, dir := newTestServer(t)
	addTestServer(t, s, dir, "ava", "Ava's Server", 19140)
	h := s.routes()
	owner := ownerCookie(s)
	call(h, "POST", "/api/users", `{"username":"Ava","password":"ava password 1"}`, owner)
	call(h, "POST", "/api/users", `{"username":"Sam","password":"sam password 1"}`, owner)
	call(h, "PUT", "/api/users/Ava", `{"grants":{"ava":"care"}}`, owner)
	call(h, "PUT", "/api/users/Sam", `{"grants":{"ava":"run"}}`, owner)
	ava := sessionFrom(t, call(h, "POST", "/api/auth/login", `{"username":"ava","password":"ava password 1"}`))
	sam := sessionFrom(t, call(h, "POST", "/api/auth/login", `{"username":"sam","password":"sam password 1"}`))

	if w := call(h, "GET", "/api/servers/ava/files", "", sam); w.Code != 403 {
		t.Errorf("someone who only runs the server: %d", w.Code)
	}
	w := call(h, "GET", "/api/servers/ava/files", "", ava)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "server.properties") || strings.Contains(w.Body.String(), ".blockheads.json") {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	if w := call(h, "GET", "/api/servers/ava/files?path=../..", "", ava); w.Code != 400 {
		t.Errorf("climbing out: %d", w.Code)
	}
	if w := call(h, "GET", "/api/servers/ava/files/text?path=server.properties", "", ava); w.Code != 200 || !strings.Contains(w.Body.String(), "allow-list=true") {
		t.Errorf("read: %d %s", w.Code, w.Body)
	}
	if w := call(h, "PUT", "/api/servers/ava/files/text", `{"path":"server.properties","text":"allow-list=false\n"}`, ava); w.Code != 200 {
		t.Errorf("write: %d %s", w.Code, w.Body)
	}
	if got := readFile(t, filepath.Join(dir, "servers", "ava", "server.properties")); got != "allow-list=false\n" {
		t.Errorf("written: %q", got)
	}
	if w := upload(h, "/api/servers/ava/files/upload?path=plugins", "evil.jar", "x", ava); w.Code != 403 {
		t.Errorf("a kid uploading code: %d %s", w.Code, w.Body)
	}
	if w := upload(h, "/api/servers/ava/files/upload", "notes.txt", "hello", ava); w.Code != 200 {
		t.Errorf("upload: %d %s", w.Code, w.Body)
	}
	if w := call(h, "POST", "/api/servers/ava/files/folder", `{"path":"plugins"}`, owner); w.Code != 200 {
		t.Errorf("the owner making the plugins folder: %d %s", w.Code, w.Body)
	}
	if w := upload(h, "/api/servers/ava/files/upload?path=plugins", "good.jar", "x", owner); w.Code != 200 {
		t.Errorf("the owner uploading a plugin: %d %s", w.Code, w.Body)
	}
	w = call(h, "GET", "/api/servers/ava/files/download?path=notes.txt", "", ava)
	if w.Code != 200 || w.Body.String() != "hello" || w.Header().Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") {
		t.Errorf("download: %d %v %q", w.Code, w.Header(), w.Body)
	}
	if w := call(h, "POST", "/api/servers/ava/files/delete", `{"path":"plugins"}`, ava); w.Code != 403 {
		t.Errorf("a kid deleting the plugins folder: %d", w.Code)
	}
	if w := call(h, "POST", "/api/servers/ava/files/rename", `{"from":"notes.txt","to":"notes2.txt"}`, ava); w.Code != 200 {
		t.Errorf("rename: %d %s", w.Code, w.Body)
	}
	if w := call(h, "POST", "/api/servers/ava/files/delete", `{"path":"notes2.txt"}`, ava); w.Code != 200 {
		t.Errorf("delete: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(dir, "servers", "ava", "notes2.txt")); err == nil {
		t.Error("the file should be gone")
	}
	if !strings.Contains(call(h, "GET", "/api/activity?server=ava", "", owner).Body.String(), "deleted notes2.txt") {
		t.Error("file changes go in the activity log")
	}
}
