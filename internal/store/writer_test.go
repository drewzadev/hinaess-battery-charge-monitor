package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"bitbucket.org/andrewburnsza/hinaess-battery-charge-monitor/internal/bms"
)

// sampleAt builds a PackSample with 16 cells and the 6 standard probes so a
// flush exercises the full per-poll row fan-out (1 samples + 16 cell_samples +
// 6 temp_samples).
func sampleAt(ts time.Time, baseMV int) bms.PackSample {
	cells := make([]int, 16)
	for i := range cells {
		cells[i] = baseMV + i
	}
	return bms.PackSample{
		Timestamp: ts,
		Cells:     cells,
		Temps: []bms.Temp{
			{Probe: "t1", DeciC: 250},
			{Probe: "t2", DeciC: 251},
			{Probe: "t3", DeciC: 252},
			{Probe: "t4", DeciC: 253},
			{Probe: "mos", DeciC: 300},
			{Probe: "env", DeciC: 200},
		},
		PackMV:    53120,
		PackMA:    1500,
		SOCPct:    78.0,
		SOHPct:    99.0,
		Cycles:    12,
		RemainMAH: 90000,
		FullMAH:   100000,
	}
}

// TestFlushRowCounts buffers two PackSamples, flushes them in one transaction,
// and asserts the expected per-table row counts and that cell_idx spans 0..15
// for each poll (Step 3 / FR-4).
func TestFlushRowCounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	t1 := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(15 * time.Second)
	st.Add(sampleAt(t1, 3300))
	st.Add(sampleAt(t2, 3310))

	n, err := st.Flush(context.Background())
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if n != 2 {
		t.Errorf("Flush returned %d, want 2 samples", n)
	}

	// Two polls → 2 samples, 2×16 cell_samples, 2×6 temp_samples.
	assertCount(t, st, "samples", 2)
	assertCount(t, st, "cell_samples", 32)
	assertCount(t, st, "temp_samples", 12)

	// The buffer must be drained after a successful flush, so a second flush is a
	// no-op rather than re-inserting (which would violate the PRIMARY KEYs).
	n, err = st.Flush(context.Background())
	if err != nil {
		t.Fatalf("second Flush: %v", err)
	}
	if n != 0 {
		t.Errorf("second Flush returned %d, want 0", n)
	}

	// cell_idx must span 0..15 for each poll's ts.
	for _, ts := range []int64{t1.UnixMilli(), t2.UnixMilli()} {
		rows, err := st.db.Query(
			"SELECT cell_idx FROM cell_samples WHERE ts = ? ORDER BY cell_idx", ts)
		if err != nil {
			t.Fatalf("query cell_idx: %v", err)
		}
		var got []int
		for rows.Next() {
			var idx int
			if err := rows.Scan(&idx); err != nil {
				rows.Close()
				t.Fatalf("scan cell_idx: %v", err)
			}
			got = append(got, idx)
		}
		rows.Close()
		if len(got) != 16 {
			t.Fatalf("ts=%d: got %d cell rows, want 16", ts, len(got))
		}
		for i, idx := range got {
			if idx != i {
				t.Errorf("ts=%d: cell_idx[%d] = %d, want %d", ts, i, idx, i)
			}
		}
	}

	// ts must be the UnixMilli of the sample timestamp, shared as the join key.
	var soc float64
	if err := st.db.QueryRow(
		"SELECT soc_pct FROM samples WHERE ts = ?", t1.UnixMilli()).Scan(&soc); err != nil {
		t.Fatalf("query sample by ts: %v", err)
	}
	if soc != 78.0 {
		t.Errorf("soc_pct = %v, want 78.0", soc)
	}
}

// TestFlushEmpty verifies flushing an empty buffer is a no-op.
func TestFlushEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	n, err := st.Flush(context.Background())
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if n != 0 {
		t.Errorf("Flush returned %d, want 0", n)
	}
}

func assertCount(t *testing.T, st *Store, table string, want int) {
	t.Helper()
	var got int
	if err := st.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&got); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if got != want {
		t.Errorf("%s row count = %d, want %d", table, got, want)
	}
}
