// Package store owns the SQLite file and every write the service performs.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// Store wraps the SQLite handle so callers never touch database/sql directly.
type Store struct {
	db *sql.DB
}

// Sentinel errors the API layer maps to the published error codes.
var (
	ErrSnapshotExists           = errors.New("snapshot already registered")
	ErrTableNotFound            = errors.New("lake table not found")
	ErrSnapshotNotFound         = errors.New("snapshot not found")
	ErrSnapshotNotEligible      = errors.New("snapshot not eligible for maintenance")
	ErrSnapshotAlreadyScheduled = errors.New("snapshot already has a completed maintenance task")
	ErrTaskNotFound             = errors.New("maintenance task not found")
)

// Snapshot is one registered data-lake snapshot.
type Snapshot struct {
	SnapshotID    string
	TableID       string
	FileCount     int64
	RetentionDays int64
	SnapshotTime  string    // RFC3339, exactly as registered
	TakenAt       time.Time // parsed SnapshotTime
}

// MaintenanceTask is one cleanup run over expired snapshots.
type MaintenanceTask struct {
	TaskID      string
	TableID     string
	SnapshotIDs []string
	Status      string
	CreatedAt   string // RFC3339
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

// RegisterSnapshot lazily creates the lake table and records the snapshot. It
// returns ErrSnapshotExists when the snapshot id is already registered.
func (s *Store) RegisterSnapshot(snap Snapshot) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin register snapshot: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`INSERT OR IGNORE INTO lake_tables (table_id, created_at) VALUES (?, ?)`,
		snap.TableID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("ensure lake table: %w", err)
	}

	var existing int
	if err := tx.QueryRow(`SELECT COUNT(1) FROM lake_snapshots WHERE snapshot_id = ?`, snap.SnapshotID).Scan(&existing); err != nil {
		return fmt.Errorf("check snapshot: %w", err)
	}
	if existing > 0 {
		return ErrSnapshotExists
	}

	if _, err := tx.Exec(`INSERT INTO lake_snapshots
		(snapshot_id, table_id, file_count, retention_days, snapshot_time, snapshot_nanos, expires_nanos, cleaned, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		snap.SnapshotID, snap.TableID, snap.FileCount, snap.RetentionDays, snap.SnapshotTime,
		snap.TakenAt.UTC().UnixNano(), expiryNanos(snap.TakenAt, snap.RetentionDays),
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		var duplicates int
		if checkErr := tx.QueryRow(`SELECT COUNT(1) FROM lake_snapshots WHERE snapshot_id = ?`, snap.SnapshotID).Scan(&duplicates); checkErr == nil && duplicates > 0 {
			return ErrSnapshotExists
		}
		return fmt.Errorf("insert snapshot: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit snapshot: %w", err)
	}
	return nil
}

// TableExists reports whether the lake table has been registered.
func (s *Store) TableExists(tableID string) (bool, error) {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM lake_tables WHERE table_id = ?`, tableID).Scan(&count); err != nil {
		return false, fmt.Errorf("check lake table: %w", err)
	}
	return count > 0, nil
}

// ListExpired returns the table's uncleaned snapshots whose retention elapsed
// before now, ordered from oldest to newest snapshot time.
func (s *Store) ListExpired(tableID string, now time.Time) ([]Snapshot, error) {
	rows, err := s.db.Query(`SELECT snapshot_id, table_id, file_count, retention_days, snapshot_time
		FROM lake_snapshots
		WHERE table_id = ? AND cleaned = 0 AND expires_nanos < ?
		ORDER BY snapshot_nanos ASC, snapshot_id ASC`, tableID, now.UTC().UnixNano())
	if err != nil {
		return nil, fmt.Errorf("list expired snapshots: %w", err)
	}
	defer rows.Close()

	snapshots := make([]Snapshot, 0)
	for rows.Next() {
		var snap Snapshot
		if err := rows.Scan(&snap.SnapshotID, &snap.TableID, &snap.FileCount, &snap.RetentionDays, &snap.SnapshotTime); err != nil {
			return nil, fmt.Errorf("scan expired snapshot: %w", err)
		}
		snapshots = append(snapshots, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired snapshots: %w", err)
	}
	return snapshots, nil
}

// CreateMaintenanceTask validates the snapshots against the table, records a
// completed task and marks the snapshots cleaned from oldest to newest, all in
// a single transaction that rolls back on any failure.
func (s *Store) CreateMaintenanceTask(tableID string, snapshotIDs []string, now time.Time) (*MaintenanceTask, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin maintenance task: %w", err)
	}
	defer tx.Rollback()

	var tables int
	if err := tx.QueryRow(`SELECT COUNT(1) FROM lake_tables WHERE table_id = ?`, tableID).Scan(&tables); err != nil {
		return nil, fmt.Errorf("check lake table: %w", err)
	}
	if tables == 0 {
		return nil, ErrTableNotFound
	}

	type candidate struct {
		id            string
		snapshotNanos int64
	}
	candidates := make([]candidate, 0, len(snapshotIDs))
	seen := make(map[string]bool, len(snapshotIDs))
	nowNanos := now.UTC().UnixNano()
	for _, snapshotID := range snapshotIDs {
		if seen[snapshotID] {
			continue
		}
		seen[snapshotID] = true

		var owner string
		var cleaned int
		var expiresNanos, snapshotNanos int64
		err := tx.QueryRow(`SELECT table_id, cleaned, expires_nanos, snapshot_nanos
			FROM lake_snapshots WHERE snapshot_id = ?`, snapshotID).
			Scan(&owner, &cleaned, &expiresNanos, &snapshotNanos)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSnapshotNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("load snapshot: %w", err)
		}
		if owner != tableID || expiresNanos >= nowNanos {
			return nil, ErrSnapshotNotEligible
		}
		if cleaned != 0 {
			return nil, ErrSnapshotAlreadyScheduled
		}
		candidates = append(candidates, candidate{id: snapshotID, snapshotNanos: snapshotNanos})
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].snapshotNanos != candidates[j].snapshotNanos {
			return candidates[i].snapshotNanos < candidates[j].snapshotNanos
		}
		return candidates[i].id < candidates[j].id
	})

	task := &MaintenanceTask{
		TaskID:    uuid.NewString(),
		TableID:   tableID,
		Status:    "completed",
		CreatedAt: now.UTC().Format(time.RFC3339Nano),
	}
	if _, err := tx.Exec(`INSERT INTO maintenance_tasks (task_id, table_id, status, created_at) VALUES (?, ?, ?, ?)`,
		task.TaskID, task.TableID, task.Status, task.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert maintenance task: %w", err)
	}
	for seq, cand := range candidates {
		if _, err := tx.Exec(`INSERT INTO maintenance_task_snapshots (task_id, seq, snapshot_id) VALUES (?, ?, ?)`,
			task.TaskID, seq, cand.id); err != nil {
			return nil, fmt.Errorf("link snapshot to task: %w", err)
		}
		if _, err := tx.Exec(`UPDATE lake_snapshots SET cleaned = 1 WHERE snapshot_id = ?`, cand.id); err != nil {
			return nil, fmt.Errorf("mark snapshot cleaned: %w", err)
		}
		task.SnapshotIDs = append(task.SnapshotIDs, cand.id)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit maintenance task: %w", err)
	}
	return task, nil
}

// GetMaintenanceTask loads one task with its snapshot ids in processing order.
func (s *Store) GetMaintenanceTask(taskID string) (*MaintenanceTask, error) {
	task := &MaintenanceTask{}
	err := s.db.QueryRow(`SELECT task_id, table_id, status, created_at FROM maintenance_tasks WHERE task_id = ?`, taskID).
		Scan(&task.TaskID, &task.TableID, &task.Status, &task.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTaskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load maintenance task: %w", err)
	}

	rows, err := s.db.Query(`SELECT snapshot_id FROM maintenance_task_snapshots WHERE task_id = ? ORDER BY seq ASC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list task snapshots: %w", err)
	}
	defer rows.Close()

	task.SnapshotIDs = make([]string, 0)
	for rows.Next() {
		var snapshotID string
		if err := rows.Scan(&snapshotID); err != nil {
			return nil, fmt.Errorf("scan task snapshot: %w", err)
		}
		task.SnapshotIDs = append(task.SnapshotIDs, snapshotID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task snapshots: %w", err)
	}
	return task, nil
}

const dayNanos = int64(24 * time.Hour)

// expiryNanos converts snapshot time plus retention into UTC unix nanos,
// clamping absurd retention values instead of overflowing.
func expiryNanos(takenAt time.Time, retentionDays int64) int64 {
	if retentionDays >= math.MaxInt64/dayNanos {
		return math.MaxInt64
	}
	start := takenAt.UTC().UnixNano()
	if expiry := start + retentionDays*dayNanos; expiry >= start {
		return expiry
	}
	return math.MaxInt64
}

const schema = `
CREATE TABLE IF NOT EXISTS service_metadata (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS lake_tables (
	table_id   TEXT PRIMARY KEY,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS lake_snapshots (
	snapshot_id     TEXT PRIMARY KEY,
	table_id        TEXT NOT NULL,
	file_count      INTEGER NOT NULL,
	retention_days  INTEGER NOT NULL,
	snapshot_time   TEXT NOT NULL,
	snapshot_nanos  INTEGER NOT NULL,
	expires_nanos   INTEGER NOT NULL,
	cleaned         INTEGER NOT NULL DEFAULT 0,
	created_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS lake_snapshots_table_expiry ON lake_snapshots (table_id, cleaned, expires_nanos);
CREATE TABLE IF NOT EXISTS maintenance_tasks (
	task_id    TEXT PRIMARY KEY,
	table_id   TEXT NOT NULL,
	status     TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS maintenance_task_snapshots (
	task_id     TEXT NOT NULL,
	seq         INTEGER NOT NULL,
	snapshot_id TEXT NOT NULL,
	PRIMARY KEY (task_id, snapshot_id)
);
`
