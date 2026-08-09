package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func decodeBrowse(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return body
}

func entryNames(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, ok := body["entries"].([]any)
	if !ok {
		t.Fatalf("entries missing from %v", body)
	}
	names := make([]string, 0, len(raw))
	for _, e := range raw {
		names = append(names, e.(map[string]any)["name"].(string))
	}
	return names
}

func browseRequest(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	srv := &Server{}
	rec := httptest.NewRecorder()
	srv.handleBrowse(rec, httptest.NewRequest(http.MethodGet, "/api/fs/dirs?"+query, nil))
	return rec
}

func TestBrowseListsDirectoriesOnly(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"beta", "Alpha", ".hidden"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "Alpha", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	rec := browseRequest(t, "path="+root)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	body := decodeBrowse(t, rec)

	// Sorted case-insensitively; files and dot-directories are left out.
	got := entryNames(t, body)
	want := []string{"Alpha", "beta"}
	if len(got) != len(want) {
		t.Fatalf("entries = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entries = %v, want %v", got, want)
		}
	}

	if body["parent"] != filepath.Dir(root) {
		t.Errorf("parent = %v, want %s", body["parent"], filepath.Dir(root))
	}
	if repo := body["entries"].([]any)[0].(map[string]any)["repo"]; repo != true {
		t.Errorf("Alpha repo = %v, want true", repo)
	}
}

func TestBrowseHiddenOptIn(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".config"), 0o755); err != nil {
		t.Fatal(err)
	}

	rec := browseRequest(t, "hidden=1&path="+root)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if got := entryNames(t, decodeBrowse(t, rec)); len(got) != 1 || got[0] != ".config" {
		t.Fatalf("entries = %v, want [.config]", got)
	}
}

func TestBrowseFilePathFallsBackToItsDirectory(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := browseRequest(t, "path="+file)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if got := decodeBrowse(t, rec)["path"]; got != root {
		t.Errorf("path = %v, want %s", got, root)
	}
}

func TestBrowseMissingPath(t *testing.T) {
	rec := browseRequest(t, "path="+filepath.Join(t.TempDir(), "nope"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestBrowseDefaultsToHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory in this environment")
	}
	rec := browseRequest(t, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if got := decodeBrowse(t, rec)["path"]; got != home {
		t.Errorf("path = %v, want %s", got, home)
	}
}

func TestBrowseRejectsCrossOrigin(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/fs/dirs", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	srv.handleBrowse(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestResolveBrowsePathExpandsTilde(t *testing.T) {
	got, err := resolveBrowsePath("~/projects", "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/home/u", "projects"); got != want {
		t.Errorf("resolveBrowsePath = %s, want %s", got, want)
	}
}
