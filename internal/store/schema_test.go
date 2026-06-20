package store

import (
	"context"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// TestOpenSchemaAndWAL is AC-8: opening a temp DB sets WAL mode and creates the
// three tables and two indexes.
func TestOpenSchemaAndWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.db")

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// WAL must be active (AC-8).
	var mode string
	if err := st.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}

	// All three tables must exist.
	wantTables := []string{"cell_samples", "samples", "temp_samples"}
	gotTables := objectNames(t, st, "table")
	if !equal(gotTables, wantTables) {
		t.Errorf("tables = %v, want %v", gotTables, wantTables)
	}

	// Both named indexes must exist (SQLite also creates implicit indexes for the
	// PRIMARY KEYs, named sqlite_autoindex_*; we only assert our two explicit ones).
	wantIndexes := []string{"idx_cell_samples_ts", "idx_samples_ts"}
	gotIndexes := filterPrefix(objectNames(t, st, "index"), "idx_")
	if !equal(gotIndexes, wantIndexes) {
		t.Errorf("indexes = %v, want %v", gotIndexes, wantIndexes)
	}
}

// TestOpenIdempotent verifies the IF NOT EXISTS DDL lets a second Open on the
// same file succeed without error.
func TestOpenIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.db")

	st1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := st1.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if err := st2.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestReadHandleSeesWrites is the FR-2 acceptance: a sample written through the
// single-writer handle (Flush) is visible when read back through the separate
// read-only handle (rdb). WAL must let the reader see the last committed write.
func TestReadHandleSeesWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.db")

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if st.rdb == nil {
		t.Fatal("Open did not set the read handle (rdb is nil)")
	}

	ts := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	st.Add(sampleAt(ts, 3300))
	if _, err := st.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Read the just-written row back through the read-only handle.
	var gotMV int
	if err := st.rdb.QueryRow(
		"SELECT pack_mv FROM samples WHERE ts = ?", ts.UnixMilli()).Scan(&gotMV); err != nil {
		t.Fatalf("read via rdb: %v", err)
	}
	if gotMV != 53120 {
		t.Errorf("pack_mv via rdb = %d, want 53120", gotMV)
	}

	// The read handle must reject writes (mode=ro / query_only).
	if _, err := st.rdb.Exec(
		"INSERT INTO samples (ts, pack_mv, pack_ma) VALUES (1, 1, 1)"); err == nil {
		t.Error("write through read-only handle succeeded, want error")
	}
}

// objectNames returns the sorted names of sqlite_master objects of the given
// type, excluding SQLite's internal sqlite_* objects.
func objectNames(t *testing.T, st *Store, typ string) []string {
	t.Helper()
	rows, err := st.db.Query(
		"SELECT name FROM sqlite_master WHERE type = ? AND name NOT LIKE 'sqlite_%' ORDER BY name", typ)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan name: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	return names
}

func filterPrefix(names []string, prefix string) []string {
	var out []string
	for _, n := range names {
		if len(n) >= len(prefix) && n[:len(prefix)] == prefix {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
