package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"bitbucket.org/andrewburnsza/hinaess-battery-charge-monitor/internal/bms"
)

// openTestStore opens a fresh store in a temp dir and registers Close on cleanup.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "samples.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// cells16 builds a 16-element cell slice starting at baseMV (index i = baseMV+i).
func cells16(baseMV int) []int {
	cells := make([]int, 16)
	for i := range cells {
		cells[i] = baseMV + i
	}
	return cells
}

func TestLatest(t *testing.T) {
	ctx := context.Background()

	t.Run("empty DB returns Found=false", func(t *testing.T) {
		st := openTestStore(t)
		ls, err := st.Latest(ctx)
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		if ls.Found {
			t.Fatalf("Found = true, want false on empty DB")
		}
		if ls.TS != 0 {
			t.Fatalf("TS = %d, want 0 on empty DB", ls.TS)
		}
		if len(ls.Cells) != 0 || len(ls.Temps) != 0 {
			t.Fatalf("cells=%d temps=%d, want 0/0 on empty DB", len(ls.Cells), len(ls.Temps))
		}
	})

	t.Run("one poll assembles 16 cells and temps", func(t *testing.T) {
		st := openTestStore(t)
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
			Temps: []bms.Temp{
				{Probe: "env", DeciC: 210},
				{Probe: "mos", DeciC: 272},
				{Probe: "t1", DeciC: 231},
			},
		})
		if _, err := st.Flush(ctx); err != nil {
			t.Fatalf("Flush: %v", err)
		}

		ls, err := st.Latest(ctx)
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		if !ls.Found {
			t.Fatalf("Found = false, want true")
		}
		if ls.TS != ts.UnixMilli() {
			t.Fatalf("TS = %d, want %d", ls.TS, ts.UnixMilli())
		}
		if ls.PackMV != 53120 || ls.PackMA != 1500 {
			t.Fatalf("pack mv/ma = %d/%d, want 53120/1500", ls.PackMV, ls.PackMA)
		}
		if ls.SOCPct != 78.0 || ls.SOHPct != 100.0 {
			t.Fatalf("soc/soh = %v/%v, want 78/100", ls.SOCPct, ls.SOHPct)
		}
		if ls.Cycles != 12 || ls.RemainMAH != 280000 || ls.FullMAH != 320000 {
			t.Fatalf("cycles/remain/full = %d/%d/%d, want 12/280000/320000",
				ls.Cycles, ls.RemainMAH, ls.FullMAH)
		}
		if len(ls.Cells) != 16 {
			t.Fatalf("cells len = %d, want 16", len(ls.Cells))
		}
		for i, mv := range ls.Cells {
			if mv != 3300+i {
				t.Fatalf("cells[%d] = %d, want %d", i, mv, 3300+i)
			}
		}
		// Temps are returned ordered by probe.
		wantTemps := []TempRow{
			{Probe: "env", DeciC: 210},
			{Probe: "mos", DeciC: 272},
			{Probe: "t1", DeciC: 231},
		}
		if len(ls.Temps) != len(wantTemps) {
			t.Fatalf("temps len = %d, want %d", len(ls.Temps), len(wantTemps))
		}
		for i, want := range wantTemps {
			if ls.Temps[i] != want {
				t.Fatalf("temps[%d] = %+v, want %+v", i, ls.Temps[i], want)
			}
		}
	})

	t.Run("latest picks the newest of multiple polls", func(t *testing.T) {
		st := openTestStore(t)
		t1 := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
		t2 := t1.Add(15 * time.Second)
		st.Add(bms.PackSample{Timestamp: t1, PackMV: 50000, Cells: cells16(3200)})
		st.Add(bms.PackSample{Timestamp: t2, PackMV: 53000, Cells: cells16(3400)})
		if _, err := st.Flush(ctx); err != nil {
			t.Fatalf("Flush: %v", err)
		}

		ls, err := st.Latest(ctx)
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		if ls.TS != t2.UnixMilli() {
			t.Fatalf("TS = %d, want %d (newest)", ls.TS, t2.UnixMilli())
		}
		if ls.Cells[0] != 3400 {
			t.Fatalf("cells[0] = %d, want 3400 (from newest poll)", ls.Cells[0])
		}
	})
}

func TestRangeRaw(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

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
	res, err := st.Range(ctx, from, to, nil, false)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if res.Truncated {
		t.Fatalf("Truncated = true, want false for small range")
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
	// Values aligned ascending by ts.
	for i := 0; i < n; i++ {
		if res.PackMV[i] == nil || *res.PackMV[i] != 53000+i {
			t.Fatalf("PackMV[%d] = %v, want %d", i, res.PackMV[i], 53000+i)
		}
		if res.PackMA[i] == nil || *res.PackMA[i] != 1500+i {
			t.Fatalf("PackMA[%d] = %v, want %d", i, res.PackMA[i], 1500+i)
		}
		if res.Cells[0][i] == nil || *res.Cells[0][i] != 3300+i {
			t.Fatalf("Cells[0][%d] = %v, want %d", i, res.Cells[0][i], 3300+i)
		}
		if res.Cells[15][i] == nil || *res.Cells[15][i] != 3300+i+15 {
			t.Fatalf("Cells[15][%d] = %v, want %d", i, res.Cells[15][i], 3300+i+15)
		}
	}
}

func TestRangeBucketed(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	// Insert >5000 sample rows (with cells) efficiently via the buffer + a single
	// Flush transaction so the test stays fast.
	base := time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC)
	const n = 6000
	for i := 0; i < n; i++ {
		ts := base.Add(time.Duration(i) * 15 * time.Second)
		st.Add(bms.PackSample{
			Timestamp: ts,
			PackMV:    53000 + (i % 10),
			PackMA:    1500 + (i % 10),
			Cells:     cells16(3300 + (i % 10)),
		})
	}
	if _, err := st.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	from := base.UnixMilli()
	to := base.Add(time.Duration(n) * 15 * time.Second).UnixMilli()
	res, err := st.Range(ctx, from, to, nil, false)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(res.TS) == 0 || len(res.TS) > 5000 {
		t.Fatalf("len(TS) = %d, want >0 and <=5000", len(res.TS))
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
	// TS must be strictly ascending.
	for i := 1; i < len(res.TS); i++ {
		if res.TS[i] <= res.TS[i-1] {
			t.Fatalf("TS not ascending at %d: %d <= %d", i, res.TS[i], res.TS[i-1])
		}
	}
}

func TestRangeNullGaps(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	// Two polls. The second poll writes only the pack row and a partial cell set
	// (cells 0..7), leaving cells 8..15 absent at that ts -> nil at that position.
	t1 := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(15 * time.Second)
	st.Add(bms.PackSample{Timestamp: t1, PackMV: 53000, PackMA: 1500, Cells: cells16(3300)})

	partial := make([]int, 8) // only cells 0..7 present
	for i := range partial {
		partial[i] = 3400 + i
	}
	st.Add(bms.PackSample{Timestamp: t2, PackMV: 53010, PackMA: 1510, Cells: partial})
	if _, err := st.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	from := t1.Add(-time.Minute).UnixMilli()
	to := t1.Add(time.Hour).UnixMilli()
	res, err := st.Range(ctx, from, to, nil, false)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(res.TS) != 2 {
		t.Fatalf("len(TS) = %d, want 2", len(res.TS))
	}
	// Cell 10 exists at t1 (position 0) but not at t2 (position 1).
	if res.Cells[10][0] == nil || *res.Cells[10][0] != 3310 {
		t.Fatalf("Cells[10][0] = %v, want 3310", res.Cells[10][0])
	}
	if res.Cells[10][1] != nil {
		t.Fatalf("Cells[10][1] = %v, want nil (gap)", res.Cells[10][1])
	}
	// Pack series populated at both positions; TS still aligns.
	if res.PackMV[0] == nil || res.PackMV[1] == nil {
		t.Fatalf("PackMV has unexpected nil: %v", res.PackMV)
	}
}

func TestLastSampleAgeMS(t *testing.T) {
	ctx := context.Background()

	t.Run("empty DB returns found=false", func(t *testing.T) {
		st := openTestStore(t)
		age, found, err := st.LastSampleAgeMS(ctx, 5000)
		if err != nil {
			t.Fatalf("LastSampleAgeMS: %v", err)
		}
		if found {
			t.Fatalf("found = true, want false on empty DB")
		}
		if age != 0 {
			t.Fatalf("age = %d, want 0 on empty DB", age)
		}
	})

	t.Run("age math against newest sample", func(t *testing.T) {
		st := openTestStore(t)
		t1 := time.UnixMilli(1000).UTC()
		t2 := time.UnixMilli(3000).UTC()
		st.Add(bms.PackSample{Timestamp: t1, PackMV: 50000, Cells: cells16(3300)})
		st.Add(bms.PackSample{Timestamp: t2, PackMV: 50000, Cells: cells16(3300)})
		if _, err := st.Flush(ctx); err != nil {
			t.Fatalf("Flush: %v", err)
		}

		age, found, err := st.LastSampleAgeMS(ctx, 10000)
		if err != nil {
			t.Fatalf("LastSampleAgeMS: %v", err)
		}
		if !found {
			t.Fatalf("found = false, want true")
		}
		if age != 7000 { // 10000 - newest ts 3000
			t.Fatalf("age = %d, want 7000", age)
		}
	})
}
