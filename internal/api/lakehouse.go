package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kc1wfotfm4/qry-lakehouse-maintainer/internal/store"
)

type snapshotRequest struct {
	SnapshotID    *string `json:"snapshotId"`
	FileCount     *int64  `json:"fileCount"`
	RetentionDays *int64  `json:"retentionDays"`
	SnapshotTime  *string `json:"snapshotTime"`
}

type snapshotResponse struct {
	SnapshotID    string `json:"snapshotId"`
	TableID       string `json:"tableId"`
	FileCount     int64  `json:"fileCount"`
	RetentionDays int64  `json:"retentionDays"`
	SnapshotTime  string `json:"snapshotTime"`
}

type maintenanceRequest struct {
	TableID     *string   `json:"tableId"`
	SnapshotIDs *[]string `json:"snapshotIds"`
}

type maintenanceResponse struct {
	TaskID      string   `json:"taskId"`
	TableID     string   `json:"tableId"`
	SnapshotIDs []string `json:"snapshotIds"`
	Status      string   `json:"status"`
	CreatedAt   string   `json:"createdAt"`
}

func respondError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

func respondInternalError(c *gin.Context) {
	respondError(c, http.StatusInternalServerError, "internal_error", "the service could not complete the request")
}

func toSnapshotResponse(snap store.Snapshot) snapshotResponse {
	return snapshotResponse{
		SnapshotID:    snap.SnapshotID,
		TableID:       snap.TableID,
		FileCount:     snap.FileCount,
		RetentionDays: snap.RetentionDays,
		SnapshotTime:  snap.SnapshotTime,
	}
}

func toMaintenanceResponse(task store.MaintenanceTask) maintenanceResponse {
	return maintenanceResponse{
		TaskID:      task.TaskID,
		TableID:     task.TableID,
		SnapshotIDs: task.SnapshotIDs,
		Status:      task.Status,
		CreatedAt:   task.CreatedAt,
	}
}

// registerSnapshot handles POST /api/v1/lake-tables/:id/snapshots.
func registerSnapshot(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		tableID := c.Param("id")

		var req snapshotRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			respondError(c, http.StatusBadRequest, "invalid_request", "request body is missing, malformed, or has invalid field types")
			return
		}
		if req.SnapshotID == nil || req.FileCount == nil || req.RetentionDays == nil || req.SnapshotTime == nil {
			respondError(c, http.StatusBadRequest, "invalid_request", "snapshotId, fileCount, retentionDays, and snapshotTime are required")
			return
		}
		if *req.SnapshotID == "" {
			respondError(c, http.StatusBadRequest, "invalid_request", "snapshotId must be a non-empty string")
			return
		}
		if *req.FileCount < 0 {
			respondError(c, http.StatusBadRequest, "invalid_request", "fileCount must be a non-negative integer")
			return
		}
		if *req.RetentionDays <= 0 {
			respondError(c, http.StatusBadRequest, "invalid_retention_days", "retentionDays must be a positive integer")
			return
		}
		if _, err := time.Parse(time.RFC3339, *req.SnapshotTime); err != nil {
			respondError(c, http.StatusBadRequest, "invalid_request", "snapshotTime must be an RFC3339 timestamp")
			return
		}

		snap := store.Snapshot{
			SnapshotID:    *req.SnapshotID,
			TableID:       tableID,
			FileCount:     *req.FileCount,
			RetentionDays: *req.RetentionDays,
			SnapshotTime:  *req.SnapshotTime,
		}
		if err := st.RegisterSnapshot(snap); err != nil {
			if errors.Is(err, store.ErrSnapshotExists) {
				respondError(c, http.StatusConflict, "snapshot_exists", "a snapshot with this snapshotId already exists")
				return
			}
			respondInternalError(c)
			return
		}
		c.JSON(http.StatusCreated, toSnapshotResponse(snap))
	}
}

// listExpiredSnapshots handles GET /api/v1/lake-tables/:id/expired.
func listExpiredSnapshots(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		tableID := c.Param("id")

		exists, err := st.TableExists(tableID)
		if err != nil {
			respondInternalError(c)
			return
		}
		if !exists {
			respondError(c, http.StatusNotFound, "table_not_found", "lake table does not exist")
			return
		}

		expired, err := st.ListExpiredSnapshots(tableID, time.Now())
		if err != nil {
			respondInternalError(c)
			return
		}
		response := make([]snapshotResponse, 0, len(expired))
		for _, snap := range expired {
			response = append(response, toSnapshotResponse(snap))
		}
		c.JSON(http.StatusOK, response)
	}
}

// createMaintenance handles POST /api/v1/maintenance.
func createMaintenance(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req maintenanceRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			respondError(c, http.StatusBadRequest, "invalid_request", "request body is missing, malformed, or has invalid field types")
			return
		}
		if req.TableID == nil {
			respondError(c, http.StatusBadRequest, "invalid_request", "tableId is required")
			return
		}
		if req.SnapshotIDs == nil || len(*req.SnapshotIDs) == 0 {
			respondError(c, http.StatusBadRequest, "empty_snapshot_ids", "snapshotIds must contain at least one snapshot id")
			return
		}

		exists, err := st.TableExists(*req.TableID)
		if err != nil {
			respondInternalError(c)
			return
		}
		if !exists {
			respondError(c, http.StatusNotFound, "table_not_found", "lake table does not exist")
			return
		}

		now := time.Now()
		snaps := make([]store.Snapshot, 0, len(*req.SnapshotIDs))
		for _, snapshotID := range dedupeStrings(*req.SnapshotIDs) {
			snap, err := st.GetSnapshot(snapshotID)
			if err != nil {
				if errors.Is(err, store.ErrSnapshotNotFound) {
					respondError(c, http.StatusNotFound, "snapshot_not_found", "snapshot does not exist")
					return
				}
				respondInternalError(c)
				return
			}
			if snap.TableID != *req.TableID {
				respondError(c, http.StatusConflict, "snapshot_not_eligible", "snapshot does not belong to the given table")
				return
			}
			scheduled, err := st.HasCompletedTask(snapshotID)
			if err != nil {
				respondInternalError(c)
				return
			}
			if scheduled {
				respondError(c, http.StatusConflict, "snapshot_already_scheduled", "snapshot already has a completed maintenance task")
				return
			}
			if snap.Cleaned || !snap.ExpiresAt().Before(now) {
				respondError(c, http.StatusConflict, "snapshot_not_eligible", "snapshot is not expired or is already cleaned")
				return
			}
			snaps = append(snaps, snap)
		}

		task, err := st.CreateMaintenanceTask(snaps)
		if err != nil {
			if errors.Is(err, store.ErrSnapshotAlreadyScheduled) {
				respondError(c, http.StatusConflict, "snapshot_already_scheduled", "snapshot already has a completed maintenance task")
				return
			}
			respondInternalError(c)
			return
		}
		c.JSON(http.StatusCreated, toMaintenanceResponse(task))
	}
}

// getMaintenance handles GET /api/v1/maintenance/:taskId.
func getMaintenance(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		task, err := st.GetMaintenanceTask(c.Param("taskId"))
		if err != nil {
			if errors.Is(err, store.ErrTaskNotFound) {
				respondError(c, http.StatusNotFound, "task_not_found", "maintenance task does not exist")
				return
			}
			respondInternalError(c)
			return
		}
		c.JSON(http.StatusOK, toMaintenanceResponse(task))
	}
}

// dedupeStrings keeps the first occurrence of every id so a repeated id is
// scheduled once while the submission order is preserved.
func dedupeStrings(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	return unique
}
