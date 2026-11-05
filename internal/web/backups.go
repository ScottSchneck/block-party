package web

import (
	"net/http"
	"os"

	"github.com/ScottSchneck/block-party/internal/servers"
)

func (s *Server) backupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/servers/{id}/backups", s.withServer(levelRun, s.listBackups))
	mux.HandleFunc("POST /api/servers/{id}/backups", s.withServer(levelRun, s.makeBackup))
	mux.HandleFunc("PUT /api/servers/{id}/backups/plan", s.withServer(levelCare, s.setBackupPlan))
	mux.HandleFunc("GET /api/servers/{id}/backups/{name}", s.withServer(levelCare, s.downloadBackup))
	mux.HandleFunc("DELETE /api/servers/{id}/backups/{name}", s.withServer(levelCare, s.deleteBackup))
	mux.HandleFunc("POST /api/servers/{id}/backups/{name}/restore", s.withServer(levelCare, s.restoreBackup))
	mux.HandleFunc("GET /api/servers/{id}/backups/{name}/worlds", s.withServer(levelCare, s.backupWorlds))
	mux.HandleFunc("POST /api/servers/{id}/backups/{name}/worlds/{world}/restore", s.withServer(levelCare, s.restoreWorld))
}

func (s *Server) listBackups(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	writeJSON(w, http.StatusOK, srv.Backups())
}

func (s *Server) makeBackup(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	if err := s.did(r, srv, srv.StartBackup(), "made a backup"); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, srv.Backups())
}

func (s *Server) setBackupPlan(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	var p servers.BackupPlan
	if !readBody(w, r, &p) {
		return
	}
	if err := s.did(r, srv, srv.SetPlan(p), "changed the backup schedule"); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, srv.Backups())
}

func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	path, err := srv.BackupPath(r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	name := r.PathValue("name") // checked by BackupPath: letters, digits and - . only
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	if err := s.did(r, srv, srv.DeleteBackup(r.PathValue("name")), "deleted the backup "+r.PathValue("name")); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, srv.Backups())
}

func (s *Server) restoreBackup(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	if err := s.did(r, srv, srv.Restore(r.PathValue("name")), "restored the backup "+r.PathValue("name")); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, srv.Status())
}

// backupWorlds lists the worlds in a backup, to put one back.
func (s *Server) backupWorlds(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	worlds, err := srv.BackupWorlds(r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"worlds": worlds})
}

func (s *Server) restoreWorld(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	name, world := r.PathValue("name"), r.PathValue("world")
	if err := s.did(r, srv, srv.RestoreWorld(name, world), "put back the world "+world+" from the backup "+name); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, srv.Status())
}
