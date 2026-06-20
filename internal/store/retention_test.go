package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestPruneRemovesOldRows inserts back-dated rows (two days old) and recent rows,
// then prunes with retention_days=1 and asserts the old rows are gone from all
// three tables while the recent rows survive (FR-6 / AC-10).
func TestPruneRemovesOldRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour) // two days old → older than the 1-day cutoff
	recent := now.Add(-1 * time.Minute)

	st.Add(sampleAt(old, 3300))
	st.Add(sampleAt(recent, 3310))
	if _, err := st.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Sanity: both polls landed before pruning.
	assertCount(t, st, "samples", 2)
	assertCount(t, st, "cell_samples", 32)
	assertCount(t, st, "temp_samples", 12)

	deleted, err := st.Prune(context.Background(), 1)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	// One stale poll: 1 samples + 16 cell_samples + 6 temp_samples = 23 rows.
	if deleted != 23 {
		t.Errorf("Prune deleted %d rows, want 23", deleted)
	}

	// The back-dated poll is gone; only the recent poll's rows survive.
	assertCount(t, st, "samples", 1)
	assertCount(t, st, "cell_samples", 16)
	assertCount(t, st, "temp_samples", 6)

	var ts int64
	if err := st.db.QueryRow("SELECT ts FROM samples").Scan(&ts); err != nil {
		t.Fatalf("query surviving ts: %v", err)
	}
	if ts != recent.UnixMilli() {
		t.Errorf("surviving ts = %d, want %d (recent)", ts, recent.UnixMilli())
	}
}

// TestPruneZeroKeepsForever verifies retention_days=0 deletes nothing.
func TestPruneZeroKeepsForever(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	old := time.Now().UTC().Add(-365 * 24 * time.Hour)
	st.Add(sampleAt(old, 3300))
	if _, err := st.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	deleted, err := st.Prune(context.Background(), 0)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if deleted != 0 {
		t.Errorf("Prune(0) deleted %d rows, want 0", deleted)
	}
	assertCount(t, st, "samples", 1)
	assertCount(t, st, "cell_samples", 16)
	assertCount(t, st, "temp_samples", 6)
}
