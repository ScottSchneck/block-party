package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ScottSchneck/block-party/internal/servers"
)

// Play time: server hours, kids' limits, and who played (owner only, except
// a server's own week and a kid's own time left).

func (s *Server) playtimeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/playtime", ownerOnly(s.getPlaytime))
	mux.HandleFunc("POST /api/playtime/kids", ownerOnly(s.saveKid))
	mux.HandleFunc("POST /api/playtime/kids/delete", ownerOnly(s.deleteKid))
	mux.HandleFunc("POST /api/playtime/kids/extra", ownerOnly(s.extraTime))
	mux.HandleFunc("GET /api/me/playtime", s.myPlaytime)
	mux.HandleFunc("POST /api/servers/{id}/hours", s.withServer(levelOwner, s.setHours))
	mux.HandleFunc("POST /api/servers/{id}/open-late", s.withServer(levelOwner, s.openLate))
	mux.HandleFunc("GET /api/servers/{id}/played", s.withServer(levelRun, s.played))
}

func (s *Server) getPlaytime(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.PlayTime())
}

func (s *Server) saveKid(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Old string      `json:"old"`
		Kid servers.Kid `json:"kid"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if err := s.mgr.SaveKid(req.Old, req.Kid); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Old == "" {
		s.note(r, nil, "added "+strings.TrimSpace(req.Kid.Name)+" to Play time")
	} else {
		s.note(r, nil, "changed "+strings.TrimSpace(req.Kid.Name)+"'s play time")
	}
	writeJSON(w, http.StatusOK, s.mgr.PlayTime())
}

func (s *Server) deleteKid(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if err := s.mgr.DeleteKid(req.Name); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.note(r, nil, "took "+req.Name+" off Play time")
	writeJSON(w, http.StatusOK, s.mgr.PlayTime())
}

func (s *Server) extraTime(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		Minutes int    `json:"minutes"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if err := s.mgr.GiveExtraTime(req.Name, req.Minutes); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Minutes > 0 {
		s.note(r, nil, "gave "+req.Name+" "+minutesText(req.Minutes)+" more to play today")
	} else {
		s.note(r, nil, "took back "+req.Name+"'s extra time today")
	}
	writeJSON(w, http.StatusOK, s.mgr.PlayTime())
}

func minutesText(n int) string {
	if n%60 == 0 {
		if n == 60 {
			return "an hour"
		}
		return strconv.Itoa(n/60) + " hours"
	}
	return strconv.Itoa(n) + " minutes"
}

// myPlaytime is a kid's own time today, when the owner has put an account
// of the same name on Play time.
func (s *Server) myPlaytime(w http.ResponseWriter, r *http.Request) {
	p := me(r)
	if p == nil {
		writeError(w, http.StatusUnauthorized, errors.New("sign in first"))
		return
	}
	if p.Owner {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	for _, k := range s.mgr.PlayTime().Kids {
		if strings.EqualFold(k.Name, p.Name) {
			writeJSON(w, http.StatusOK, map[string]any{"today": k.Today, "left": k.Left, "why": k.Why, "hours": k.Hours, "limit": k.Limit})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) setHours(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	var h servers.Hours
	if !readBody(w, r, &h) {
		return
	}
	if err := srv.SetHours(h); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if h.On {
		st := srv.HoursNow()
		s.note(r, srv, "set its hours: "+st.School.From+" to "+st.School.Until+" Sunday to Thursday, "+st.Weekend.From+" to "+st.Weekend.Until+" Friday and Saturday")
	} else {
		s.note(r, srv, "turned its hours off (open any time)")
	}
	writeJSON(w, http.StatusOK, srv.Status())
}

// openLate keeps a server open past its hours tonight: until "22:30" (today,
// or after midnight tomorrow), or "" to end that.
func (s *Server) openLate(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	var req struct {
		Until string `json:"until"`
	}
	if !readBody(w, r, &req) {
		return
	}
	var t time.Time
	if req.Until != "" {
		hm, err := time.ParseInLocation("15:04", req.Until, time.Local)
		if err != nil {
			writeError(w, http.StatusBadRequest, errors.New("pick a time like 22:30"))
			return
		}
		now := time.Now()
		t = time.Date(now.Year(), now.Month(), now.Day(), hm.Hour(), hm.Minute(), 0, 0, time.Local)
		if !t.After(now) {
			t = t.AddDate(0, 0, 1) // after midnight
		}
	}
	if err := srv.KeepOpenUntil(t); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if t.IsZero() {
		s.note(r, srv, "ended its late night (back to its usual hours)")
	} else {
		s.note(r, srv, "kept it open until "+strings.ToLower(t.Format("3:04 pm")))
	}
	writeJSON(w, http.StatusOK, srv.Status())
}

func (s *Server) played(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	writeJSON(w, http.StatusOK, srv.PlayedThisWeek())
}
