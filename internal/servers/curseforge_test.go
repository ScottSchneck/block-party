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
					// Like CurseForge: the newest files, one entry per game version.
					var idx []map[string]any
					for _, fl := range modFiles[m["id"].(int)] {
						if fl["isServerPack"] == true {
							continue
						}
						for _, v := range fl["gameVersions"].([]string) {
							e := map[string]any{"gameVersion": v, "fileId": fl["id"], "releaseType": fl["releaseType"]}
							if l, ok := fl["modLoader"]; ok {
								e["modLoader"] = l
							}
							idx = append(idx, e)
						}
					}
					c := map[string]any{"latestFilesIndexes": idx}
					for k, v := range m {
						c[k] = v
					}
					out = append(out, c)
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

	// Bedrock: an add-on, one the creator keeps to CurseForge, a world, one
	// with a file for each Minecraft version, and one only for a newer
	// Minecraft than the test server's 1.26.52.
	bedrock := func(v ...string) map[string]any { return map[string]any{"gameVersions": v} }
	addMod(100, 1, "Cool Mobs", true, file(1001, 100, "cool.mcpack", []byte(zipBytes(t, map[string]string{"manifest.json": resourceManifest})), bedrock("1.21.50", "26.0")))
	addMod(101, 1, "Kept Mobs", false, file(1011, 101, "kept.mcpack", []byte("x"), map[string]any{"downloadUrl": nil, "gameVersions": []string{"26.50"}}))
	addMod(102, 3, "Castle Map", true, file(1021, 102, "castle.mcworld", []byte(zipBytes(t, map[string]string{"Castle/level.dat": "c", "Castle/levelname.txt": "Big Castle"})), bedrock()))
	versioned := func(id int, v, when string) map[string]any {
		mf := strings.Replace(resourceManifest, `"version": "2.0.1"}`, `"version": "`+v+`.0"}`, 1)
		return file(id, 103, "mobs-"+v+".mcpack", []byte(zipBytes(t, map[string]string{"manifest.json": mf})), map[string]any{"gameVersions": []string{v}, "fileDate": when})
	}
	addMod(103, 1, "Version Mobs", true, versioned(1031, "26.60", "2026-10-20T00:00:00Z"), versioned(1032, "26.50", "2026-09-20T00:00:00Z"), versioned(1033, "26.40", "2026-08-20T00:00:00Z"))
	addMod(104, 1, "Future Mobs", true, file(1041, 104, "future.mcpack", []byte(zipBytes(t, map[string]string{"manifest.json": resourceManifest})), bedrock("26.60", "Bedrock")))

	// Java: a modpack's mods (one for the server, a texture pack, a
	// client-only mod, one kept to CurseForge), and two packs.
	addMod(300, 6, "Server Mod", true, file(3001, 300, "servermod.jar", []byte("server mod"), nil))
	addMod(301, 12, "Shiny Textures", true, file(3011, 301, "shiny.zip", []byte("textures"), nil))
	addMod(302, 6, "Minimap", true, file(3021, 302, "minimap.jar", []byte("minimap"), map[string]any{"gameVersions": []string{"1.21.4", "Client"}}))
	addMod(303, 6, "Kept Mod", false, file(3031, 303, "kept.jar", []byte("kept"), map[string]any{"downloadUrl": nil}))
	// For the Mods tab: a mod with a Fabric file and a newer Forge one.
	addMod(304, 6, "Pipes", true,
		file(3041, 304, "pipes-fabric.jar", modJar("fabric", "pipes", "Pipes", "2.0"), map[string]any{"gameVersions": []string{"1.21.4", "Fabric", "Server"}}),
		file(3042, 304, "pipes-forge.jar", modJar("forge", "pipes", "Pipes", "2.1"), map[string]any{"gameVersions": []string{"1.21.4", "Forge"}, "fileDate": "2026-10-05T00:00:00Z"}))
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
	addMod(201, 4471, "Tidy Pack", true, file(2011, 201, "tidy.zip", []byte(zipBytes(t, map[string]string{"manifest.json": manifest("Tidy Pack")})), map[string]any{"serverPackFileId": 2012, "modLoader": cfFabric}), serverPack)
	addMod(202, 4471, "Quilted Pack", true, file(2021, 202, "quilted.zip", []byte("q"), map[string]any{"modLoader": cfQuilt}))
	addMod(203, 4471, "Kept Pack", false, file(2031, 203, "keptpack.zip", []byte("k"), map[string]any{"downloadUrl": nil}))
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
	mojang := newFakeMojang(t, "1.26.52.3")
	m := New(Options{DataDir: t.TempDir(), DownloadAPI: mojang.srv.URL + "/links", StopTimeout: time.Second, CurseForgeAPI: cf.srv.URL})
	if err := m.Load(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.StopAll)
	if _, _, err := m.SearchCurseForge(t.Context(), CFAddons, "", ""); !errors.Is(err, ErrNoCurseForgeKey) {
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
	s := createRunning(t, m, "Family")
	if v := s.Status().Version; v != "1.26.52.3" {
		t.Fatalf("server version %q", v)
	}

	// For this server: only what works on 1.26.52, and the file made for it.
	res, notes, err := m.SearchCurseForge(t.Context(), CFAddons, "mobs", s.Status().Version)
	if err != nil || len(res) != 2 || res[0].ID+res[1].ID != 203 {
		t.Fatalf("search: %+v %v", res, err)
	}
	for _, r := range res {
		if r.ID == 103 && (len(r.Versions) != 1 || r.Versions[0] != "26.50") {
			t.Errorf("version mobs is shown as made for %v", r.Versions)
		}
	}
	if j := strings.Join(notes, " "); !strings.Contains(j, "1 more found need a newer Minecraft than this server's 1.26.52.3") || !strings.Contains(j, "1 more found can only be downloaded from CurseForge's own site") {
		t.Errorf("notes: %q", notes)
	}
	// Not for a server: everything that can be downloaded.
	if res, _, err := m.SearchCurseForge(t.Context(), CFAddons, "mobs", ""); err != nil || len(res) != 3 {
		t.Errorf("search without a server: %+v %v", res, err)
	}

	name, packs, _, err := s.InstallCurseForge(t.Context(), 100)
	if err != nil || name != "Cool Mobs" || len(packs.Installed) != 1 {
		t.Fatalf("install: %s %+v %v", name, packs, err)
	}
	if _, packs, _, err := s.InstallCurseForge(t.Context(), 103); err != nil || len(packs.Installed) != 1 || packs.Installed[0].Version != "26.50.0" {
		t.Errorf("the file for this version: %+v %v", packs, err)
	}
	if _, _, _, err := s.InstallCurseForge(t.Context(), 104); err == nil || !strings.Contains(err.Error(), "needs Minecraft 26.60 or newer, and this server has 1.26.52.3") {
		t.Errorf("too new: %v", err)
	}

	// Updates: the pack knows where it came from; once a better file is out
	// (here: the record says an older one), Update installs it.
	pv, _ := s.Packs()
	var mobs Pack
	for _, p := range pv.Packs {
		if p.Version == "26.50.0" {
			mobs = p
		}
	}
	if mobs.CurseForge != 103 || mobs.Page == "" {
		t.Fatalf("the pack from CurseForge: %+v", mobs)
	}
	if u := s.PackUpdates(t.Context()); len(u) != 0 {
		t.Errorf("no update yet: %v", u)
	}
	s.mu.Lock()
	r := s.meta.CurseForgePacks[mobs.UUID]
	r.File = 1033
	s.meta.CurseForgePacks[mobs.UUID] = r
	s.mu.Unlock()
	if u := s.PackUpdates(t.Context()); u[mobs.UUID] != "mobs-26.50" {
		t.Fatalf("update: %v", u)
	}
	if _, res, err := s.UpdateCurseForgePack(t.Context(), mobs.UUID); err != nil || len(res.Installed) != 1 {
		t.Fatalf("updating: %+v %v", res, err)
	}
	if r := s.cfPackRecords()[mobs.UUID]; r.File != 1032 {
		t.Errorf("after the update: %+v", r)
	}
	if _, _, err := s.UpdateCurseForgePack(t.Context(), "66666666-0000-0000-0000-000000000000"); err == nil {
		t.Error("a pack not from CurseForge")
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
	// Search: not the Quilt pack or the one kept to CurseForge.
	res, notes, err := m.SearchCurseForge(t.Context(), CFModpacks, "pack", "")
	if err != nil || len(res) != 1 || res[0].Name != "Tidy Pack" {
		t.Errorf("modpack search: %+v %v", res, err)
	}
	if j := strings.Join(notes, " "); !strings.Contains(j, "1 more found are for Quilt") || !strings.Contains(j, "1 more found can only be downloaded") {
		t.Errorf("modpack notes: %q", notes)
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

func TestCurseForgeVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"26.60", "1.26.60.1", 0}, {"26.50", "1.26.52.3", -1}, {"1.21.50", "26.10", -1}, {"26.0.02", "1.26.0", 0}, {"1.21.130", "1.21.50", 1},
	} {
		if got := cmpVer(bedrockVer(c.a), bedrockVer(c.b)); got != c.want {
			t.Errorf("%s vs %s: %d", c.a, c.b, got)
		}
	}
	for _, v := range []string{"Bedrock", "", "0.1", "26.x"} {
		if bedrockVer(v) != nil {
			t.Errorf("%q isn't a version", v)
		}
	}
	server := bedrockVer("1.26.52.3")
	cands := append(cfCandidates(1, []string{"26.60"}, true, "c"), cfCandidates(2, []string{"26.50", "26.40"}, false, "b")...)
	cands = append(cands, cfCandidates(3, []string{"26.50"}, true, "a")...)
	if best, ok, _ := cfPick(cands, server); !ok || best.file != 3 {
		t.Errorf("a release for 26.50 over a beta: %+v", best)
	}
	if best, ok, _ := cfPick(cands, nil); !ok || best.file != 1 {
		t.Errorf("no server version, the newest: %+v", best)
	}
	if _, ok, needs := cfPick(cfCandidates(1, []string{"26.70", "26.60"}, true, ""), server); ok || needs != "26.60" {
		t.Errorf("too new: %v %q", ok, needs)
	}
	if best, ok, _ := cfPick(cfCandidates(4, []string{"Bedrock"}, true, ""), server); !ok || best.file != 4 {
		t.Errorf("a file that doesn't say fits: %v", ok)
	}
}
