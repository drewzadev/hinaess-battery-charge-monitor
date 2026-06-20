// Package store persists BMS samples to a local SQLite database. It uses the
// pure-Go modernc.org/sqlite driver (driver name "sqlite") via database/sql so
// the binary cross-compiles to the Pi with CGO_ENABLED=0 — never the cgo-based
// mattn/go-sqlite3 (requirements.md:34, requirements.md:41). This file owns the
// schema, WAL configuration, and open/close lifecycle (FR-3); writer.go adds the
// batched-write path (FR-4).
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"bitbucket.org/andrewburnsza/hinaess-battery-charge-monitor/internal/bms"

	_ "modernc.org/sqlite"
)

// ddl creates the three sample tables and two time indexes if they do not yet
// exist. The schema is verbatim from the requirements (requirements.md:213-238):
// ts is the de-facto join key across all three tables — a single poll's writes
// share one ts value (host-clock receive time, epoch ms UTC).
const ddl = `
CREATE TABLE IF NOT EXISTS samples (
  ts           INTEGER NOT NULL,
  pack_mv      INTEGER NOT NULL,
  pack_ma      INTEGER NOT NULL,
  soc_pct      REAL,
  soh_pct      REAL,
  cycles       INTEGER,
  remain_mah   INTEGER,
  full_mah     INTEGER
);
CREATE TABLE IF NOT EXISTS cell_samples (
  ts           INTEGER NOT NULL,
  cell_idx     INTEGER NOT NULL,
  mv           INTEGER NOT NULL,
  PRIMARY KEY (ts, cell_idx)
);
CREATE TABLE IF NOT EXISTS temp_samples (
  ts           INTEGER NOT NULL,
  probe        TEXT NOT NULL,
  deci_c       INTEGER NOT NULL,
  PRIMARY KEY (ts, probe)
);
CREATE INDEX IF NOT EXISTS idx_samples_ts ON samples(ts);
CREATE INDEX IF NOT EXISTS idx_cell_samples_ts ON cell_samples(ts);
`

// Store is a single-writer handle to the SQLite database. The in-memory write
// buffer (FR-4) lives alongside db; this file only manages db's lifecycle, while
// writer.go drains buf in Flush. rdb is a separate read-only handle (FR-2) so
// Slice 3's API reads run on their own connections, concurrent with the writer's
// single connection (WAL allows many readers + one writer); reader.go uses it.
type Store struct {
	db  *sql.DB
	rdb *sql.DB
	buf []bms.PackSample
}

// Open creates the parent directory if needed, opens the database in WAL mode,
// verifies WAL is active, and applies the schema. WAL is mandatory so Slice 3
// can read while the writer writes (requirements.md:240). PRAGMAs are passed in
// the DSN, which modernc.org/sqlite reads from _pragma= query params.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("store: create db dir: %w", err)
	}

	dsn := "file:" + path + "?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" + // NORMAL, not FULL (requirements.md:395)
		"&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1) // single writer; Slice 3 opens its own read handle

	// Verify WAL actually took effect; a silent fallback to the rollback journal
	// would break concurrent reads in Slice 3 (AC-8).
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: query journal_mode: %w", err)
	}
	if mode != "wal" {
		db.Close()
		return nil, fmt.Errorf("store: journal_mode = %q, want wal", mode)
	}

	if _, err := db.Exec(ddl); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}

	// Open a second, read-only handle on the same file now that the writer has
	// created it and applied the schema (FR-2). mode=ro + query_only make writes
	// through this handle impossible; the default connection pool is left in place
	// so concurrent API reads don't serialize behind the writer's single conn.
	rdsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=query_only(true)&mode=ro"
	rdb, err := sql.Open("sqlite", rdsn)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("store: open read handle %s: %w", path, err)
	}

	return &Store{db: db, rdb: rdb}, nil
}

// Close closes the read-only handle first (FR-2), then checkpoint-truncates the
// WAL so the -wal sidecar is emptied on graceful shutdown (keeps the on-disk
// footprint clean for teardown, see Risks) and closes the writer. Closing rdb
// before db ensures no read connection lingers on the file during the writer's
// checkpoint-and-close.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	if s.rdb != nil {
		if err := s.rdb.Close(); err != nil {
			s.db.Close()
			return fmt.Errorf("store: close read handle: %w", err)
		}
	}
	if _, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		s.db.Close()
		return fmt.Errorf("store: wal checkpoint: %w", err)
	}
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("store: close db: %w", err)
	}
	return nil
}
