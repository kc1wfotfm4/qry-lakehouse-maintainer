package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kc1wfotfm4/qry-lakehouse-maintainer/internal/store"
)

// handler bundles the HTTP entry points so they can share the store.
type handler struct {
	st *store.Store
}

func writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

func writeInternalError(c *gin.Context) {
	writeError(c, http.StatusInternalServerError, "internal_error", "an internal error occurred")
}

type registerSnapshotRequest struct {
	SnapshotID    *string `json:"snapshotId"`
	FileCount     *int64  `json:"fileCount"`
	RetentionDays *int64  `json:"retentionDays"`
	SnapshotTime  *string `json:"snapshotTime"`
}

type snapshotResponse struct {
	SnapshotID    string `json:"snapshotId"`
	FileCount     int64  `json:"fileCount"`
	RetentionDays int64  `json:"retentionDays"`
	SnapshotTime  string `json:"snapshotTime"`
}

func newSnapshotResponse(snap store.Snapshot) snapshotResponse {
	return snapshotResponse{
		SnapshotID:    snap.SnapshotID,
		FileCount:     snap.FileCount,
		RetentionDays: snap.RetentionDays,
		SnapshotTime:  snap.SnapshotTime,
	}
}

func (h *handler) registerSnapshot(c *gin.Context) {
	var req registerSnapshotRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "invalid_request", "request body must be a JSON object with snapshotId, fileCount, retentionDays and snapshotTime")
		return
	}
	if req.SnapshotID == nil || *req.SnapshotID == "" {
		writeError(c, http.StatusBadRequest, "invalid_request", "snapshotId is required and must be a non-empty string")
		return
	}
	if req.FileCount == nil {
		writeError(c, http.StatusBadRequest, "invalid_request", "fileCount is required")
		return
	}
	if *req.FileCount < 0 {
		writeError(c, http.StatusBadRequest, "invalid_request", "fileCount must be a non-negative integer")
		return
	}
	if req.RetentionDays == nil {
		writeError(c, http.StatusBadRequest, "invalid_request", "retentionDays is required")
		return
	}
	if *req.RetentionDays <= 0 {
		writeError(c, http.StatusBadRequest, "invalid_retention_days", "retentionDays must be a positive integer")
		return
	}
	if req.SnapshotTime == nil || *req.SnapshotTime == "" {
		writeError(c, http.StatusBadRequest, "invalid_request", "snapshotTime is required")
		return
	}
	takenAt, err := time.Parse(time.RFC3339, *req.SnapshotTime)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid_request", "snapshotTime must be an RFC3339 timestamp")
		return
	}

	snap := store.Snapshot{
		SnapshotID:    *req.SnapshotID,
		TableID:       c.Param("id"),
		FileCount:     *req.FileCount,
		RetentionDays: *req.RetentionDays,
		SnapshotTime:  *req.SnapshotTime,
		TakenAt:       takenAt,
	}
	if err := h.st.RegisterSnapshot(snap); err != nil {
		if errors.Is(err, store.ErrSnapshotExists) {
			writeError(c, http.StatusConflict, "snapshot_exists", "a snapshot with this snapshotId already exists")
			return
		}
		writeInternalError(c)
		return
	}
	c.JSON(http.StatusCreated, newSnapshotResponse(snap))
}

func (h *handler) listExpiredSnapshots(c *gin.Context) {
	tableID := c.Param("id")
	exists, err := h.st.TableExists(tableID)
	if err != nil {
		writeInternalError(c)
		return
	}
	if !exists {
		writeError(c, http.StatusNotFound, "table_not_found", "lake table not found")
		return
	}

	snapshots, err := h.st.ListExpired(tableID, time.Now())
	if err != nil {
		writeInternalError(c)
		return
	}
	response := make([]snapshotResponse, 0, len(snapshots))
	for _, snap := range snapshots {
		response = append(response, newSnapshotResponse(snap))
	}
	c.JSON(http.StatusOK, response)
}

type createMaintenanceRequest struct {
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

func newMaintenanceResponse(task *store.MaintenanceTask) maintenanceResponse {
	snapshotIDs := task.SnapshotIDs
	if snapshotIDs == nil {
		snapshotIDs = make([]string, 0)
	}
	return maintenanceResponse{
		TaskID:      task.TaskID,
		TableID:     task.TableID,
		SnapshotIDs: snapshotIDs,
		Status:      task.Status,
		CreatedAt:   task.CreatedAt,
	}
}

func (h *handler) createMaintenanceTask(c *gin.Context) {
	var req createMaintenanceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "invalid_request", "request body must be a JSON object with tableId and snapshotIds")
		return
	}
	if req.TableID == nil || *req.TableID == "" {
		writeError(c, http.StatusBadRequest, "invalid_request", "tableId is required and must be a non-empty string")
		return
	}
	if req.SnapshotIDs == nil {
		writeError(c, http.StatusBadRequest, "invalid_request", "snapshotIds is required")
		return
	}
	if len(*req.SnapshotIDs) == 0 {
		writeError(c, http.StatusBadRequest, "empty_snapshot_ids", "snapshotIds must contain at least one snapshot id")
		return
	}

	task, err := h.st.CreateMaintenanceTask(*req.TableID, *req.SnapshotIDs, time.Now())
	if err != nil {
		switch {
		case errors.Is(err, store.ErrTableNotFound):
			writeError(c, http.StatusNotFound, "table_not_found", "lake table not found")
		case errors.Is(err, store.ErrSnapshotNotFound):
			writeError(c, http.StatusNotFound, "snapshot_not_found", "snapshot not found")
		case errors.Is(err, store.ErrSnapshotNotEligible):
			writeError(c, http.StatusConflict, "snapshot_not_eligible", "snapshot does not belong to the table or is not expired")
		case errors.Is(err, store.ErrSnapshotAlreadyScheduled):
			writeError(c, http.StatusConflict, "snapshot_already_scheduled", "snapshot already has a completed maintenance task")
		default:
			writeInternalError(c)
		}
		return
	}
	c.JSON(http.StatusCreated, newMaintenanceResponse(task))
}

func (h *handler) getMaintenanceTask(c *gin.Context) {
	task, err := h.st.GetMaintenanceTask(c.Param("taskId"))
	if err != nil {
		if errors.Is(err, store.ErrTaskNotFound) {
			writeError(c, http.StatusNotFound, "task_not_found", "maintenance task not found")
			return
		}
		writeInternalError(c)
		return
	}
	c.JSON(http.StatusOK, newMaintenanceResponse(task))
}
