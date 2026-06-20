package web

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"bitbucket.org/andrewburnsza/hinaess-battery-charge-monitor/internal/store"
)

// apiRoutes registers the read-only /api/* JSON endpoints on mux. Only GET is
// accepted; the Go 1.22+ method-prefixed patterns make any other method fall
// through to a 405 (FR-4). No CORS headers are set (same-origin LAN, Non-goal 1).
func (s *Server) apiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/latest", s.handleLatest)
	mux.HandleFunc("GET /api/range", s.handleRange)
	mux.HandleFunc("GET /api/health", s.handleHealth)
}

// writeJSON marshals v and writes it with the given status and a JSON content
// type. A marshal failure is a programmer error (the API's response types are
// fixed), so it surfaces as a 500 rather than a partial body.
func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeErr emits a short JSON error body with the given status (FR-6 400s).
func (s *Server) writeErr(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, map[string]string{"error": msg})
}

// handleLatest serves store.Latest as JSON (FR-5). On an empty DB it returns 200
// with empty cells_mv/temps arrays so the frontend's poll loop tolerates a
// just-started service without erroring.
func (s *Server) handleLatest(w http.ResponseWriter, r *http.Request) {
	ls, err := s.store.Latest(r.Context())
	if err != nil {
		s.log.Error("api: latest", "err", err)
		s.writeErr(w, http.StatusInternalServerError, "latest failed")
		return
	}
	if !ls.Found {
		// Found=false leaves Cells/Temps nil, which would marshal to null; the
		// frontend expects empty arrays (FR-5).
		ls.Cells = []int{}
		ls.Temps = []store.TempRow{}
	}
	s.writeJSON(w, http.StatusOK, ls)
}

// validFields is the set of series names accepted in /api/range's ?fields=.
var validFields = map[string]bool{"cells": true, "pack_mv": true, "pack_ma": true}

// handleRange parses the range query params, validates them (400 on bad input),
// and serves store.Range as column-oriented JSON aligned for uPlot (FR-6).
func (s *Server) handleRange(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	from, err := strconv.ParseInt(q.Get("from"), 10, 64)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "from must be an epoch-ms integer")
		return
	}
	to, err := strconv.ParseInt(q.Get("to"), 10, 64)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "to must be an epoch-ms integer")
		return
	}
	if from >= to {
		s.writeErr(w, http.StatusBadRequest, "from must be less than to")
		return
	}

	var fields []string
	if raw := q.Get("fields"); raw != "" {
		for _, f := range strings.Split(raw, ",") {
			f = strings.TrimSpace(f)
			if !validFields[f] {
				s.writeErr(w, http.StatusBadRequest, "fields must be a comma list of cells,pack_mv,pack_ma")
				return
			}
			fields = append(fields, f)
		}
	}

	res, err := s.store.Range(r.Context(), from, to, fields, q.Get("raw") == "1")
	if err != nil {
		s.log.Error("api: range", "err", err)
		s.writeErr(w, http.StatusInternalServerError, "range failed")
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

// healthResponse is /api/health's body. LastSampleAgeS is a pointer so it
// marshals to JSON null when the DB is empty (FR-7).
type healthResponse struct {
	Status         string `json:"status"`
	LastSampleAgeS *int64 `json:"last_sample_age_s"`
}

// handleHealth reports liveness: 200 + "ok" when a sample was written within 2×
// the poll interval, 503 + "stale" when stale or the DB is empty (FR-7).
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ageMS, found, err := s.store.LastSampleAgeMS(r.Context(), time.Now().UTC().UnixMilli())
	if err != nil {
		s.log.Error("api: health", "err", err)
		s.writeErr(w, http.StatusInternalServerError, "health failed")
		return
	}
	if !found {
		s.writeJSON(w, http.StatusServiceUnavailable, healthResponse{Status: "stale"})
		return
	}

	ageS := int64(math.Round(float64(ageMS) / 1000))
	resp := healthResponse{Status: "ok", LastSampleAgeS: &ageS}
	status := http.StatusOK
	if ageMS > int64(2*s.pollInterval/time.Millisecond) {
		resp.Status = "stale"
		status = http.StatusServiceUnavailable
	}
	s.writeJSON(w, status, resp)
}
