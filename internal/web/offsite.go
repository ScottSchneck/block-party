package web

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/ScottSchneck/block-party/internal/offsite"
)

// Off-site backups (owner only).

func (s *Server) offsiteRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/offsite", ownerOnly(s.getOffsite))
	mux.HandleFunc("POST /api/offsite/connect", ownerOnly(s.offsiteConnect))
	mux.HandleFunc("POST /api/offsite/sign-in", ownerOnly(s.offsiteSignIn))
	mux.HandleFunc("POST /api/offsite/answer", ownerOnly(s.offsiteAnswer))
	mux.HandleFunc("POST /api/offsite/options", ownerOnly(s.offsiteOptions))
	mux.HandleFunc("POST /api/offsite/copy-now", ownerOnly(s.offsiteCopyNow))
	mux.HandleFunc("POST /api/offsite/disconnect", ownerOnly(s.offsiteDisconnect))
	mux.HandleFunc("GET /api/offsite/copies", ownerOnly(s.offsiteCopies))
	mux.HandleFunc("POST /api/offsite/download", ownerOnly(s.offsiteDownload))
}

type offsiteView struct {
	Settings offsite.Settings   `json:"settings"`
	Status   offsite.Status     `json:"status"`
	Kinds    []offsite.KindInfo `json:"kinds"`
	Rclone   string             `json:"rclone"` // rclone's version, "" if it's missing
}

func (s *Server) offsiteView(ctx context.Context) offsiteView {
	return offsiteView{Settings: s.off.Settings(), Status: s.off.Status(), Kinds: offsite.Kinds, Rclone: s.off.Version(ctx)}
}

func (s *Server) getOffsite(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.offsiteView(r.Context()))
}

// connectStep answers a step of connecting, noting when it's done.
func (s *Server) connectStep(w http.ResponseWriter, r *http.Request, step offsite.Step, err error) {
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if step.Step == "done" {
		where := step.Settings.Kind
		for _, k := range offsite.Kinds {
			if k.Kind == where {
				where = k.Label
			}
		}
		s.note(r, nil, "set up off-site backups to "+where)
	}
	writeJSON(w, http.StatusOK, step)
}

func (s *Server) offsiteConnect(w http.ResponseWriter, r *http.Request) {
	var req offsite.ConnectRequest
	if !readBody(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	step, err := s.off.Connect(ctx, req)
	s.connectStep(w, r, step, err)
}

func (s *Server) offsiteSignIn(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Address string `json:"address"`
	}
	if !readBody(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	step, err := s.off.FinishSignIn(ctx, req.Address)
	s.connectStep(w, r, step, err)
}

func (s *Server) offsiteAnswer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Answer string `json:"answer"`
	}
	if !readBody(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	step, err := s.off.Answer(ctx, req.Answer)
	s.connectStep(w, r, step, err)
}

func (s *Server) offsiteOptions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		On   bool `json:"on"`
		Keep int  `json:"keep"`
	}
	if !readBody(w, r, &req) {
		return
	}
	before := s.off.Settings()
	if _, err := s.off.SetOptions(req.On, req.Keep); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if before.On != req.On {
		if req.On {
			s.note(r, nil, "turned off-site backups back on")
		} else {
			s.note(r, nil, "paused off-site backups")
		}
	}
	writeJSON(w, http.StatusOK, s.offsiteView(r.Context()))
}

func (s *Server) offsiteCopyNow(w http.ResponseWriter, r *http.Request) {
	s.off.Soon()
	writeJSON(w, http.StatusAccepted, s.offsiteView(r.Context()))
}

func (s *Server) offsiteDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := s.off.Disconnect(); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	s.note(r, nil, "disconnected off-site backups (the copies already made are kept there)")
	writeJSON(w, http.StatusOK, s.offsiteView(r.Context()))
}

func (s *Server) offsiteCopies(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	list, err := s.off.List(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) offsiteDownload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Server string `json:"server"`
		Name   string `json:"name"`
	}
	if !readBody(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Minute)
	defer cancel()
	if err := s.off.Download(ctx, req.Server, req.Name); err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, offsite.ErrNotACopy) {
			code = http.StatusBadRequest
		}
		writeError(w, code, err)
		return
	}
	s.note(r, nil, "downloaded the off-site backup "+req.Name)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
