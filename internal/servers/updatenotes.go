package servers

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/ScottSchneck/block-party/internal/notify"
)

// Update notes: once a day the panel looks for new versions of what it
// installed on each server (plugins and mods, add-ons from CurseForge, the
// modpack) and sends one notification per server about the ones it hasn't
// told about yet. The checks are the same as the tabs' (and share their
// caches), so opening a tab afterwards doesn't ask the sites again.

const updateNotesEvery = 24 * time.Hour

// RunUpdateNotes checks for updates a while after the panel starts, then
// once a day.
func (m *Manager) RunUpdateNotes() {
	if m.opts.Notify == nil {
		return
	}
	go func() {
		wait := 20 * time.Minute // after the servers have started
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-time.After(wait):
			}
			wait = updateNotesEvery
			for _, s := range m.all() {
				ctx, cancel := context.WithTimeout(m.ctx, 2*time.Minute)
				s.noteUpdates(ctx)
				cancel()
			}
		}
	}()
}

// noteUpdates tells the owner about new updates for a server, once each.
func (s *Server) noteUpdates(ctx context.Context) {
	s.mu.Lock()
	skip := s.deleted || s.state == StateImporting || s.state == StateRestoring
	s.mu.Unlock()
	if skip {
		return
	}
	var keys, found, where []string
	if s.isJava() {
		if v, err := s.Plugins(); err == nil {
			names := map[string]string{}
			for _, p := range v.Plugins {
				names[p.File] = firstNonEmpty(p.Title, p.Name)
			}
			ups := s.PluginUpdates(ctx)
			for file, version := range ups {
				keys = append(keys, "jar:"+file+"@"+version)
				found = append(found, names[file]+" "+version)
			}
			if len(ups) > 0 {
				tab := "Plugins"
				if v.Kind == "mods" {
					tab = "Mods"
				}
				where = append(where, "Update them on the "+tab+" tab.")
			}
		}
		if info := s.ModpackInfo(ctx); info.Newer != "" {
			keys = append(keys, "modpack@"+info.Newer)
			found = append(found, "the modpack ("+info.Newer+")")
			where = append(where, "Update modpack is on the Mods tab.")
		}
	}
	if s.isBedrock() {
		if v, err := s.Packs(); err == nil {
			ups := s.PackUpdates(ctx)
			for _, p := range v.Packs {
				if name, ok := ups[p.UUID]; ok {
					keys = append(keys, "pack:"+p.UUID+"@"+name)
					found = append(found, p.Name+" ("+name+")")
				}
			}
			if len(ups) > 0 {
				where = append(where, "Update them on the Add-ons tab.")
			}
		}
	}
	s.mu.Lock()
	told := map[string]bool{}
	for _, k := range s.meta.UpdatesNoted {
		told[k] = true
	}
	fresh := 0
	for _, k := range keys {
		if !told[k] {
			fresh++
		}
	}
	changed := strings.Join(s.meta.UpdatesNoted, "\n") != strings.Join(sortedUnique(keys), "\n")
	s.meta.UpdatesNoted = sortedUnique(keys) // what's out now (done ones drop off)
	name, id := s.meta.Name, s.meta.ID
	s.mu.Unlock()
	if changed {
		_ = s.saveMeta()
	}
	if fresh == 0 {
		return
	}
	sort.Strings(found)
	text := "New versions: " + joinNames(found) + ". " + strings.Join(where, " ")
	s.notify(notify.UpdatesOut, "Updates for "+name, strings.TrimSpace(text), "updates/"+id+"/"+strings.Join(sortedUnique(keys), ","))
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
