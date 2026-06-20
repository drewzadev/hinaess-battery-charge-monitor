package store

import (
	"context"
	"fmt"
	"strings"

	"bitbucket.org/andrewburnsza/hinaess-battery-charge-monitor/internal/bms"
)

// Add appends a sample to the in-memory write buffer (FR-4). It does not touch
// the database; the buffer is drained by Flush, which the poll loop (FR-5)
// drives via a ticker and once more on shutdown.
func (s *Store) Add(sample bms.PackSample) {
	s.buf = append(s.buf, sample)
}

// Flush writes every buffered sample to the database in a single transaction
// and returns the number of samples (samples-table rows) flushed. Batching all
// buffered samples into one tx keeps writes to ≤ 1 transaction per flush
// interval, sparing the SD card (requirements.md:242). The buffer is cleared
// only on a successful commit, so a failed flush leaves the samples queued for
// the next attempt.
//
// For each buffered sample it inserts one samples row, one cell_samples row per
// cell (cell_idx = slice index 0..15), and one temp_samples row per probe. ts
// is sample.Timestamp.UTC().UnixMilli() and is shared across all three tables so
// a poll's writes join on a single ts (Current State).
func (s *Store) Flush(ctx context.Context) (int, error) {
	if len(s.buf) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin flush tx: %w", err)
	}
	// Roll back on any error path; a no-op after a successful Commit.
	defer tx.Rollback()

	sampleStmt, err := tx.PrepareContext(ctx,
		`INSERT INTO samples (ts, pack_mv, pack_ma, soc_pct, soh_pct, cycles, remain_mah, full_mah)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("store: prepare samples insert: %w", err)
	}
	defer sampleStmt.Close()

	cellStmt, err := tx.PrepareContext(ctx,
		`INSERT INTO cell_samples (ts, cell_idx, mv) VALUES (?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("store: prepare cell_samples insert: %w", err)
	}
	defer cellStmt.Close()

	tempStmt, err := tx.PrepareContext(ctx,
		`INSERT INTO temp_samples (ts, probe, deci_c) VALUES (?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("store: prepare temp_samples insert: %w", err)
	}
	defer tempStmt.Close()

	for _, sample := range s.buf {
		ts := sample.Timestamp.UTC().UnixMilli()

		if _, err := sampleStmt.ExecContext(ctx, ts,
			sample.PackMV, sample.PackMA, sample.SOCPct, sample.SOHPct,
			sample.Cycles, sample.RemainMAH, sample.FullMAH); err != nil {
			return 0, fmt.Errorf("store: insert sample ts=%d: %w", ts, err)
		}

		for idx, mv := range sample.Cells {
			if _, err := cellStmt.ExecContext(ctx, ts, idx, mv); err != nil {
				return 0, fmt.Errorf("store: insert cell ts=%d idx=%d: %w", ts, idx, err)
			}
		}

		for _, t := range sample.Temps {
			// Slice 1 already emits lowercase probe labels; ToLower is a
			// harmless-but-redundant defense matching the schema column (FR-4).
			if _, err := tempStmt.ExecContext(ctx, ts, strings.ToLower(t.Probe), t.DeciC); err != nil {
				return 0, fmt.Errorf("store: insert temp ts=%d probe=%q: %w", ts, t.Probe, err)
			}
		}
	}

	n := len(s.buf)
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit flush tx: %w", err)
	}
	s.buf = s.buf[:0]
	return n, nil
}
