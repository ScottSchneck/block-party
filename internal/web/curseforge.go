package web

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/ScottSchneck/block-party/internal/servers"
)

func (s *Server) curseForgeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/curseforge", ownerOnly(s.getCurseForge))
	mux.HandleFunc("PUT /api/curseforge", ownerOnly(s.putCurseForge))
	mux.HandleFunc("GET /api/curseforge/search", s.searchCurseForge)
	mux.HandleFunc("POST /api/servers/{id}/curseforge", s.withServer(levelCare, s.installCurseForge))
	mux.HandleFunc("GET /api/servers/{id}/packs/updates", s.withServer(levelCare, s.packUpdates))
	mux.HandleFunc("POST /api/servers/{id}/packs/{uuid}/update", s.withServer(levelCare, s.updatePack))
}

func (s *Server) getCurseForge(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"set": s.mgr.CurseForgeKeySet()})
}

// putCurseForge saves the owner's CurseForge API key ("" removes it). It's
// checked with CurseForge first, and never sent back.
func (s *Server) putCurseForge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if !readBody(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.mgr.SetCurseForgeKey(ctx, req.Key); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Key == "" {
		s.note(r, nil, "removed the CurseForge key")
	} else {
		s.note(r, nil, "added a CurseForge key")
	}
	writeJSON(w, http.StatusOK, map[string]bool{"set": s.mgr.CurseForgeKeySet()})
}

// searchCurseForge: modpacks for the owner; Bedrock add-ons, texture packs
// and worlds for anyone who takes care of a server.
func (s *Server) searchCurseForge(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	p := me(r)
	forVersion := ""
	switch kind {
	case servers.CFModpacks:
		if !p.Owner {
			writeError(w, http.StatusForbidden, errors.New("only the owner can add modpack servers"))
			return
		}
	case servers.CFAddons, servers.CFTextures, servers.CFWorlds:
		// For a server: only what works on its version.
		if id := r.URL.Query().Get("server"); id != "" {
			srv, ok := s.mgr.Get(id)
			if !ok || p.Level(id) == 0 {
				writeError(w, http.StatusNotFound, errors.New("no such server"))
				return
			}
			if p.Level(id) < levelCare {
				writeError(w, http.StatusForbidden, errNotAllowed)
				return
			}
			forVersion = srv.Status().Version
			break
		}
		allowed := p.Owner
		for _, st := range s.mgr.List() {
			allowed = allowed || p.Level(st.ID) >= levelCare
		}
		if !allowed {
			writeError(w, http.StatusForbidden, errNotAllowed)
			return
		}
	default:
		writeError(w, http.StatusBadRequest, errors.New("search add-ons, texture-packs, worlds or modpacks"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, notes, err := s.mgr.SearchCurseForge(ctx, kind, r.URL.Query().Get("q"), forVersion)
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, servers.ErrNoCurseForgeKey) {
			code = http.StatusBadRequest
		}
		writeError(w, code, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": res, "notes": notes})
}

func (s *Server) installCurseForge(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	var req struct {
		ID int `json:"id"`
	}
	if !readBody(w, r, &req) || req.ID <= 0 {
		if req.ID <= 0 {
			writeError(w, http.StatusBadRequest, errors.New("choose something to install"))
		}
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	name, res, world, err := srv.InstallCurseForge(ctx, req.ID)
	for _, p := range res.Installed {
		s.note(r, srv, "installed the add-on "+p.Name+" "+p.Version+" from CurseForge")
	}
	if world.World.Folder != "" {
		s.note(r, srv, "added the world "+world.World.Folder+" from CurseForge")
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "packs": res, "world": world})
}

// packUpdates: the add-ons from CurseForge that have a newer file.
func (s *Server) packUpdates(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, srv.PackUpdates(ctx))
}

func (s *Server) updatePack(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	name, res, err := srv.UpdateCurseForgePack(ctx, r.PathValue("uuid"))
	for _, p := range res.Installed {
		s.note(r, srv, "updated the add-on "+p.Name+" to "+p.Version+" from CurseForge")
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "packs": res})
}
