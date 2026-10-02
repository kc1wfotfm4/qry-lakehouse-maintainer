package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kc1wfotfm4/qry-lakehouse-maintainer/internal/store"
)

func newTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewRouter(st)
}

func doRequest(t *testing.T, router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	return recorder
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), target); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
}

func errorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decodeBody(t, recorder, &payload)
	if payload.Error.Code == "" || payload.Error.Message == "" {
		t.Fatalf("error payload missing fields: %s", recorder.Body.String())
	}
	return payload.Error.Code
}

func registerSnapshotOK(t *testing.T, router *gin.Engine, tableID, snapshotID, snapshotTime string, retentionDays int64) {
	t.Helper()
	body := fmt.Sprintf(`{"snapshotId":%q,"fileCount":3,"retentionDays":%d,"snapshotTime":%q}`,
		snapshotID, retentionDays, snapshotTime)
	recorder := doRequest(t, router, http.MethodPost, "/api/v1/lake-tables/"+tableID+"/snapshots", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("register %s: status = %d, body = %s", snapshotID, recorder.Code, recorder.Body.String())
	}
}

func TestRegisterSnapshotCreated(t *testing.T) {
	router := newTestRouter(t)

	recorder := doRequest(t, router, http.MethodPost, "/api/v1/lake-tables/tbl-1/snapshots",
		`{"snapshotId":"snap-1","fileCount":12,"retentionDays":7,"snapshotTime":"2026-09-01T10:00:00Z"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var snap snapshotResponse
	decodeBody(t, recorder, &snap)
	if snap.SnapshotID != "snap-1" || snap.TableID != "tbl-1" || snap.FileCount != 12 ||
		snap.RetentionDays != 7 || snap.SnapshotTime != "2026-09-01T10:00:00Z" {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
}

func TestRegisterSnapshotDuplicate(t *testing.T) {
	router := newTestRouter(t)
	registerSnapshotOK(t, router, "tbl-1", "snap-1", "2026-09-01T10:00:00Z", 7)

	recorder := doRequest(t, router, http.MethodPost, "/api/v1/lake-tables/tbl-1/snapshots",
		`{"snapshotId":"snap-1","fileCount":1,"retentionDays":3,"snapshotTime":"2026-09-02T10:00:00Z"}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", recorder.Code)
	}
	if code := errorCode(t, recorder); code != "snapshot_exists" {
		t.Fatalf("code = %s, want snapshot_exists", code)
	}
}

func TestRegisterSnapshotValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		code string
	}{
		{"missing field", `{"snapshotId":"s","fileCount":1,"retentionDays":3}`, "invalid_request"},
		{"wrong type", `{"snapshotId":"s","fileCount":"1","retentionDays":3,"snapshotTime":"2026-09-01T00:00:00Z"}`, "invalid_request"},
		{"negative fileCount", `{"snapshotId":"s","fileCount":-1,"retentionDays":3,"snapshotTime":"2026-09-01T00:00:00Z"}`, "invalid_request"},
		{"empty snapshotId", `{"snapshotId":"","fileCount":1,"retentionDays":3,"snapshotTime":"2026-09-01T00:00:00Z"}`, "invalid_request"},
		{"zero retentionDays", `{"snapshotId":"s","fileCount":1,"retentionDays":0,"snapshotTime":"2026-09-01T00:00:00Z"}`, "invalid_retention_days"},
		{"negative retentionDays", `{"snapshotId":"s","fileCount":1,"retentionDays":-2,"snapshotTime":"2026-09-01T00:00:00Z"}`, "invalid_retention_days"},
		{"bad snapshotTime", `{"snapshotId":"s","fileCount":1,"retentionDays":3,"snapshotTime":"not-a-time"}`, "invalid_request"},
		{"malformed json", `{"snapshotId":`, "invalid_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := newTestRouter(t)
			recorder := doRequest(t, router, http.MethodPost, "/api/v1/lake-tables/tbl-1/snapshots", tc.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", recorder.Code, recorder.Body.String())
			}
			if code := errorCode(t, recorder); code != tc.code {
				t.Fatalf("code = %s, want %s", code, tc.code)
			}
		})
	}
}

func TestListExpiredSnapshotsOrdersAndFilters(t *testing.T) {
	router := newTestRouter(t)
	registerSnapshotOK(t, router, "tbl-1", "snap-mid", "2021-01-01T00:00:00Z", 1)
	registerSnapshotOK(t, router, "tbl-1", "snap-old", "2020-01-01T00:00:00Z", 1)
	registerSnapshotOK(t, router, "tbl-1", "snap-new", "2022-01-01T00:00:00Z", 1)
	registerSnapshotOK(t, router, "tbl-1", "snap-fresh", "2099-01-01T00:00:00Z", 1)
	registerSnapshotOK(t, router, "tbl-2", "snap-other", "2020-01-01T00:00:00Z", 1)

	recorder := doRequest(t, router, http.MethodGet, "/api/v1/lake-tables/tbl-1/expired", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var snaps []snapshotResponse
	decodeBody(t, recorder, &snaps)
	if len(snaps) != 3 {
		t.Fatalf("expired count = %d, want 3: %s", len(snaps), recorder.Body.String())
	}
	want := []string{"snap-old", "snap-mid", "snap-new"}
	for i, id := range want {
		if snaps[i].SnapshotID != id {
			t.Fatalf("expired[%d] = %s, want %s", i, snaps[i].SnapshotID, id)
		}
	}
}

func TestListExpiredSnapshotsEmptyArray(t *testing.T) {
	router := newTestRouter(t)
	registerSnapshotOK(t, router, "tbl-1", "snap-fresh", "2099-01-01T00:00:00Z", 1)

	recorder := doRequest(t, router, http.MethodGet, "/api/v1/lake-tables/tbl-1/expired", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if got := strings.TrimSpace(recorder.Body.String()); got != "[]" {
		t.Fatalf("body = %s, want []", got)
	}
}

func TestListExpiredSnapshotsTableNotFound(t *testing.T) {
	router := newTestRouter(t)
	recorder := doRequest(t, router, http.MethodGet, "/api/v1/lake-tables/nope/expired", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	if code := errorCode(t, recorder); code != "table_not_found" {
		t.Fatalf("code = %s, want table_not_found", code)
	}
}

func TestMaintenanceLifecycle(t *testing.T) {
	router := newTestRouter(t)
	registerSnapshotOK(t, router, "tbl-1", "snap-a", "2020-01-01T00:00:00Z", 1)
	registerSnapshotOK(t, router, "tbl-1", "snap-b", "2021-01-01T00:00:00Z", 1)

	recorder := doRequest(t, router, http.MethodPost, "/api/v1/maintenance",
		`{"tableId":"tbl-1","snapshotIds":["snap-a","snap-b"]}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var created maintenanceResponse
	decodeBody(t, recorder, &created)
	if created.TaskID == "" || created.TableID != "tbl-1" || created.Status != "completed" {
		t.Fatalf("unexpected task: %+v", created)
	}
	if len(created.SnapshotIDs) != 2 || created.SnapshotIDs[0] != "snap-a" || created.SnapshotIDs[1] != "snap-b" {
		t.Fatalf("unexpected snapshotIds: %+v", created.SnapshotIDs)
	}

	recorder = doRequest(t, router, http.MethodGet, "/api/v1/maintenance/"+created.TaskID, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("get task status = %d", recorder.Code)
	}
	var fetched maintenanceResponse
	decodeBody(t, recorder, &fetched)
	if fetched.TaskID != created.TaskID || fetched.TableID != "tbl-1" || fetched.Status != "completed" {
		t.Fatalf("unexpected fetched task: %+v", fetched)
	}
	if len(fetched.SnapshotIDs) != 2 {
		t.Fatalf("fetched snapshotIds = %+v", fetched.SnapshotIDs)
	}
	if _, err := time.Parse(time.RFC3339Nano, fetched.CreatedAt); err != nil {
		t.Fatalf("createdAt %q is not RFC3339: %v", fetched.CreatedAt, err)
	}

	recorder = doRequest(t, router, http.MethodGet, "/api/v1/lake-tables/tbl-1/expired", "")
	if got := strings.TrimSpace(recorder.Body.String()); got != "[]" {
		t.Fatalf("expired after cleanup = %s, want []", got)
	}

	recorder = doRequest(t, router, http.MethodPost, "/api/v1/maintenance",
		`{"tableId":"tbl-1","snapshotIds":["snap-a"]}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("reschedule status = %d, want 409", recorder.Code)
	}
	if code := errorCode(t, recorder); code != "snapshot_already_scheduled" {
		t.Fatalf("code = %s, want snapshot_already_scheduled", code)
	}
}

func TestMaintenanceValidation(t *testing.T) {
	setup := func(t *testing.T) *gin.Engine {
		router := newTestRouter(t)
		registerSnapshotOK(t, router, "tbl-1", "snap-expired", "2020-01-01T00:00:00Z", 1)
		registerSnapshotOK(t, router, "tbl-1", "snap-fresh", "2099-01-01T00:00:00Z", 1)
		registerSnapshotOK(t, router, "tbl-2", "snap-other-table", "2020-01-01T00:00:00Z", 1)
		return router
	}

	cases := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"malformed json", `{"tableId":`, http.StatusBadRequest, "invalid_request"},
		{"missing tableId", `{"snapshotIds":["snap-expired"]}`, http.StatusBadRequest, "invalid_request"},
		{"missing snapshotIds", `{"tableId":"tbl-1"}`, http.StatusBadRequest, "empty_snapshot_ids"},
		{"empty snapshotIds", `{"tableId":"tbl-1","snapshotIds":[]}`, http.StatusBadRequest, "empty_snapshot_ids"},
		{"unknown table", `{"tableId":"nope","snapshotIds":["snap-expired"]}`, http.StatusNotFound, "table_not_found"},
		{"unknown snapshot", `{"tableId":"tbl-1","snapshotIds":["nope"]}`, http.StatusNotFound, "snapshot_not_found"},
		{"wrong table", `{"tableId":"tbl-1","snapshotIds":["snap-other-table"]}`, http.StatusConflict, "snapshot_not_eligible"},
		{"not expired", `{"tableId":"tbl-1","snapshotIds":["snap-fresh"]}`, http.StatusConflict, "snapshot_not_eligible"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := setup(t)
			recorder := doRequest(t, router, http.MethodPost, "/api/v1/maintenance", tc.body)
			if recorder.Code != tc.status {
				t.Fatalf("status = %d, want %d, body = %s", recorder.Code, tc.status, recorder.Body.String())
			}
			if code := errorCode(t, recorder); code != tc.code {
				t.Fatalf("code = %s, want %s", code, tc.code)
			}
		})
	}
}

func TestGetMaintenanceTaskNotFound(t *testing.T) {
	router := newTestRouter(t)
	recorder := doRequest(t, router, http.MethodGet, "/api/v1/maintenance/nope", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	if code := errorCode(t, recorder); code != "task_not_found" {
		t.Fatalf("code = %s, want task_not_found", code)
	}
}
