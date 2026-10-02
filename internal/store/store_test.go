package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenCreatesUsableStore(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if err := st.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestRegisterSnapshotCreatesTableAndRejectsDuplicates(t *testing.T) {
	st := openTestStore(t)

	snap := Snapshot{
		SnapshotID:    "snap-1",
		TableID:       "tbl-1",
		FileCount:     4,
		RetentionDays: 7,
		SnapshotTime:  "2026-09-01T10:00:00Z",
	}
	if err := st.RegisterSnapshot(snap); err != nil {
		t.Fatalf("register: %v", err)
	}
	exists, err := st.TableExists("tbl-1")
	if err != nil || !exists {
		t.Fatalf("table exists = %v, err = %v", exists, err)
	}
	if err := st.RegisterSnapshot(snap); !errors.Is(err, ErrSnapshotExists) {
		t.Fatalf("duplicate err = %v, want ErrSnapshotExists", err)
	}
}

func TestListExpiredSnapshotsFiltersCleanedAndSorts(t *testing.T) {
	st := openTestStore(t)
	now := time.Now()

	register := func(id, tableID, snapshotTime string) {
		t.Helper()
		if err := st.RegisterSnapshot(Snapshot{
			SnapshotID:    id,
			TableID:       tableID,
			FileCount:     1,
			RetentionDays: 1,
			SnapshotTime:  snapshotTime,
		}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	register("snap-new", "tbl-1", "2022-01-01T00:00:00Z")
	register("snap-old", "tbl-1", "2020-01-01T00:00:00Z")
	register("snap-fresh", "tbl-1", now.Add(24*time.Hour).UTC().Format(time.RFC3339))

	if _, err := st.CreateMaintenanceTask([]Snapshot{{
		SnapshotID:    "snap-new",
		TableID:       "tbl-1",
		RetentionDays: 1,
		SnapshotTime:  "2022-01-01T00:00:00Z",
	}}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	expired, err := st.ListExpiredSnapshots("tbl-1", now)
	if err != nil {
		t.Fatalf("list expired: %v", err)
	}
	if len(expired) != 1 || expired[0].SnapshotID != "snap-old" {
		t.Fatalf("expired = %+v, want only snap-old", expired)
	}
}

func TestMaintenanceTaskRoundTripAndConflict(t *testing.T) {
	st := openTestStore(t)
	snaps := []Snapshot{
		{SnapshotID: "snap-b", TableID: "tbl-1", RetentionDays: 1, SnapshotTime: "2021-01-01T00:00:00Z"},
		{SnapshotID: "snap-a", TableID: "tbl-1", RetentionDays: 1, SnapshotTime: "2020-01-01T00:00:00Z"},
	}
	for _, snap := range snaps {
		if err := st.RegisterSnapshot(snap); err != nil {
			t.Fatalf("register: %v", err)
		}
	}

	task, err := st.CreateMaintenanceTask(snaps)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if task.Status != TaskStatusCompleted || task.TableID != "tbl-1" || len(task.SnapshotIDs) != 2 {
		t.Fatalf("unexpected task: %+v", task)
	}

	fetched, err := st.GetMaintenanceTask(task.TaskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if fetched.TaskID != task.TaskID || fetched.Status != TaskStatusCompleted ||
		fetched.SnapshotIDs[0] != "snap-b" || fetched.SnapshotIDs[1] != "snap-a" {
		t.Fatalf("fetched = %+v", fetched)
	}

	for _, snap := range snaps {
		loaded, err := st.GetSnapshot(snap.SnapshotID)
		if err != nil {
			t.Fatalf("get snapshot: %v", err)
		}
		if !loaded.Cleaned {
			t.Fatalf("snapshot %s not marked cleaned", snap.SnapshotID)
		}
		scheduled, err := st.HasCompletedTask(snap.SnapshotID)
		if err != nil || !scheduled {
			t.Fatalf("has completed task = %v, err = %v", scheduled, err)
		}
	}

	if _, err := st.CreateMaintenanceTask(snaps); !errors.Is(err, ErrSnapshotAlreadyScheduled) {
		t.Fatalf("reclean err = %v, want ErrSnapshotAlreadyScheduled", err)
	}
	if _, err := st.GetMaintenanceTask("missing"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing task err = %v, want ErrTaskNotFound", err)
	}
}
