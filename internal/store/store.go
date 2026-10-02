// Package store owns the SQLite file and every write the service performs.
package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite handle so callers never touch database/sql directly.
type Store struct {
	db *sql.DB
}

// Open prepares the database file and the schema this service needs.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable wal: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Ping reports whether the storage layer is usable.
func (s *Store) Ping() error { return s.db.Ping() }

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS service_metadata (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS lake_tables (
	id         TEXT PRIMARY KEY,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS snapshots (
	snapshot_id    TEXT PRIMARY KEY,
	table_id       TEXT NOT NULL REFERENCES lake_tables (id),
	file_count     INTEGER NOT NULL,
	retention_days INTEGER NOT NULL,
	snapshot_time  TEXT NOT NULL,
	cleaned_at     TEXT,
	created_at     TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_snapshots_table ON snapshots (table_id);

CREATE TABLE IF NOT EXISTS maintenance_tasks (
	task_id    TEXT PRIMARY KEY,
	table_id   TEXT NOT NULL REFERENCES lake_tables (id),
	status     TEXT NOT NULL,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS maintenance_task_snapshots (
	task_id     TEXT NOT NULL REFERENCES maintenance_tasks (task_id),
	snapshot_id TEXT NOT NULL REFERENCES snapshots (snapshot_id),
	position    INTEGER NOT NULL,
	PRIMARY KEY (task_id, snapshot_id)
);
`
