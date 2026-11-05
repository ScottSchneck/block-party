package web

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/ScottSchneck/block-party/internal/servers"
)

// Java plugins (Paper, Purpur) and mods (Fabric, Forge, NeoForge), under
// the same addresses: the server's type decides which. They run their own
// code on the computer, so only the owner searches for, installs, updates
// and removes them; someone who takes care of a server sees them and can
// turn them off and on.

func (s *Server) pluginRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/servers/{id}/plugins", s.withServer(levelCare, s.listPlugins))
	mux.HandleFunc("GET /api/servers/{id}/plugins/updates", s.withServer(levelCare, s.pluginUpdates))
	mux.HandleFunc("GET /api/servers/{id}/plugins/search", s.withServer(levelOwner, s.searchPlugins))
	mux.HandleFunc("POST /api/servers/{id}/plugins", s.withServer(levelOwner, s.installPlugin))
	mux.HandleFunc("POST /api/servers/{id}/plugins/{file}/update", s.withServer(levelOwner, s.updatePlugin))
	mux.HandleFunc("POST /api/servers/{id}/plugins/{file}/enabled", s.withServer(levelCare, s.setPluginEnabled))
	mux.HandleFunc("DELETE /api/servers/{id}/plugins/{file}", s.withServer(levelOwner, s.removePlugin))
}

func (s *Server) listPlugins(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	v, err := srv.Plugins()
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) pluginUpdates(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, srv.PluginUpdates(ctx))
}

func (s *Server) searchPlugins(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	st := srv.Status()
	if st.Type != servers.TypeJava {
		writeError(w, http.StatusBadRequest, errors.New("plugins are for Java servers"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var results []servers.PluginResult
	var notes []string
	if st.Loader == servers.LoaderFabric || st.Loader == servers.LoaderForge || st.Loader == servers.LoaderNeoForge {
		results, notes = s.mgr.SearchMods(ctx, r.URL.Query().Get("q"), st.Version, st.Loader)
	} else {
		results, notes = s.mgr.SearchPlugins(ctx, r.URL.Query().Get("q"), st.Version)
	}
	if notes == nil {
		notes = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "notes": notes})
}

func pluginError(w http.ResponseWriter, err error) {
	writeError(w, http.StatusConflict, err)
}

func (s *Server) installPlugin(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	var req struct {
		Source string `json:"source"`
		ID     string `json:"id"`
	}
	if !readBody(w, r, &req) {
		return
	}
	title, err := srv.InstallPlugin(req.Source, req.ID)
	if err != nil {
		pluginError(w, err)
		return
	}
	s.note(r, srv, "installed the "+jarWord(srv)+" "+title)
	s.listPlugins(w, r, srv)
}

func (s *Server) updatePlugin(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	file := r.PathValue("file")
	version, err := srv.UpdatePlugin(file)
	if err != nil {
		pluginError(w, err)
		return
	}
	s.note(r, srv, "updated the "+jarWord(srv)+" "+file+" to "+version)
	s.listPlugins(w, r, srv)
}

func (s *Server) setPluginEnabled(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if !readBody(w, r, &req) {
		return
	}
	file := r.PathValue("file")
	if err := srv.SetPluginEnabled(file, req.Enabled); err != nil {
		pluginError(w, err)
		return
	}
	if req.Enabled {
		s.note(r, srv, "turned on the "+jarWord(srv)+" "+file)
	} else {
		s.note(r, srv, "turned off the "+jarWord(srv)+" "+file)
	}
	s.listPlugins(w, r, srv)
}

func (s *Server) removePlugin(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	file := r.PathValue("file")
	if err := srv.RemovePlugin(file); err != nil {
		pluginError(w, err)
		return
	}
	s.note(r, srv, "removed the "+jarWord(srv)+" "+file)
	s.listPlugins(w, r, srv)
}

// jarWord is what a server's jars are called, for the activity list.
func jarWord(srv *servers.Server) string {
	switch srv.Status().Loader {
	case servers.LoaderFabric, servers.LoaderForge, servers.LoaderNeoForge:
		return "mod"
	}
	return "plugin"
}
