package servers

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
)

// Mods on Fabric, Forge and NeoForge servers: the Mods tab, which is the
// Plugins tab's code working in mods/ (see plugins.go). Each mod says who it
// is in its own file: fabric.mod.json for Fabric, META-INF/mods.toml for
// Forge, META-INF/neoforge.mods.toml for NeoForge.

// notNeeded are "mods" every server has: the game, the loader and Java.
var notNeeded = map[string]bool{
	"minecraft": true, "java": true, "fabricloader": true, "fabric-loader": true,
	"forge": true, "neoforge": true, "fml": true, "javafml": true, "quilt_loader": true,
}

// modMeta reads a mod's ID, name, version and the mods it needs.
func modMeta(path string) (id, name, version string, depends []string, err error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return "", "", "", nil, errors.New("it isn't a mod (not a .jar file)")
	}
	defer r.Close()
	files := map[string]*zip.File{}
	for _, f := range r.File {
		files[f.Name] = f
	}
	read := func(name string) []byte {
		f := files[name]
		if f == nil {
			return nil
		}
		rc, err := f.Open()
		if err != nil {
			return nil
		}
		defer rc.Close()
		b, _ := io.ReadAll(io.LimitReader(rc, 1<<20))
		return b
	}
	if b := read("fabric.mod.json"); b != nil {
		id, name, version, depends, err = fabricModMeta(b)
	} else if b := read("META-INF/neoforge.mods.toml"); b != nil {
		id, name, version, depends = tomlModMeta(b)
	} else if b := read("META-INF/mods.toml"); b != nil {
		id, name, version, depends = tomlModMeta(b)
	} else if read("quilt.mod.json") != nil {
		return "", "", "", nil, errors.New("it's a Quilt mod, which the panel doesn't run")
	} else {
		return "", "", "", nil, errors.New("it isn't a mod (no fabric.mod.json or mods.toml)")
	}
	if err != nil {
		return "", "", "", nil, err
	}
	if id == "" {
		return "", "", "", nil, errors.New("its mod file has no ID")
	}
	if strings.HasPrefix(version, "${") {
		version = manifestVersion(read("META-INF/MANIFEST.MF"))
	}
	if name == "" {
		name = id
	}
	return id, name, version, depends, nil
}

func fabricModMeta(b []byte) (id, name, version string, depends []string, err error) {
	var m struct {
		ID      string          `json:"id"`
		Name    string          `json:"name"`
		Version string          `json:"version"`
		Depends map[string]any  `json:"depends"`
		Env     json.RawMessage `json:"environment"`
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	if err := json.Unmarshal(b, &m); err != nil {
		if err2 := json.Unmarshal(looseJSON(b), &m); err2 != nil {
			return "", "", "", nil, errors.New("its fabric.mod.json can't be read")
		}
	}
	for d := range m.Depends {
		d = strings.ToLower(d)
		if d == "fabric" {
			d = "fabric-api" // its old name
		}
		if !notNeeded[d] {
			depends = append(depends, d)
		}
	}
	return m.ID, m.Name, m.Version, sortedUnique(depends), nil
}

// tomlModMeta reads the first mod in a mods.toml, and the mods it must
// have on the server. It reads just the keys it needs, line by line.
func tomlModMeta(b []byte) (id, name, version string, depends []string) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	section, mods := "", 0
	var dep struct {
		id, side, kind string
		mandatory      bool
		open           bool
	}
	flush := func() {
		if !dep.open {
			return
		}
		need := dep.mandatory || strings.EqualFold(dep.kind, "required")
		d := strings.ToLower(dep.id)
		if need && d != "" && !notNeeded[d] && !strings.EqualFold(dep.side, "CLIENT") {
			depends = append(depends, d)
		}
		dep.id, dep.side, dep.kind, dep.mandatory, dep.open = "", "", "", false, false
	}
	multi := "" // inside a multi-line string, its closing quotes
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if multi != "" {
			if strings.Contains(line, multi) {
				multi = ""
			}
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			flush()
			section = strings.TrimSpace(strings.Trim(strings.SplitN(line, "#", 2)[0], "[] \t"))
			if strings.HasPrefix(line, "[[") {
				if section == "mods" {
					mods++
				}
				if strings.HasPrefix(section, "dependencies.") {
					dep.open = true
				}
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		for _, q := range []string{`"""`, `'''`} {
			if strings.HasPrefix(v, q) {
				if !strings.Contains(v[3:], q) {
					multi = q
				}
				v = ""
			}
		}
		v = tomlValue(v)
		switch {
		case section == "mods" && mods == 1:
			switch k {
			case "modId":
				id = v
			case "version":
				version = v
			case "displayName":
				name = v
			}
		case dep.open:
			switch k {
			case "modId":
				dep.id = v
			case "mandatory":
				dep.mandatory = v == "true"
			case "type":
				dep.kind = v
			case "side":
				dep.side = v
			}
		}
	}
	flush()
	return id, name, version, sortedUnique(depends)
}

// tomlValue is a simple TOML value: a quoted string, or a bare word, with
// any comment after it left off.
func tomlValue(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
		if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
			return v[1 : 1+end]
		}
	}
	v, _, _ = strings.Cut(v, "#")
	return strings.TrimSpace(v)
}

// manifestVersion is a jar's Implementation-Version (what Forge's
// ${file.jarVersion} means).
func manifestVersion(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "Implementation-Version" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func sortedUnique(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range list {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
