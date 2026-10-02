package api

import (
	"net/http"
	"reflect"
	"testing"
	"time"
)

func TestMaintenanceTaskLifecycle(t *testing.T) {
	router := newTestRouter(t)
	registerSnapshot(t, router, "orders", "s-newer", "2020-01-02T00:00:00Z", 1)
	registerSnapshot(t, router, "orders", "s-older", "2020-01-01T00:00:00Z", 1)

	recorder := doRequest(t, router, http.MethodPost, "/api/v1/maintenance",
		`{"tableId":"orders","snapshotIds":["s-newer","s-older"]}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var created maintenanceResponse
	decodeBody(t, recorder, &created)
	if created.TaskID == "" {
		t.Fatalf("taskId is empty: %s", recorder.Body.String())
	}
	if created.TableID != "orders" || created.Status != "completed" {
		t.Fatalf("task = %+v", created)
	}
	if want := []string{"s-older", "s-newer"}; !reflect.DeepEqual(created.SnapshotIDs, want) {
		t.Fatalf("snapshotIds = %v, want %v (oldest first)", created.SnapshotIDs, want)
	}
	if _, err := time.Parse(time.RFC3339, created.CreatedAt); err != nil {
		t.Fatalf("createdAt %q does not parse as RFC3339: %v", created.CreatedAt, err)
	}

	recorder = doRequest(t, router, http.MethodGet, "/api/v1/lake-tables/orders/expired", nil)
	if got := recorder.Body.String(); recorder.Code != http.StatusOK || got != "[]" {
		t.Fatalf("expired after cleanup: status = %d, body = %s", recorder.Code, got)
	}

	recorder = doRequest(t, router, http.MethodGet, "/api/v1/maintenance/"+created.TaskID, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var fetched maintenanceResponse
	decodeBody(t, recorder, &fetched)
	if !reflect.DeepEqual(fetched, created) {
		t.Fatalf("fetched = %+v, want %+v", fetched, created)
	}

	recorder = doRequest(t, router, http.MethodPost, "/api/v1/maintenance",
		`{"tableId":"orders","snapshotIds":["s-older"]}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if code := errorCode(t, recorder); code != "snapshot_already_scheduled" {
		t.Fatalf("code = %q, want snapshot_already_scheduled", code)
	}
}

func TestMaintenanceValidation(t *testing.T) {
	router := newTestRouter(t)
	registerSnapshot(t, router, "orders", "s-expired", "2020-01-01T00:00:00Z", 1)
	registerSnapshot(t, router, "orders", "s-fresh", time.Now().UTC().Format(time.RFC3339), 365)
	registerSnapshot(t, router, "archive", "s-foreign", "2020-01-01T00:00:00Z", 1)

	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"missing table id", `{"snapshotIds":["s-expired"]}`, http.StatusBadRequest, "invalid_request"},
		{"missing snapshot ids", `{"tableId":"orders"}`, http.StatusBadRequest, "invalid_request"},
		{"null snapshot ids", `{"tableId":"orders","snapshotIds":null}`, http.StatusBadRequest, "invalid_request"},
		{"empty snapshot ids", `{"tableId":"orders","snapshotIds":[]}`, http.StatusBadRequest, "empty_snapshot_ids"},
		{"snapshot ids wrong type", `{"tableId":"orders","snapshotIds":[7]}`, http.StatusBadRequest, "invalid_request"},
		{"unknown table", `{"tableId":"ghost","snapshotIds":["s-expired"]}`, http.StatusNotFound, "table_not_found"},
		{"unknown snapshot", `{"tableId":"orders","snapshotIds":["ghost"]}`, http.StatusNotFound, "snapshot_not_found"},
		{"snapshot from other table", `{"tableId":"orders","snapshotIds":["s-foreign"]}`, http.StatusConflict, "snapshot_not_eligible"},
		{"unexpired snapshot", `{"tableId":"orders","snapshotIds":["s-fresh"]}`, http.StatusConflict, "snapshot_not_eligible"},
		{"valid then unexpired", `{"tableId":"orders","snapshotIds":["s-expired","s-fresh"]}`, http.StatusConflict, "snapshot_not_eligible"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := doRequest(t, router, http.MethodPost, "/api/v1/maintenance", tc.body)
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if code := errorCode(t, recorder); code != tc.wantCode {
				t.Fatalf("code = %q, want %q", code, tc.wantCode)
			}
		})
	}

	recorder := doRequest(t, router, http.MethodGet, "/api/v1/lake-tables/orders/expired", nil)
	var body []snapshotResponse
	decodeBody(t, recorder, &body)
	if len(body) != 1 || body[0].SnapshotID != "s-expired" {
		t.Fatalf("failed maintenance must not mark anything cleaned, expired = %+v", body)
	}
}

func TestGetMaintenanceTaskNotFound(t *testing.T) {
	router := newTestRouter(t)

	recorder := doRequest(t, router, http.MethodGet, "/api/v1/maintenance/ghost", nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if code := errorCode(t, recorder); code != "task_not_found" {
		t.Fatalf("code = %q, want task_not_found", code)
	}
}
