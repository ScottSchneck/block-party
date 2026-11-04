package web

import (
	"net/http"
	"time"

	"github.com/ScottSchneck/block-party/internal/servers"
)

func (s *Server) metricsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/servers/{id}/metrics", s.withServer(levelRun, s.serverMetrics))
}

// metricRanges are the spans the graphs offer.
var metricRanges = map[string]time.Duration{"1h": time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour}

func (s *Server) serverMetrics(w http.ResponseWriter, r *http.Request, srv *servers.Server) {
	span, ok := metricRanges[r.URL.Query().Get("range")]
	if !ok {
		span = time.Hour
	}
	writeJSON(w, http.StatusOK, srv.Metrics(time.Now().Add(-span)))
}
