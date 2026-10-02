package api

import (
	"net/http"
	"reflect"
	"testing"
	"time"
)

func TestRegisterSnapshotReturnsCreatedSnapshot(t *testing.T) {
	router := newTestRouter(t)

	recorder := doRequest(t, router, http.MethodPost, "/api/v1/lake-tables/orders/snapshots",
		`{"snapshotId":"s-1","fileCount":12,"retentionDays":30,"snapshotTime":"2026-01-01T08:00:00+08:00"}`)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	decodeBody(t, recorder, &body)
	want := map[string]any{
		"snapshotId":    "s-1",
		"fileCount":     float64(12),
		"retentionDays": float64(30),
		"snapshotTime":  "2026-01-01T08:00:00+08:00",
	}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("body = %v, want %v", body, want)
	}
}

func TestRegisterSnapshotRejectsDuplicateID(t *testing.T) {
	router := newTestRouter(t)
	registerSnapshot(t, router, "orders", "s-1", "2026-01-01T00:00:00Z", 30)

	recorder := doRequest(t, router, http.MethodPost, "/api/v1/lake-tables/orders/snapshots",
		`{"snapshotId":"s-1","fileCount":4,"retentionDays":7,"snapshotTime":"2026-02-01T00:00:00Z"}`)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if code := errorCode(t, recorder); code != "snapshot_exists" {
		t.Fatalf("code = %q, want snapshot_exists", code)
	}
}

func TestRegisterSnapshotValidation(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode string
	}{
		{"missing snapshot id", `{"fileCount":1,"retentionDays":1,"snapshotTime":"2026-01-01T00:00:00Z"}`, "invalid_request"},
		{"empty snapshot id", `{"snapshotId":"","fileCount":1,"retentionDays":1,"snapshotTime":"2026-01-01T00:00:00Z"}`, "invalid_request"},
		{"snapshot id wrong type", `{"snapshotId":7,"fileCount":1,"retentionDays":1,"snapshotTime":"2026-01-01T00:00:00Z"}`, "invalid_request"},
		{"missing file count", `{"snapshotId":"s-1","retentionDays":1,"snapshotTime":"2026-01-01T00:00:00Z"}`, "invalid_request"},
		{"negative file count", `{"snapshotId":"s-1","fileCount":-1,"retentionDays":1,"snapshotTime":"2026-01-01T00:00:00Z"}`, "invalid_request"},
		{"fractional file count", `{"snapshotId":"s-1","fileCount":1.5,"retentionDays":1,"snapshotTime":"2026-01-01T00:00:00Z"}`, "invalid_request"},
		{"missing retention days", `{"snapshotId":"s-1","fileCount":1,"snapshotTime":"2026-01-01T00:00:00Z"}`, "invalid_request"},
		{"zero retention days", `{"snapshotId":"s-1","fileCount":1,"retentionDays":0,"snapshotTime":"2026-01-01T00:00:00Z"}`, "invalid_retention_days"},
		{"negative retention days", `{"snapshotId":"s-1","fileCount":1,"retentionDays":-2,"snapshotTime":"2026-01-01T00:00:00Z"}`, "invalid_retention_days"},
		{"missing snapshot time", `{"snapshotId":"s-1","fileCount":1,"retentionDays":1}`, "invalid_request"},
		{"malformed snapshot time", `{"snapshotId":"s-1","fileCount":1,"retentionDays":1,"snapshotTime":"tomorrow"}`, "invalid_request"},
		{"broken json", `{"snapshotId"`, "invalid_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := newTestRouter(t)
			recorder := doRequest(t, router, http.MethodPost, "/api/v1/lake-tables/orders/snapshots", tc.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if code := errorCode(t, recorder); code != tc.wantCode {
				t.Fatalf("code = %q, want %q", code, tc.wantCode)
			}
		})
	}
}

func TestListExpiredFiltersAndSorts(t *testing.T) {
	router := newTestRouter(t)
	registerSnapshot(t, router, "orders", "s-late", "2020-01-03T00:00:00Z", 1)
	registerSnapshot(t, router, "orders", "s-early", "2020-01-01T00:00:00Z", 1)
	registerSnapshot(t, router, "orders", "s-middle", "2020-01-02T00:00:00Z", 1)
	registerSnapshot(t, router, "orders", "s-fresh", time.Now().UTC().Format(time.RFC3339), 365)
	registerSnapshot(t, router, "archive", "s-other-table", "2020-01-01T00:00:00Z", 1)

	recorder := doRequest(t, router, http.MethodGet, "/api/v1/lake-tables/orders/expired", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var body []snapshotResponse
	decodeBody(t, recorder, &body)
	got := make([]string, 0, len(body))
	for _, snap := range body {
		got = append(got, snap.SnapshotID)
	}
	want := []string{"s-early", "s-middle", "s-late"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	if body[0].SnapshotTime != "2020-01-01T00:00:00Z" || body[0].RetentionDays != 1 || body[0].FileCount != 2 {
		t.Fatalf("first entry = %+v", body[0])
	}
}

func TestListExpiredEmptyIsEmptyArray(t *testing.T) {
	router := newTestRouter(t)
	registerSnapshot(t, router, "orders", "s-fresh", time.Now().UTC().Format(time.RFC3339), 365)

	recorder := doRequest(t, router, http.MethodGet, "/api/v1/lake-tables/orders/expired", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Body.String(); got != "[]" {
		t.Fatalf("body = %q, want []", got)
	}
}

func TestListExpiredUnknownTable(t *testing.T) {
	router := newTestRouter(t)

	recorder := doRequest(t, router, http.MethodGet, "/api/v1/lake-tables/ghost/expired", nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if code := errorCode(t, recorder); code != "table_not_found" {
		t.Fatalf("code = %q, want table_not_found", code)
	}
}
