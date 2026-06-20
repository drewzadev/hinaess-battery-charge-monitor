package store

import (
	"context"
	"fmt"
	"time"
)

const msPerDay = int64(86_400_000)

// Prune deletes samples older than retentionDays from all three tables in one
// transaction (FR-6, requirements.md:246) and returns the total number of rows
// deleted across the three tables. The cutoff is now_ms - retentionDays*86_400_000;
// rows with ts strictly below the cutoff are removed while more recent rows
// survive.
//
// retentionDays == 0 means "keep forever": Prune skips entirely and returns
// (0, nil) without opening a transaction. The poll loop (FR-5) calls this on
// startup and once every 24 h thereafter.
func (s *Store) Prune(ctx context.Context, retentionDays int) (int, error) {
	if retentionDays == 0 {
		return 0, nil
	}

	cutoff := time.Now().UTC().UnixMilli() - int64(retentionDays)*msPerDay

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin prune tx: %w", err)
	}
	// Roll back on any error path; a no-op after a successful Commit.
	defer tx.Rollback()

	var deleted int
	for _, table := range []string{"samples", "cell_samples", "temp_samples"} {
		res, err := tx.ExecContext(ctx,
			"DELETE FROM "+table+" WHERE ts < ?", cutoff)
		if err != nil {
			return 0, fmt.Errorf("store: prune %s: %w", table, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("store: prune %s rows affected: %w", table, err)
		}
		deleted += int(n)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit prune tx: %w", err)
	}
	return deleted, nil
}
