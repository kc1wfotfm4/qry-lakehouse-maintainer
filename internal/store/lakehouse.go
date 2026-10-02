package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
)

// Sentinel errors mapped to the published API error codes by the api layer.
var (
	ErrSnapshotExists           = errors.New("snapshot already exists")
	ErrSnapshotNotFound         = errors.New("snapshot not found")
	ErrSnapshotAlreadyScheduled = errors.New("snapshot already has a completed maintenance task")
	ErrTaskNotFound             = errors.New("maintenance task not found")
)

// TaskStatusCompleted marks a maintenance task that finished cleaning its snapshots.
const TaskStatusCompleted = "completed"

// Snapshot is one registered lake-table snapshot. SnapshotTime keeps the exact
// RFC3339 text the caller registered so responses echo it unchanged.
type Snapshot struct {
	SnapshotID    string
	TableID       string
	FileCount     int64
	RetentionDays int64
	SnapshotTime  string
	Cleaned       bool
}

// ExpiresAt returns the instant after which the snapshot counts as expired.
func (s Snapshot) ExpiresAt() time.Time {
	taken, err := time.Parse(time.RFC3339, s.SnapshotTime)
	if err != nil {
		return time.Time{}
	}
	return taken.AddDate(0, 0, int(s.RetentionDays))
}

// MaintenanceTask is one cleanup run over a set of snapshots.
type MaintenanceTask struct {
	TaskID      string
	TableID     string
	SnapshotIDs []string
	Status      string
	CreatedAt   string
}

// RegisterSnapshot creates the lake table on first use and stores the snapshot.
// It returns ErrSnapshotExists when the snapshot id is already registered.
func (s *Store) RegisterSnapshot(snap Snapshot) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin register snapshot: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(
		`INSERT INTO lake_tables (id, created_at) VALUES (?, ?) ON CONFLICT (id) DO NOTHING`,
		snap.TableID, now,
	); err != nil {
		return fmt.Errorf("ensure lake table: %w", err)
	}

	var existing int
	if err := tx.QueryRow(
		`SELECT COUNT(1) FROM snapshots WHERE snapshot_id = ?`, snap.SnapshotID,
	).Scan(&existing); err != nil {
		return fmt.Errorf("check snapshot: %w", err)
	}
	if existing > 0 {
		return ErrSnapshotExists
	}

	if _, err := tx.Exec(
		`INSERT INTO snapshots (snapshot_id, table_id, file_count, retention_days, snapshot_time, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		snap.SnapshotID, snap.TableID, snap.FileCount, snap.RetentionDays, snap.SnapshotTime, now,
	); err != nil {
		return fmt.Errorf("insert snapshot: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit snapshot: %w", err)
	}
	return nil
}

// TableExists reports whether a lake table has been registered.
func (s *Store) TableExists(tableID string) (bool, error) {
	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(1) FROM lake_tables WHERE id = ?`, tableID,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("check lake table: %w", err)
	}
	return count > 0, nil
}

// GetSnapshot loads one snapshot by id or returns ErrSnapshotNotFound.
func (s *Store) GetSnapshot(snapshotID string) (Snapshot, error) {
	var snap Snapshot
	var cleanedAt sql.NullString
	err := s.db.QueryRow(
		`SELECT snapshot_id, table_id, file_count, retention_days, snapshot_time, cleaned_at
		 FROM snapshots WHERE snapshot_id = ?`, snapshotID,
	).Scan(&snap.SnapshotID, &snap.TableID, &snap.FileCount, &snap.RetentionDays, &snap.SnapshotTime, &cleanedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, ErrSnapshotNotFound
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("load snapshot: %w", err)
	}
	snap.Cleaned = cleanedAt.Valid
	return snap, nil
}

// ListExpiredSnapshots returns the table's uncleaned snapshots whose retention
// has elapsed by now, ordered from oldest to newest snapshot time.
func (s *Store) ListExpiredSnapshots(tableID string, now time.Time) ([]Snapshot, error) {
	rows, err := s.db.Query(
		`SELECT snapshot_id, table_id, file_count, retention_days, snapshot_time
		 FROM snapshots WHERE table_id = ? AND cleaned_at IS NULL`, tableID,
	)
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	defer rows.Close()

	expired := make([]Snapshot, 0)
	for rows.Next() {
		var snap Snapshot
		if err := rows.Scan(&snap.SnapshotID, &snap.TableID, &snap.FileCount, &snap.RetentionDays, &snap.SnapshotTime); err != nil {
			return nil, fmt.Errorf("scan snapshot: %w", err)
		}
		if snap.ExpiresAt().Before(now) {
			expired = append(expired, snap)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate snapshots: %w", err)
	}
	sortSnapshotsByTime(expired)
	return expired, nil
}

// HasCompletedTask reports whether the snapshot already belongs to a completed
// maintenance task.
func (s *Store) HasCompletedTask(snapshotID string) (bool, error) {
	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(1) FROM maintenance_task_snapshots ts
		 JOIN maintenance_tasks t ON t.task_id = ts.task_id
		 WHERE ts.snapshot_id = ? AND t.status = ?`, snapshotID, TaskStatusCompleted,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("check snapshot tasks: %w", err)
	}
	return count > 0, nil
}

// CreateMaintenanceTask records one completed cleanup task and marks every
// snapshot cleaned in a single transaction, oldest snapshot first. Any failure
// rolls the whole batch back. It returns ErrSnapshotAlreadyScheduled when a
// snapshot was cleaned concurrently.
func (s *Store) CreateMaintenanceTask(snaps []Snapshot) (MaintenanceTask, error) {
	if len(snaps) == 0 {
		return MaintenanceTask{}, errors.New("no snapshots to clean")
	}
	task := MaintenanceTask{
		TaskID:    uuid.NewString(),
		TableID:   snaps[0].TableID,
		Status:    TaskStatusCompleted,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}

	tx, err := s.db.Begin()
	if err != nil {
		return MaintenanceTask{}, fmt.Errorf("begin maintenance task: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO maintenance_tasks (task_id, table_id, status, created_at) VALUES (?, ?, ?, ?)`,
		task.TaskID, task.TableID, task.Status, task.CreatedAt,
	); err != nil {
		return MaintenanceTask{}, fmt.Errorf("insert maintenance task: %w", err)
	}

	task.SnapshotIDs = make([]string, 0, len(snaps))
	for position, snap := range snaps {
		if _, err := tx.Exec(
			`INSERT INTO maintenance_task_snapshots (task_id, snapshot_id, position) VALUES (?, ?, ?)`,
			task.TaskID, snap.SnapshotID, position,
		); err != nil {
			return MaintenanceTask{}, fmt.Errorf("link snapshot to task: %w", err)
		}
		task.SnapshotIDs = append(task.SnapshotIDs, snap.SnapshotID)
	}

	cleaning := make([]Snapshot, len(snaps))
	copy(cleaning, snaps)
	sortSnapshotsByTime(cleaning)
	for _, snap := range cleaning {
		res, err := tx.Exec(
			`UPDATE snapshots SET cleaned_at = ? WHERE snapshot_id = ? AND cleaned_at IS NULL`,
			task.CreatedAt, snap.SnapshotID,
		)
		if err != nil {
			return MaintenanceTask{}, fmt.Errorf("mark snapshot cleaned: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return MaintenanceTask{}, fmt.Errorf("check cleaned snapshot: %w", err)
		}
		if affected == 0 {
			return MaintenanceTask{}, ErrSnapshotAlreadyScheduled
		}
	}

	if err := tx.Commit(); err != nil {
		return MaintenanceTask{}, fmt.Errorf("commit maintenance task: %w", err)
	}
	return task, nil
}

// GetMaintenanceTask loads one task with its snapshot ids in submission order,
// or returns ErrTaskNotFound.
func (s *Store) GetMaintenanceTask(taskID string) (MaintenanceTask, error) {
	var task MaintenanceTask
	err := s.db.QueryRow(
		`SELECT task_id, table_id, status, created_at FROM maintenance_tasks WHERE task_id = ?`, taskID,
	).Scan(&task.TaskID, &task.TableID, &task.Status, &task.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return MaintenanceTask{}, ErrTaskNotFound
	}
	if err != nil {
		return MaintenanceTask{}, fmt.Errorf("load maintenance task: %w", err)
	}

	rows, err := s.db.Query(
		`SELECT snapshot_id FROM maintenance_task_snapshots WHERE task_id = ? ORDER BY position`, taskID,
	)
	if err != nil {
		return MaintenanceTask{}, fmt.Errorf("list task snapshots: %w", err)
	}
	defer rows.Close()

	task.SnapshotIDs = make([]string, 0)
	for rows.Next() {
		var snapshotID string
		if err := rows.Scan(&snapshotID); err != nil {
			return MaintenanceTask{}, fmt.Errorf("scan task snapshot: %w", err)
		}
		task.SnapshotIDs = append(task.SnapshotIDs, snapshotID)
	}
	if err := rows.Err(); err != nil {
		return MaintenanceTask{}, fmt.Errorf("iterate task snapshots: %w", err)
	}
	return task, nil
}

// sortSnapshotsByTime orders snapshots from oldest to newest snapshot time,
// breaking ties by id so the order is deterministic.
func sortSnapshotsByTime(snaps []Snapshot) {
	sort.SliceStable(snaps, func(i, j int) bool {
		left, leftErr := time.Parse(time.RFC3339, snaps[i].SnapshotTime)
		right, rightErr := time.Parse(time.RFC3339, snaps[j].SnapshotTime)
		if leftErr != nil || rightErr != nil || left.Equal(right) {
			return snaps[i].SnapshotID < snaps[j].SnapshotID
		}
		return left.Before(right)
	})
}
