package web

import (
	"net/http"

	"github.com/ScottSchneck/block-party/internal/servers"
)

// Scheduled tasks: anyone who takes care of a server.
func (s *Server) taskRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/servers/{id}/tasks", s.withServer(levelCare, s.listTasks))
	mux.HandleFunc("POST /api/servers/{id}/tasks", s.withServer(levelCare, s.saveTask))
	mux.HandleFunc("DELETE /api/servers/{id}/tasks/{task}", s.withServer(levelCare, s.deleteTask))
	mux.HandleFunc("POST /api/servers/{id}/tasks/{task}/run", s.withServer(levelCare, s.runTask))
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	writeJSON(w, http.StatusOK, map[string]any{"tasks": srv.Tasks()})
}

func (s *Server) saveTask(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	var t servers.Task
	if !readBody(w, r, &t) {
		return
	}
	isNew := t.ID == ""
	saved, err := srv.SaveTask(t)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if isNew {
		s.note(r, srv, "added the scheduled task "+saved.Name)
	} else {
		s.note(r, srv, "changed the scheduled task "+saved.Name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": srv.Tasks()})
}

func (s *Server) deleteTask(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	name := taskName(srv, r.PathValue("task"))
	if err := s.did(r, srv, srv.DeleteTask(r.PathValue("task")), "removed the scheduled task "+name); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": srv.Tasks()})
}

func (s *Server) runTask(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	name := taskName(srv, r.PathValue("task"))
	if err := s.did(r, srv, srv.RunTaskNow(r.PathValue("task")), "ran the scheduled task "+name+" now"); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"tasks": srv.Tasks()})
}

func taskName(srv *servers.Server, id string) string {
	for _, t := range srv.Tasks() {
		if t.ID == id {
			return t.Name
		}
	}
	return id
}
