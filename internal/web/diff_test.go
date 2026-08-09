package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oleksiiipatov/agent-web-manager/internal/git"
	"github.com/oleksiiipatov/agent-web-manager/internal/manager"
	"github.com/oleksiiipatov/agent-web-manager/internal/sbx"
)

// diffServer returns a server holding one sandbox whose workspace is the given
// directory, and that sandbox's id.
//
// The sandbox is written straight into the state file and loaded back, which
// is the only way to register one without an sbx to create it — and creating
// one is not what these tests are about.
func diffServer(t *testing.T, workspace string) (*Server, string) {
	t.Helper()
	stateDir := t.TempDir()
	const id = "diff0001"
	state := []manager.Sandbox{{ID: id, Name: "box", Agent: "claude", Workspace: workspace}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "sandboxes.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := manager.New(sbx.New("sbx"), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{mgr: mgr, git: git.New("")}, id
}

func diffRequest(t *testing.T, srv *Server, id, path, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/sandboxes/"+id+path+"?"+query, nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	if strings.HasSuffix(path, "/file") {
		srv.handleDiffFile(rec, req)
	} else {
		srv.handleDiff(rec, req)
	}
	return rec
}

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--initial-branch=main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return dir
}

func TestDiffListsChanges(t *testing.T) {
	dir := gitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, id := diffServer(t, dir)

	rec := diffRequest(t, srv, id, "/diff", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var body changesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Repo || body.Changes == nil {
		t.Fatalf("body = %+v, want a repository with changes", body)
	}
	if len(body.Changes.Files) != 1 || body.Changes.Files[0].Path != "new.txt" {
		t.Fatalf("files = %+v, want new.txt", body.Changes.Files)
	}
}

func TestDiffFileReturnsHunks(t *testing.T) {
	dir := gitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, id := diffServer(t, dir)

	rec := diffRequest(t, srv, id, "/diff/file", "path=new.txt")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var body git.FileDiff
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Hunks) != 1 || body.Hunks[0].Lines[0].Text != "hello" {
		t.Fatalf("hunks = %+v, want the added line", body.Hunks)
	}
}

// A workspace that is not a checkout is an ordinary thing for one to be, and
// the page has something to say about it — it is not a failed request.
func TestDiffOnPlainDirectoryIsNotAnError(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	srv, id := diffServer(t, t.TempDir())

	rec := diffRequest(t, srv, id, "/diff", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var body changesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Repo || body.Message == "" {
		t.Fatalf("body = %+v, want repo false with a reason", body)
	}
}

func TestDiffWithoutAWorkspace(t *testing.T) {
	srv, id := diffServer(t, "")

	rec := diffRequest(t, srv, id, "/diff", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var body changesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Repo || body.Message == "" {
		t.Fatalf("body = %+v, want repo false with a reason", body)
	}

	if rec := diffRequest(t, srv, id, "/diff/file", "path=x"); rec.Code != http.StatusBadRequest {
		t.Fatalf("file status = %d, want 400", rec.Code)
	}
}

func TestDiffFileRejectsEscapingPaths(t *testing.T) {
	srv, id := diffServer(t, gitRepo(t))

	for _, bad := range []string{"", "..%2F..%2Fetc%2Fpasswd", "%2Fetc%2Fpasswd"} {
		rec := diffRequest(t, srv, id, "/diff/file", "path="+bad)
		if rec.Code == http.StatusOK {
			t.Errorf("path=%q was allowed", bad)
		}
	}
}

func TestDiffUnknownSandbox(t *testing.T) {
	srv, _ := diffServer(t, "")
	rec := diffRequest(t, srv, "nope", "/diff", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// The diff is the contents of the user's source, so it is behind the same
// origin check as the folder picker rather than open to any page.
func TestDiffRejectsCrossOrigin(t *testing.T) {
	srv, id := diffServer(t, "")
	for _, path := range []string{"/diff", "/diff/file"} {
		req := httptest.NewRequest(http.MethodGet, "/api/sandboxes/"+id+path, nil)
		req.SetPathValue("id", id)
		req.Header.Set("Origin", "https://evil.example")
		rec := httptest.NewRecorder()
		if path == "/diff" {
			srv.handleDiff(rec, req)
		} else {
			srv.handleDiffFile(rec, req)
		}
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s status = %d, want 403", path, rec.Code)
		}
	}
}
