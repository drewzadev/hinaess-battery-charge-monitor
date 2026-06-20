package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
)

// numCells is the fixed number of per-poll cell readings (cell_idx 0..15).
const numCells = 16

// rangeRawThreshold is the per-range sample-row count at or below which Range
// returns raw rows; above it (and not raw) it downsamples into buckets (FR-6).
const rangeRawThreshold = 5000

// rangeRawCap is the hard cap on raw points per series; above it Range clamps to
// the most-recent rangeRawCap timestamps and sets Truncated (FR-6).
const rangeRawCap = 50000

// rangeBuckets is the target number of time buckets when downsampling (FR-6).
const rangeBuckets = 5000

// LatestSample is the most-recent poll across all three tables.
type LatestSample struct {
	TS        int64     `json:"ts"` // epoch ms; 0 and Found=false if DB empty
	PackMV    int       `json:"pack_mv"`
	PackMA    int       `json:"pack_ma"`
	SOCPct    float64   `json:"soc_pct"`
	SOHPct    float64   `json:"soh_pct"`
	Cycles    int       `json:"cycles"`
	RemainMAH int       `json:"remain_mah"`
	FullMAH   int       `json:"full_mah"`
	Cells     []int     `json:"cells_mv"` // index = cell_idx, mV
	Temps     []TempRow `json:"temps"`
	Found     bool      `json:"-"`
}

// TempRow is one temperature probe reading for the latest poll.
type TempRow struct {
	Probe string `json:"probe"`
	DeciC int    `json:"deci_c"`
}

// Latest returns the most-recent poll assembled from the samples, cell_samples,
// and temp_samples tables (all sharing one ts). On an empty DB it returns
// LatestSample{Found:false} with a nil error. Reads run on the read-only handle
// (rdb) so they never contend with the single-connection writer (FR-1/FR-2).
func (s *Store) Latest(ctx context.Context) (LatestSample, error) {
	var ls LatestSample
	row := s.rdb.QueryRowContext(ctx, `SELECT ts, pack_mv, pack_ma, soc_pct, soh_pct,
		cycles, remain_mah, full_mah FROM samples ORDER BY ts DESC LIMIT 1`)
	err := row.Scan(&ls.TS, &ls.PackMV, &ls.PackMA, &ls.SOCPct, &ls.SOHPct,
		&ls.Cycles, &ls.RemainMAH, &ls.FullMAH)
	if err == sql.ErrNoRows {
		return LatestSample{Found: false}, nil
	}
	if err != nil {
		return LatestSample{}, fmt.Errorf("latest sample: %w", err)
	}
	ls.Found = true

	cells, err := s.rdb.QueryContext(ctx,
		`SELECT cell_idx, mv FROM cell_samples WHERE ts=? ORDER BY cell_idx`, ls.TS)
	if err != nil {
		return LatestSample{}, fmt.Errorf("latest cells: %w", err)
	}
	defer cells.Close()
	for cells.Next() {
		var idx, mv int
		if err := cells.Scan(&idx, &mv); err != nil {
			return LatestSample{}, fmt.Errorf("scan cell: %w", err)
		}
		ls.Cells = append(ls.Cells, mv)
	}
	if err := cells.Err(); err != nil {
		return LatestSample{}, fmt.Errorf("iterate cells: %w", err)
	}

	temps, err := s.rdb.QueryContext(ctx,
		`SELECT probe, deci_c FROM temp_samples WHERE ts=? ORDER BY probe`, ls.TS)
	if err != nil {
		return LatestSample{}, fmt.Errorf("latest temps: %w", err)
	}
	defer temps.Close()
	for temps.Next() {
		var tr TempRow
		if err := temps.Scan(&tr.Probe, &tr.DeciC); err != nil {
			return LatestSample{}, fmt.Errorf("scan temp: %w", err)
		}
		ls.Temps = append(ls.Temps, tr)
	}
	if err := temps.Err(); err != nil {
		return LatestSample{}, fmt.Errorf("iterate temps: %w", err)
	}

	return ls, nil
}

// RangeResult is column-oriented for uPlot: every array is the same length as
// TS and index-aligned to it. Empty buckets carry null (a *int nil),
// which uPlot renders as a gap.
type RangeResult struct {
	TS        []int64  `json:"ts"`
	Cells     [][]*int `json:"cells,omitempty"`   // [cellIdx][bucket]; present iff "cells" requested
	PackMV    []*int   `json:"pack_mv,omitempty"`
	PackMA    []*int   `json:"pack_ma,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
}

// wantFields resolves the requested field set into three booleans. An empty
// fields slice means all three (FR-6).
func wantFields(fields []string) (cells, packMV, packMA bool) {
	if len(fields) == 0 {
		return true, true, true
	}
	for _, f := range fields {
		switch f {
		case "cells":
			cells = true
		case "pack_mv":
			packMV = true
		case "pack_ma":
			packMA = true
		}
	}
	return cells, packMV, packMA
}

// Range returns the time series for [from, to] (epoch ms, inclusive) shaped for
// uPlot: a shared TS axis with each requested series index-aligned to it and
// null in positions with no data. fields is the validated subset of
// {"cells","pack_mv","pack_ma"}; empty means all three. When raw is true, or the
// in-range sample count is <= rangeRawThreshold, raw rows are returned (capped at
// rangeRawCap, most-recent-wins, with Truncated set); otherwise the series are
// downsampled into a shared deterministic bucket grid (FR-6). Reads run on the
// read-only handle (rdb).
func (s *Store) Range(ctx context.Context, from, to int64, fields []string, raw bool) (RangeResult, error) {
	wantCells, wantMV, wantMA := wantFields(fields)

	var count int64
	row := s.rdb.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM samples WHERE ts >= ? AND ts <= ?`, from, to)
	if err := row.Scan(&count); err != nil {
		return RangeResult{}, fmt.Errorf("range count: %w", err)
	}

	if raw || count <= rangeRawThreshold {
		return s.rangeRaw(ctx, from, to, wantCells, wantMV, wantMA, count)
	}
	return s.rangeBucketed(ctx, from, to, wantCells, wantMV, wantMA)
}

// rangeRaw returns raw rows aligned on the shared sorted set of timestamps that
// exist in samples within [from, to]. If count exceeds rangeRawCap it restricts
// to the most-recent rangeRawCap timestamps and sets Truncated (FR-6).
func (s *Store) rangeRaw(ctx context.Context, from, to int64, wantCells, wantMV, wantMA bool, count int64) (RangeResult, error) {
	var res RangeResult

	// Determine the shared, ascending set of timestamps and the cap window.
	var (
		tsList    []int64
		truncated bool
	)
	if count > rangeRawCap {
		truncated = true
		rows, err := s.rdb.QueryContext(ctx,
			`SELECT ts FROM samples WHERE ts >= ? AND ts <= ? ORDER BY ts DESC LIMIT ?`,
			from, to, rangeRawCap)
		if err != nil {
			return RangeResult{}, fmt.Errorf("range raw ts (capped): %w", err)
		}
		for rows.Next() {
			var ts int64
			if err := rows.Scan(&ts); err != nil {
				rows.Close()
				return RangeResult{}, fmt.Errorf("scan range ts: %w", err)
			}
			tsList = append(tsList, ts)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return RangeResult{}, fmt.Errorf("iterate range ts: %w", err)
		}
		rows.Close()
		// Re-sort ascending: the LIMIT query came back descending.
		sort.Slice(tsList, func(i, j int) bool { return tsList[i] < tsList[j] })
	} else {
		rows, err := s.rdb.QueryContext(ctx,
			`SELECT ts FROM samples WHERE ts >= ? AND ts <= ? ORDER BY ts ASC`, from, to)
		if err != nil {
			return RangeResult{}, fmt.Errorf("range raw ts: %w", err)
		}
		for rows.Next() {
			var ts int64
			if err := rows.Scan(&ts); err != nil {
				rows.Close()
				return RangeResult{}, fmt.Errorf("scan range ts: %w", err)
			}
			tsList = append(tsList, ts)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return RangeResult{}, fmt.Errorf("iterate range ts: %w", err)
		}
		rows.Close()
	}

	res.TS = tsList
	res.Truncated = truncated

	// Position lookup keyed by ts; positions outside the (possibly capped) set
	// are simply ignored, which clamps all series to the shared timestamps.
	pos := make(map[int64]int, len(tsList))
	for i, ts := range tsList {
		pos[ts] = i
	}
	loBound := int64(math.MinInt64)
	if len(tsList) > 0 {
		loBound = tsList[0] // cap window low edge for the capped case
	}

	if wantMV {
		res.PackMV = make([]*int, len(tsList))
	}
	if wantMA {
		res.PackMA = make([]*int, len(tsList))
	}
	if wantMV || wantMA {
		rows, err := s.rdb.QueryContext(ctx,
			`SELECT ts, pack_mv, pack_ma FROM samples WHERE ts >= ? AND ts <= ? ORDER BY ts ASC`,
			loBound, to)
		if err != nil {
			return RangeResult{}, fmt.Errorf("range raw pack: %w", err)
		}
		for rows.Next() {
			var ts int64
			var mv, ma int
			if err := rows.Scan(&ts, &mv, &ma); err != nil {
				rows.Close()
				return RangeResult{}, fmt.Errorf("scan range pack: %w", err)
			}
			p, ok := pos[ts]
			if !ok {
				continue
			}
			if wantMV {
				v := mv
				res.PackMV[p] = &v
			}
			if wantMA {
				v := ma
				res.PackMA[p] = &v
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return RangeResult{}, fmt.Errorf("iterate range pack: %w", err)
		}
		rows.Close()
	}

	if wantCells {
		res.Cells = make([][]*int, numCells)
		for i := range res.Cells {
			res.Cells[i] = make([]*int, len(tsList))
		}
		rows, err := s.rdb.QueryContext(ctx,
			`SELECT ts, cell_idx, mv FROM cell_samples WHERE ts >= ? AND ts <= ? ORDER BY ts, cell_idx`,
			loBound, to)
		if err != nil {
			return RangeResult{}, fmt.Errorf("range raw cells: %w", err)
		}
		for rows.Next() {
			var ts int64
			var idx, mv int
			if err := rows.Scan(&ts, &idx, &mv); err != nil {
				rows.Close()
				return RangeResult{}, fmt.Errorf("scan range cell: %w", err)
			}
			p, ok := pos[ts]
			if !ok || idx < 0 || idx >= numCells {
				continue
			}
			v := mv
			res.Cells[idx][p] = &v
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return RangeResult{}, fmt.Errorf("iterate range cells: %w", err)
		}
		rows.Close()
	}

	return res, nil
}

// rangeBucketed downsamples each requested series into a shared, deterministic
// bucket grid and slots the rounded averages positionally so all series stay
// index-aligned to TS, leaving genuinely-empty positions nil (FR-6).
func (s *Store) rangeBucketed(ctx context.Context, from, to int64, wantCells, wantMV, wantMA bool) (RangeResult, error) {
	var res RangeResult

	bucketMS := (to - from) / rangeBuckets
	if bucketMS < 1 {
		bucketMS = 1
	}
	bucketTS := func(k int64) int64 { return from + k*bucketMS + bucketMS/2 }

	// Per-series bucket-index -> rounded average, plus the union of populated ks.
	packMV := map[int64]int{}
	packMA := map[int64]int{}
	cells := make([]map[int64]int, numCells)
	kSet := map[int64]struct{}{}

	if wantMV || wantMA {
		rows, err := s.rdb.QueryContext(ctx,
			`SELECT ((ts - ?) / ?) AS k, AVG(pack_mv), AVG(pack_ma)
			 FROM samples WHERE ts >= ? AND ts <= ? GROUP BY k ORDER BY k`,
			from, bucketMS, from, to)
		if err != nil {
			return RangeResult{}, fmt.Errorf("range bucket pack: %w", err)
		}
		for rows.Next() {
			var k int64
			var avgMV, avgMA float64
			if err := rows.Scan(&k, &avgMV, &avgMA); err != nil {
				rows.Close()
				return RangeResult{}, fmt.Errorf("scan bucket pack: %w", err)
			}
			if wantMV {
				packMV[k] = int(math.Round(avgMV))
			}
			if wantMA {
				packMA[k] = int(math.Round(avgMA))
			}
			kSet[k] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return RangeResult{}, fmt.Errorf("iterate bucket pack: %w", err)
		}
		rows.Close()
	}

	if wantCells {
		for i := 0; i < numCells; i++ {
			cells[i] = map[int64]int{}
			rows, err := s.rdb.QueryContext(ctx,
				`SELECT ((ts - ?) / ?) AS k, AVG(mv)
				 FROM cell_samples WHERE ts >= ? AND ts <= ? AND cell_idx = ? GROUP BY k ORDER BY k`,
				from, bucketMS, from, to, i)
			if err != nil {
				return RangeResult{}, fmt.Errorf("range bucket cell %d: %w", i, err)
			}
			for rows.Next() {
				var k int64
				var avg float64
				if err := rows.Scan(&k, &avg); err != nil {
					rows.Close()
					return RangeResult{}, fmt.Errorf("scan bucket cell %d: %w", i, err)
				}
				cells[i][k] = int(math.Round(avg))
				kSet[k] = struct{}{}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return RangeResult{}, fmt.Errorf("iterate bucket cell %d: %w", i, err)
			}
			rows.Close()
		}
	}

	// Sorted union of populated bucket indices -> positions.
	ks := make([]int64, 0, len(kSet))
	for k := range kSet {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i] < ks[j] })

	res.TS = make([]int64, len(ks))
	pos := make(map[int64]int, len(ks))
	for i, k := range ks {
		res.TS[i] = bucketTS(k)
		pos[k] = i
	}

	fill := func(src map[int64]int) []*int {
		out := make([]*int, len(ks))
		for k, v := range src {
			val := v
			out[pos[k]] = &val
		}
		return out
	}

	if wantMV {
		res.PackMV = fill(packMV)
	}
	if wantMA {
		res.PackMA = fill(packMA)
	}
	if wantCells {
		res.Cells = make([][]*int, numCells)
		for i := 0; i < numCells; i++ {
			res.Cells[i] = fill(cells[i])
		}
	}

	return res, nil
}

// LastSampleAgeMS returns the age, in milliseconds, of the most-recent sample
// relative to nowMS (epoch ms UTC). found is false on an empty DB. Backs the
// /api/health staleness check (FR-1/FR-7).
func (s *Store) LastSampleAgeMS(ctx context.Context, nowMS int64) (ageMS int64, found bool, err error) {
	var ts int64
	row := s.rdb.QueryRowContext(ctx, `SELECT ts FROM samples ORDER BY ts DESC LIMIT 1`)
	switch scanErr := row.Scan(&ts); {
	case scanErr == sql.ErrNoRows:
		return 0, false, nil
	case scanErr != nil:
		return 0, false, fmt.Errorf("last sample ts: %w", scanErr)
	}
	return nowMS - ts, true, nil
}
