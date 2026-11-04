package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ScottSchneck/block-party/internal/servers"
)

func (s *Server) javaTypeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/java/versions", s.javaVersions)
	mux.HandleFunc("GET /api/modpacks", ownerOnly(s.searchModpacks))
	mux.HandleFunc("POST /api/modpacks/upload", ownerOnly(s.uploadModpack))
}

// javaVersions lists the Minecraft versions a Java server type can run.
func (s *Server) javaVersions(w http.ResponseWriter, r *http.Request) {
	if p := me(r); !p.Owner && !p.CanAdd {
		writeError(w, http.StatusForbidden, errors.New("your account can't add servers"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	list, err := s.mgr.JavaVersions(ctx, r.URL.Query().Get("loader"))
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": list})
}

func (s *Server) searchModpacks(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	list, err := s.mgr.SearchModpacks(ctx, r.URL.Query().Get("q"))
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": list})
}

// uploadModpack takes a .mrpack as the request body and adds a server made
// from it (?name=…&memoryMB=…).
func (s *Server) uploadModpack(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	mem, _ := strconv.Atoi(r.URL.Query().Get("memoryMB"))
	file, _ := url.QueryUnescape(r.Header.Get("X-File-Name"))
	if !strings.HasSuffix(strings.ToLower(file), ".mrpack") {
		writeError(w, http.StatusBadRequest, errors.New("choose a Modrinth modpack (.mrpack); CurseForge packs aren't supported yet"))
		return
	}
	f, err := os.CreateTemp(s.mgr.UploadsDir(), ".upload-*.mrpack")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	path := f.Name()
	_, err = f.ReadFrom(http.MaxBytesReader(w, r.Body, 512<<20))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		writeError(w, http.StatusBadRequest, fmt.Errorf("the upload didn't finish: %w", err))
		return
	}
	st, err := s.mgr.CreateModpack(name, mem, servers.ModpackSource{File: filepath.Clean(path)})
	if err != nil {
		os.Remove(path)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.finishCreate(w, r, me(r), st)
}
