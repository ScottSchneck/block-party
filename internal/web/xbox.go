package web

import (
	"context"
	"net/http"
	"time"
)

// The panel's Xbox account, which friends' consoles join through from the
// Friends tab (owner only). See package xbox.

func (s *Server) xboxRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/xbox", ownerOnly(s.getXbox))
	mux.HandleFunc("POST /api/xbox/sign-in", ownerOnly(s.xboxSignIn))
	mux.HandleFunc("POST /api/xbox/cancel", ownerOnly(s.xboxCancel))
	mux.HandleFunc("POST /api/xbox/sign-out", ownerOnly(s.xboxSignOut))
}

func (s *Server) getXbox(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.xbox.Status())
}

func (s *Server) xboxSignIn(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	st, err := s.xbox.SignIn(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.note(r, nil, "started signing in an Xbox account for friends' consoles")
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) xboxCancel(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.xbox.CancelSignIn())
}

func (s *Server) xboxSignOut(w http.ResponseWriter, r *http.Request) {
	if err := s.xbox.SignOut(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.note(r, nil, "signed out the Xbox account for friends' consoles")
	writeJSON(w, http.StatusOK, s.xbox.Status())
}
