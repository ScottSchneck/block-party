package servers

import (
	"crypto/sha1"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCurseForge serves CurseForge's API and downloads.
type fakeCurseForge struct {
	srv   *httptest.Server
	files map[string][]byte // download path -> body
}

func newFakeCurseForge(t *testing.T) *fakeCurseForge {
	f := &fakeCurseForge{files: map[string][]byte{}}
	file := func(id, modID int, name string, body []byte, extra map[string]any) map[string]any {
		sum := sha1.Sum(body)
		p := "/dl/" + name
		f.files[p] = body
		m := map[string]any{"id": id, "modId": modID, "displayName": name, "fileName": name, "releaseType": 1, "fileDate": "2026-10-01T00:00:00Z",
			"downloadUrl": f.srv.URL + p, "gameVersions": []string{"1.21.4", "Server"}, "hashes": []map[string]any{{"value": hexOf(sum[:]), "algo": 1}}}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	mods := map[int]map[string]any{}
	modFiles := map[int][]map[string]any{}
	fileByID := map[int]map[string]any{}
	addMod := func(id, class int, name string, allow bool, files ...map[string]any) {
		mods[id] = map[string]any{"id": id, "name": name, "summary": name + "!", "classId": class, "downloadCount": 5,
			"allowModDistribution": allow, "links": map[string]string{"websiteUrl": "https://www.curseforge.com/x/" + name}, "authors": []map[string]string{{"name": "maker"}}}
		modFiles[id] = files
		for _, fl := range files {
			fileByID[fl["id"].(int)] = fl
		}
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/dl/") {
			w.Write(f.files[r.URL.Path])
			return
		}
		if r.Header.Get("x-api-key") != "good-key" {
			w.WriteHeader(403)
			return
		}
		enc := json.NewEncoder(w)
		q := r.URL.Query()
		p := r.URL.Path
		switch {
		case p == "/v1/games":
			enc.Encode(map[string]any{"data": []map[string]any{{"id": 432, "name": "Minecraft", "slug": "minecraft"}, {"id": 78022, "name": "Minecraft Bedrock", "slug": "minecraft-bedrock"}}})
		case p == "/v1/categories" && q.Get("gameId") == "78022":
			enc.Encode(map[string]any{"data": []map[string]any{{"id": 1, "slug": "addons", "isClass": true}, {"id": 2, "slug": "texture-packs", "isClass": true}, {"id": 3, "slug": "maps", "isClass": true}}})
		case p == "/v1/categories" && q.Get("gameId") == "432":
			enc.Encode(map[string]any{"data": []map[string]any{{"id": 4471, "slug": "modpacks", "isClass": true}, {"id": 6, "slug": "mc-mods", "isClass": true}, {"id": 12, "slug": "texture-packs", "isClass": true}}})
		case p == "/v1/mods/search":
			var out []map[string]any
			for _, m := range mods {
				if m["classId"] == mustAtoi(q.Get("classId")) && strings.Contains(strings.ToLower(m["name"].(string)), strings.ToLower(q.Get("searchFilter"))) {
					out = append(out, m)
				}
			}
			enc.Encode(map[string]any{"data": out})
		case p == "/v1/mods/files" && r.Method == "POST":
			var body struct {
				FileIDs []int `json:"fileIds"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			var out []map[string]any
			for _, id := range body.FileIDs {
				out = append(out, fileByID[id])
			}
			enc.Encode(map[string]any{"data": out})
		case p == "/v1/mods" && r.Method == "POST":
			var body struct {
				ModIDs []int `json:"modIds"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			var out []map[string]any
			for _, id := range body.ModIDs {
				out = append(out, mods[id])
			}
			enc.Encode(map[string]any{"data": out})
		case strings.HasPrefix(p, "/v1/mods/"):
			parts := strings.Split(strings.TrimPrefix(p, "/v1/mods/"), "/")
			id := mustAtoi(parts[0])
			switch {
			case len(parts) == 1:
				enc.Encode(map[string]any{"data": mods[id]})
			case len(parts) == 2:
				enc.Encode(map[string]any{"data": modFiles[id]})
			default:
				enc.Encode(map[string]any{"data": fileByID[mustAtoi(parts[2])]})
			}
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)

	// Bedrock: an add-on, one the creator keeps to CurseForge, and a world.
	addMod(100, 1, "Cool Mobs", true, file(1001, 100, "cool.mcpack", []byte(zipBytes(t, map[string]string{"manifest.json": resourceManifest})), nil))
	addMod(101, 1, "Kept Mobs", false, file(1011, 101, "kept.mcpack", []byte("x"), map[string]any{"downloadUrl": nil}))
	addMod(102, 3, "Castle Map", true, file(1021, 102, "castle.mcworld", []byte(zipBytes(t, map[string]string{"Castle/level.dat": "c", "Castle/levelname.txt": "Big Castle"})), nil))

	// Java: a modpack's mods (one for the server, a texture pack, a
	// client-only mod, one kept to CurseForge), and two packs.
	addMod(300, 6, "Server Mod", true, file(3001, 300, "servermod.jar", []byte("server mod"), nil))
	addMod(301, 12, "Shiny Textures", true, file(3011, 301, "shiny.zip", []byte("textures"), nil))
	addMod(302, 6, "Minimap", true, file(3021, 302, "minimap.jar", []byte("minimap"), map[string]any{"gameVersions": []string{"1.21.4", "Client"}}))
	addMod(303, 6, "Kept Mod", false, file(3031, 303, "kept.jar", []byte("kept"), map[string]any{"downloadUrl": nil}))
	manifest := func(name string) string {
		b, _ := json.Marshal(map[string]any{"manifestType": "minecraftModpack", "manifestVersion": 1, "name": name, "version": "2.0", "overrides": "overrides",
			"minecraft": map[string]any{"version": "1.21.4", "modLoaders": []map[string]any{{"id": "fabric-0.16.9", "primary": true}}},
			"files": []map[string]any{{"projectID": 300, "fileID": 3001, "required": true}, {"projectID": 301, "fileID": 3011, "required": true},
				{"projectID": 302, "fileID": 3021, "required": true}, {"projectID": 303, "fileID": 3031, "required": true}}})
		return string(b)
	}
	addMod(200, 4471, "Kitchen Sink", true, file(2001, 200, "sink.zip", []byte(zipBytes(t, map[string]string{"manifest.json": manifest("Kitchen Sink"), "overrides/config/sink.toml": "a = 1"})), nil))
	serverPack := file(2012, 201, "tidy-server.zip", []byte(zipBytes(t, map[string]string{
		"Tidy Server/mods/tidy.jar": "tidy", "Tidy Server/config/tidy.toml": "b = 2", "Tidy Server/start.sh": "evil",
		"Tidy Server/fabric-installer.jar": "installer", "Tidy Server/libraries/x.jar": "lib", "Tidy Server/server.properties": "server-port=1"})), map[string]any{"isServerPack": true})
	addMod(201, 4471, "Tidy Pack", true, file(2011, 201, "tidy.zip", []byte(zipBytes(t, map[string]string{"manifest.json": manifest("Tidy Pack")})), map[string]any{"serverPackFileId": 2012}), serverPack)
	return f
}

func mustAtoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func allowFakeCurseForge(t *testing.T, f *fakeCurseForge) {
	host := strings.TrimPrefix(f.srv.URL, "http://")
	host, _, _ = strings.Cut(host, ":")
	cfSchemes["http"], cfDownloadHosts[host] = true, true
	t.Cleanup(func() { delete(cfSchemes, "http"); delete(cfDownloadHosts, host) })
}

func TestCurseForgeBedrock(t *testing.T) {
	cf := newFakeCurseForge(t)
	allowFakeCurseForge(t, cf)
	mojang := newFakeMojang(t, "1.0.0.1")
	m := New(Options{DataDir: t.TempDir(), DownloadAPI: mojang.srv.URL + "/links", StopTimeout: time.Second, CurseForgeAPI: cf.srv.URL})
	if err := m.Load(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.StopAll)
	if _, err := m.SearchCurseForge(t.Context(), CFAddons, ""); !errors.Is(err, ErrNoCurseForgeKey) {
		t.Errorf("without a key: %v", err)
	}
	if err := m.SetCurseForgeKey(t.Context(), "bad-key"); err == nil || m.CurseForgeKeySet() {
		t.Error("a refused key must not be saved")
	}
	if err := m.SetCurseForgeKey(t.Context(), " good-key "); err != nil || !m.CurseForgeKeySet() {
		t.Fatalf("good key: %v", err)
	}
	if st, _ := os.Stat(m.curseForgeKeyPath()); st.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %v", st.Mode())
	}
	res, err := m.SearchCurseForge(t.Context(), CFAddons, "mobs")
	if err != nil || len(res) != 2 {
		t.Fatalf("search: %+v %v", res, err)
	}
	s := createRunning(t, m, "Family")
	name, packs, _, err := s.InstallCurseForge(t.Context(), 100)
	if err != nil || name != "Cool Mobs" || len(packs.Installed) != 1 {
		t.Fatalf("install: %s %+v %v", name, packs, err)
	}
	if _, _, _, err := s.InstallCurseForge(t.Context(), 101); err == nil || !strings.Contains(err.Error(), "only allows downloading it from CurseForge") {
		t.Errorf("a creator's choice is respected: %v", err)
	}
	_, _, world, err := s.InstallCurseForge(t.Context(), 102)
	if err != nil || world.World.Folder != "Big Castle" || world.Playing {
		t.Errorf("world: %+v %v", world, err)
	}
	if err := m.SetCurseForgeKey(t.Context(), ""); err != nil || m.CurseForgeKeySet() {
		t.Errorf("removing the key: %v", err)
	}
}

func TestCurseForgeModpacks(t *testing.T) {
	cf := newFakeCurseForge(t)
	allowFakeCurseForge(t, cf)
	java := newFakeJavaWorld(t)
	loaders := newFakeLoaders(t)
	u := loaders.srv.URL
	m := New(Options{DataDir: t.TempDir(), StopTimeout: time.Second,
		PaperAPI: java.srv.URL + "/paper", AdoptiumAPI: java.srv.URL + "/adoptium", MojangVersions: u + "/manifest", MojangProfiles: java.srv.URL + "/profiles/",
		GeyserAPI: u + "/geyser", HangarAPI: java.srv.URL + "/hangar", XboxLookup: java.srv.URL + "/xbox/", ModrinthAPI: u + "/modrinth",
		PurpurAPI: u + "/purpur", FabricMeta: u + "/fabric", ForgePromos: u + "/forge/promos.json", ForgeMaven: u + "/forgemaven", NeoForgeMaven: u + "/neo",
		CurseForgeAPI: cf.srv.URL})
	if err := m.Load(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.StopAll)
	if _, err := m.CreateModpack("x", 2048, ModpackSource{CurseForge: 200}); !errors.Is(err, ErrNoCurseForgeKey) {
		t.Errorf("without a key: %v", err)
	}
	if err := m.SetCurseForgeKey(t.Context(), "good-key"); err != nil {
		t.Fatal(err)
	}

	// Mods one by one: the server's mod only, the kept one reported.
	st, err := m.CreateModpack("Sink", 4096, ModpackSource{CurseForge: 200})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m.Get(st.ID)
	waitFor(t, s, "running", state(StateRunning))
	if st := s.Status(); st.Loader != LoaderFabric || st.Version != "1.21.4" || st.Modpack != "Kitchen Sink 2.0" {
		t.Errorf("status: %+v", st)
	}
	if readFile(t, filepath.Join(s.dir(), "mods", "servermod.jar")) != "server mod" || readFile(t, filepath.Join(s.dir(), "config", "sink.toml")) != "a = 1" {
		t.Error("the pack's mod and settings")
	}
	for _, left := range []string{"shiny.zip", "minimap.jar", "kept.jar"} {
		if fileExists(filepath.Join(s.dir(), "mods", left)) {
			t.Errorf("%s shouldn't be on the server", left)
		}
	}
	waitConsole(t, s, "1 mods couldn't be downloaded because their creators only allow downloads from CurseForge itself: Kept Mod")

	// A server pack: its files, not its scripts, jars or settings.
	st, err = m.CreateModpack("Tidy", 4096, ModpackSource{CurseForge: 201})
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := m.Get(st.ID)
	waitFor(t, s2, "running", state(StateRunning))
	if readFile(t, filepath.Join(s2.dir(), "mods", "tidy.jar")) != "tidy" || readFile(t, filepath.Join(s2.dir(), "config", "tidy.toml")) != "b = 2" {
		t.Error("the server pack's files")
	}
	for _, left := range []string{"start.sh", "fabric-installer.jar", "libraries/x.jar"} {
		if fileExists(filepath.Join(s2.dir(), left)) {
			t.Errorf("%s shouldn't be copied from the server pack", left)
		}
	}
	if strings.Contains(readFile(t, s2.propertiesPath()), "server-port=1\n") {
		t.Error("the server pack's server.properties replaced the panel's")
	}
	if fileExists(filepath.Join(s2.dir(), "mods", "servermod.jar")) {
		t.Error("with a server pack, the mods aren't fetched one by one")
	}
}
