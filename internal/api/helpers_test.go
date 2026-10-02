package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kc1wfotfm4/qry-lakehouse-maintainer/internal/store"
)

func newTestRouter(t *testing.T) http.Handler {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewRouter(st)
}

// doRequest issues one HTTP call; a string body is sent verbatim, anything
// else is JSON-marshalled.
func doRequest(t *testing.T, router http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	switch v := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(v)
	default:
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(data)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), target); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
}

// errorCode asserts the published error shape and returns the code.
func errorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decodeBody(t, recorder, &body)
	if body.Error.Code == "" || body.Error.Message == "" {
		t.Fatalf("error body missing code/message: %s", recorder.Body.String())
	}
	return body.Error.Code
}

func registerSnapshot(t *testing.T, router http.Handler, tableID, snapshotID, snapshotTime string, retentionDays int64) {
	t.Helper()
	body := fmt.Sprintf(`{"snapshotId":%q,"fileCount":2,"retentionDays":%d,"snapshotTime":%q}`,
		snapshotID, retentionDays, snapshotTime)
	recorder := doRequest(t, router, http.MethodPost, "/api/v1/lake-tables/"+tableID+"/snapshots", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("register %s: status = %d, body = %s", snapshotID, recorder.Code, recorder.Body.String())
	}
}
