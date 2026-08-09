package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func uiRequest(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	srv := &Server{static: StaticFS()}
	rec := httptest.NewRecorder()
	srv.handleUI().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// The client keeps its selection in the path, so those paths have to come back
// with the index page rather than a 404 — that is what makes a reload keep the
// open sandbox or session.
func TestHandleUIServesClientRoutes(t *testing.T) {
	for _, path := range []string{"/", "/sandboxes/a1b2c3", "/sessions/d4e5f6"} {
		rec := uiRequest(t, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d, want 200", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `id="sandbox-list"`) {
			t.Fatalf("GET %s: body is not the index page", path)
		}
	}
}

func TestHandleUIServesAssets(t *testing.T) {
	rec := uiRequest(t, "/app.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /app.js: status %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<!DOCTYPE html>") {
		t.Fatal("GET /app.js: fell back to the index page")
	}
}
