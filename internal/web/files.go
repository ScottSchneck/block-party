package web

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/ScottSchneck/block-party/internal/servers"
)

// The file manager: anyone who takes care of a server. What they may change
// is limited in package servers (code is the owner's).
func (s *Server) fileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/servers/{id}/files", s.withServer(levelCare, s.listFiles))
	mux.HandleFunc("GET /api/servers/{id}/files/text", s.withServer(levelCare, s.readFileText))
	mux.HandleFunc("PUT /api/servers/{id}/files/text", s.withServer(levelCare, s.writeFileText))
	mux.HandleFunc("GET /api/servers/{id}/files/download", s.withServer(levelCare, s.downloadFile))
	mux.HandleFunc("POST /api/servers/{id}/files/upload", s.withServer(levelCare, s.uploadFiles))
	mux.HandleFunc("POST /api/servers/{id}/files/folder", s.withServer(levelCare, s.makeFolder))
	mux.HandleFunc("POST /api/servers/{id}/files/rename", s.withServer(levelCare, s.renameFile))
	mux.HandleFunc("POST /api/servers/{id}/files/delete", s.withServer(levelCare, s.deleteFile))
}

// fileError picks the status for a file manager error.
func fileError(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	switch {
	case errors.Is(err, servers.ErrFileNotFound):
		code = http.StatusNotFound
	case errors.Is(err, servers.ErrFileNotAllowed):
		code = http.StatusForbidden
	case errors.Is(err, servers.ErrFileChanged), errors.Is(err, servers.ErrImporting):
		code = http.StatusConflict
	}
	writeError(w, code, err)
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	entries, err := srv.ListFiles(r.URL.Query().Get("path"))
	if err != nil {
		fileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": r.URL.Query().Get("path"), "entries": entries, "owner": me(r).Owner})
}

func (s *Server) readFileText(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	ft, err := srv.ReadText(r.URL.Query().Get("path"))
	if err != nil {
		fileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ft)
}

func (s *Server) writeFileText(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	var req struct {
		Path    string    `json:"path"`
		Text    string    `json:"text"`
		ModTime time.Time `json:"modTime"`
	}
	if !readBodyLimit(w, r, &req, 3<<20) {
		return
	}
	if err := srv.WriteText(req.Path, req.Text, req.ModTime, me(r).Owner); err != nil {
		fileError(w, err)
		return
	}
	s.note(r, srv, "edited the file "+req.Path)
	ft, err := srv.ReadText(req.Path)
	if err != nil {
		fileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ft)
}

func (s *Server) downloadFile(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	f, st, err := srv.OpenDownload(r.URL.Query().Get("path"))
	if err != nil {
		fileError(w, err)
		return
	}
	defer f.Close()
	// Always a download, never shown in the panel's page.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(st.Name())}))
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}

// uploadFiles takes one file as the request body, named by X-File-Name (as
// the add-on and world uploads do).
func (s *Server) uploadFiles(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	dir := r.URL.Query().Get("path")
	name, err := url.QueryUnescape(r.Header.Get("X-File-Name"))
	if err != nil || strings.TrimSpace(name) == "" {
		writeError(w, http.StatusBadRequest, errors.New("the file has no name"))
		return
	}
	body := http.MaxBytesReader(w, r.Body, servers.MaxUploadBytes+1)
	if err := srv.SaveUpload(dir, name, body, me(r).Owner); err != nil {
		fileError(w, fmt.Errorf("%s: %w", name, err))
		return
	}
	s.note(r, srv, "uploaded "+name+" to "+displayDir(dir))
	writeJSON(w, http.StatusOK, map[string]any{"saved": name})
}

func displayDir(dir string) string {
	if dir == "" || dir == "/" {
		return "the server's folder"
	}
	return dir
}

func (s *Server) makeFolder(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	var req struct {
		Path string `json:"path"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if err := srv.MakeFolder(req.Path, me(r).Owner); err != nil {
		fileError(w, err)
		return
	}
	s.note(r, srv, "made the folder "+req.Path)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) renameFile(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	var req struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if err := srv.RenameFile(req.From, req.To, me(r).Owner); err != nil {
		fileError(w, err)
		return
	}
	s.note(r, srv, "renamed "+req.From+" to "+req.To)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	var req struct {
		Path string `json:"path"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if err := srv.DeleteFile(req.Path, me(r).Owner); err != nil {
		fileError(w, err)
		return
	}
	s.note(r, srv, "deleted "+req.Path)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
