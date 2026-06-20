package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"bitbucket.org/andrewburnsza/hinaess-battery-charge-monitor/internal/bms"
	"bitbucket.org/andrewburnsza/hinaess-battery-charge-monitor/internal/store"
)

// newStoreServer builds a Server backed by a fresh temp store and returns both.
// pollInterval is fixed at 15s, matching the config default, so the health
// staleness window is 30s.
func newStoreServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "samples.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, 15*time.Second, ":0", nil), st
}

// cells16 builds a 16-element cell slice starting at baseMV (index i = baseMV+i).
func cells16(baseMV int) []int {
	cells := make([]int, 16)
	for i := range cells {
		cells[i] = baseMV + i
	}
	return cells
}

func do(t *testing.T, s *Server, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, req)
	return rec
}

func TestAPILatest(t *testing.T) {
	ctx := context.Background()

	t.Run("empty DB returns 200 with empty arrays", func(t *testing.T) {
		s, _ := newStoreServer(t)
		rec := do(t, s, http.MethodGet, "/api/latest")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", ct)
		}
		// cells_mv and temps must be [] not null so the frontend tolerates it.
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if string(raw["ts"]) != "0" {
			t.Fatalf("ts = %s, want 0", raw["ts"])
		}
		if string(raw["cells_mv"]) != "[]" {
			t.Fatalf("cells_mv = %s, want []", raw["cells_mv"])
		}
		if string(raw["temps"]) != "[]" {
			t.Fatalf("temps = %s, want []", raw["temps"])
		}
	})

	t.Run("one poll returns the sample shape", func(t *testing.T) {
		s, st := newStoreServer(t)
		ts := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
		st.Add(bms.PackSample{
			Timestamp: ts,
			PackMV:    53120,
			PackMA:    1500,
			SOCPct:    78.0,
			SOHPct:    100.0,
			Cycles:    12,
			RemainMAH: 280000,
			FullMAH:   320000,
			Cells:     cells16(3300),
			Temps:     []bms.Temp{{Probe: "t1", DeciC: 231}, {Probe: "mos", DeciC: 272}},
		})
		if _, err := st.Flush(ctx); err != nil {
			t.Fatalf("Flush: %v", err)
		}

		rec := do(t, s, http.MethodGet, "/api/latest")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var ls store.LatestSample
		if err := json.Unmarshal(rec.Body.Bytes(), &ls); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if ls.TS != ts.UnixMilli() {
			t.Fatalf("ts = %d, want %d", ls.TS, ts.UnixMilli())
		}
		if ls.PackMV != 53120 || ls.PackMA != 1500 {
			t.Fatalf("pack mv/ma = %d/%d, want 53120/1500", ls.PackMV, ls.PackMA)
		}
		if len(ls.Cells) != 16 {
			t.Fatalf("cells len = %d, want 16", len(ls.Cells))
		}
		if len(ls.Temps) != 2 {
			t.Fatalf("temps len = %d, want 2", len(ls.Temps))
		}
	})
}

func TestAPIRange(t *testing.T) {
	ctx := context.Background()
	s, st := newStoreServer(t)

	base := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	const n = 5
	for i := 0; i < n; i++ {
		ts := base.Add(time.Duration(i) * 15 * time.Second)
		st.Add(bms.PackSample{
			Timestamp: ts,
			PackMV:    53000 + i,
			PackMA:    1500 + i,
			Cells:     cells16(3300 + i),
		})
	}
	if _, err := st.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	from := base.Add(-time.Minute).UnixMilli()
	to := base.Add(time.Hour).UnixMilli()

	t.Run("aligned arrays", func(t *testing.T) {
		rec := do(t, s, http.MethodGet,
			"/api/range?from="+i64(from)+"&to="+i64(to))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var res store.RangeResult
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(res.TS) != n {
			t.Fatalf("len(TS) = %d, want %d", len(res.TS), n)
		}
		if len(res.PackMV) != len(res.TS) || len(res.PackMA) != len(res.TS) {
			t.Fatalf("pack lens %d/%d, want %d", len(res.PackMV), len(res.PackMA), len(res.TS))
		}
		if len(res.Cells) != 16 {
			t.Fatalf("len(Cells) = %d, want 16", len(res.Cells))
		}
		for i, c := range res.Cells {
			if len(c) != len(res.TS) {
				t.Fatalf("len(Cells[%d]) = %d, want %d", i, len(c), len(res.TS))
			}
		}
	})

	t.Run("fields filter limits the series", func(t *testing.T) {
		rec := do(t, s, http.MethodGet,
			"/api/range?from="+i64(from)+"&to="+i64(to)+"&fields=pack_mv")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var res store.RangeResult
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(res.PackMV) == 0 {
			t.Fatalf("pack_mv empty, want populated")
		}
		if res.Cells != nil || res.PackMA != nil {
			t.Fatalf("cells/pack_ma should be absent when only pack_mv requested")
		}
	})

	badParams := []struct {
		name, query string
	}{
		{"missing from", "/api/range?to=" + i64(to)},
		{"missing to", "/api/range?from=" + i64(from)},
		{"non-integer from", "/api/range?from=abc&to=" + i64(to)},
		{"from >= to", "/api/range?from=" + i64(to) + "&to=" + i64(from)},
		{"unknown field", "/api/range?from=" + i64(from) + "&to=" + i64(to) + "&fields=bogus"},
	}
	for _, tc := range badParams {
		t.Run("400 on "+tc.name, func(t *testing.T) {
			rec := do(t, s, http.MethodGet, tc.query)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
		})
	}
}

func TestAPIHealth(t *testing.T) {
	ctx := context.Background()

	t.Run("empty DB is 503 with null age", func(t *testing.T) {
		s, _ := newStoreServer(t)
		rec := do(t, s, http.MethodGet, "/api/health")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		var resp healthResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.Status != "stale" {
			t.Fatalf("status = %q, want stale", resp.Status)
		}
		if resp.LastSampleAgeS != nil {
			t.Fatalf("last_sample_age_s = %v, want null", *resp.LastSampleAgeS)
		}
	})

	t.Run("fresh sample is 200 ok", func(t *testing.T) {
		s, st := newStoreServer(t)
		st.Add(bms.PackSample{Timestamp: time.Now().UTC(), PackMV: 53000, Cells: cells16(3300)})
		if _, err := st.Flush(ctx); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		rec := do(t, s, http.MethodGet, "/api/health")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var resp healthResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.Status != "ok" {
			t.Fatalf("status = %q, want ok", resp.Status)
		}
		if resp.LastSampleAgeS == nil {
			t.Fatalf("last_sample_age_s = null, want a number")
		}
	})

	t.Run("stale sample is 503", func(t *testing.T) {
		s, st := newStoreServer(t)
		// 60s old > 2×15s threshold.
		st.Add(bms.PackSample{Timestamp: time.Now().UTC().Add(-60 * time.Second), PackMV: 53000, Cells: cells16(3300)})
		if _, err := st.Flush(ctx); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		rec := do(t, s, http.MethodGet, "/api/health")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		var resp healthResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.Status != "stale" {
			t.Fatalf("status = %q, want stale", resp.Status)
		}
	})
}

// TestAPINonGETRejected confirms a non-GET method on an /api/* route returns 405
// (FR-4 "Only GET is accepted").
func TestAPINonGETRejected(t *testing.T) {
	s, _ := newStoreServer(t)
	for _, path := range []string{"/api/latest", "/api/range", "/api/health"} {
		rec := do(t, s, http.MethodPost, path)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s status = %d, want 405", path, rec.Code)
		}
	}
}

// i64 formats an int64 for query-string construction.
func i64(v int64) string {
	return strconv.FormatInt(v, 10)
}
