package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oleksiiipatov/agent-web-manager/internal/git"
	"github.com/oleksiiipatov/agent-web-manager/internal/manager"
	"github.com/oleksiiipatov/agent-web-manager/internal/sbx"
)

// projectServer returns a server with no projects yet, backed by an sbx
// binary that deliberately does not exist — these tests are about what
// happens on this side of "sbx create", the same convention worktreeServer
// and diffServer follow.
func projectServer(t *testing.T) *Server {
	t.Helper()
	stateDir := t.TempDir()
	mgr, err := manager.New(sbx.New(filepath.Join(t.TempDir(), "no-such-sbx")), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{mgr: mgr, git: git.New("")}
}

func decodeJSON[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body.String())
	}
	return v
}

func TestProjectCRUDRoundTrip(t *testing.T) {
	srv := projectServer(t)
	dir := t.TempDir()

	raw, _ := json.Marshal(map[string]any{"name": "Demo", "path": dir})
	req := httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(string(raw)))
	rec := httptest.NewRecorder()
	srv.handleCreateProject(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	created := decodeJSON[projectView](t, rec)
	if created.Name != "Demo" || created.Path != dir {
		t.Fatalf("got %+v", created)
	}
	if created.Sessions == nil {
		t.Error("sessions should be an empty slice, not null — the UI iterates it without a guard")
	}
	if created.MainSandbox != nil {
		t.Error("a freshly created project should have no main sandbox yet")
	}

	// GET by id
	getReq := httptest.NewRequest(http.MethodGet, "/api/projects/"+created.ID, nil)
	getReq.SetPathValue("id", created.ID)
	getRec := httptest.NewRecorder()
	srv.handleGetProject(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200 (%s)", getRec.Code, getRec.Body)
	}
	got := decodeJSON[projectView](t, getRec)
	if got.ID != created.ID {
		t.Fatalf("got id %q, want %q", got.ID, created.ID)
	}

	// LIST
	listReq := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	listRec := httptest.NewRecorder()
	srv.handleListProjects(listRec, listReq)
	var list struct {
		Projects []projectView `json:"projects"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Projects) != 1 || list.Projects[0].ID != created.ID {
		t.Fatalf("got %+v, want one project %q", list.Projects, created.ID)
	}
}

func TestCreateProjectRejectsMissingPath(t *testing.T) {
	srv := projectServer(t)

	raw, _ := json.Marshal(map[string]any{"name": "Demo", "path": filepath.Join(t.TempDir(), "does-not-exist")})
	req := httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(string(raw)))
	rec := httptest.NewRecorder()
	srv.handleCreateProject(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body)
	}
}

func TestGetProjectNotFound(t *testing.T) {
	srv := projectServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/projects/nope", nil)
	req.SetPathValue("id", "nope")
	rec := httptest.NewRecorder()
	srv.handleGetProject(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rec.Code, rec.Body)
	}
}

func postProjectSession(t *testing.T, srv *Server, id string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/projects/"+id+"/sessions", strings.NewReader(string(raw)))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	srv.handleStartProjectSession(rec, req)
	return rec
}

func TestStartProjectSessionNotFound(t *testing.T) {
	srv := projectServer(t)
	rec := postProjectSession(t, srv, "nope", map[string]any{"kind": "shell"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rec.Code, rec.Body)
	}
}

// The first session started in a project without a worktree is what makes
// its main sandbox, so it has to say what agent that sandbox is for.
func TestStartProjectSessionFirstOneNeedsAKnownAgent(t *testing.T) {
	srv := projectServer(t)
	created, err := srv.mgr.CreateProject(manager.CreateProjectRequest{Name: "demo", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	rec := postProjectSession(t, srv, created.ID, map[string]any{"kind": "shell"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "agent") {
		t.Fatalf("error does not mention the agent: %s", rec.Body)
	}
}

// A non-worktree session reuses the project's existing main sandbox
// regardless of what agent the request names — and never makes a second one,
// even though starting the session itself cannot succeed without a real sbx.
func TestStartProjectSessionReusesExistingMainSandbox(t *testing.T) {
	stateDir := t.TempDir()
	bootstrap, err := manager.New(sbx.New(""), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	projDir := t.TempDir()
	proj, err := bootstrap.CreateProject(manager.CreateProjectRequest{Name: "demo", Path: projDir})
	if err != nil {
		t.Fatal(err)
	}

	// The only way to register a sandbox without an sbx to create it: written
	// straight into the state file the next manager loads.
	sandboxes := []manager.Sandbox{{ID: "main1", Name: "main-sb", Agent: "claude", Workspace: projDir, ProjectID: proj.ID}}
	data, err := json.Marshal(sandboxes)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "sandboxes.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, err := manager.New(sbx.New(filepath.Join(t.TempDir(), "no-such-sbx")), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{mgr: mgr, git: git.New("")}

	rec := postProjectSession(t, srv, proj.ID, map[string]any{"kind": "shell", "agent": "codex"})
	// There is no sbx to actually start the session in, so the request itself
	// fails — what matters is that it failed trying to use main1 rather than
	// making a second sandbox for "codex".
	if rec.Code == http.StatusCreated {
		t.Fatalf("the session was started without an sbx to start it in: %s", rec.Body)
	}

	view, err := mgr.GetProjectView(t.Context(), proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Sandboxes) != 1 || view.Sandboxes[0].ID != "main1" {
		t.Fatalf("got sandboxes %+v, want just the original main1", view.Sandboxes)
	}
}

func TestStartProjectSessionWorktreeNeedsARepo(t *testing.T) {
	srv := projectServer(t)
	proj, err := srv.mgr.CreateProject(manager.CreateProjectRequest{Name: "demo", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	rec := postProjectSession(t, srv, proj.ID, map[string]any{
		"kind": "shell", "agent": "claude", "worktree": true, "branch": "feature-x",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "not a git checkout") {
		t.Fatalf("error does not say the project is not a checkout: %s", rec.Body)
	}
}

// A worktree made for a sandbox that never came up must not survive the
// request: the next attempt at the same branch would otherwise fail on the
// directory it left behind.
func TestStartProjectSessionWorktreeRollsBackWhenTheSandboxFails(t *testing.T) {
	dir := committedRepo(t)
	srv := projectServer(t)
	proj, err := srv.mgr.CreateProject(manager.CreateProjectRequest{Name: "demo", Path: dir})
	if err != nil {
		t.Fatal(err)
	}

	rec := postProjectSession(t, srv, proj.ID, map[string]any{
		"kind": "shell", "agent": "claude", "worktree": true, "branch": "feature-x",
	})
	if rec.Code == http.StatusCreated {
		t.Fatalf("the session was started without an sbx to start it in: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "sbx") {
		t.Fatalf("the request failed before the sandbox was attempted: %s", rec.Body)
	}

	path := git.DefaultWorktreePath(dir, "feature-x")
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s was left behind: %v", path, err)
	}
	if list := worktreeList(t, dir); strings.Contains(list, path) {
		t.Fatalf("the repository still lists the worktree:\n%s", list)
	}
}

// Deleting a project removes every sandbox it owns, including cleaning up
// the checkout behind a worktree one — the one cleanup DeleteSandbox alone
// does not do, since only the caller here talks to git.
func TestDeleteProjectCleansUpWorktrees(t *testing.T) {
	dir := committedRepo(t)
	stateDir := t.TempDir()

	bootstrap, err := manager.New(sbx.New(""), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := bootstrap.CreateProject(manager.CreateProjectRequest{Name: "demo", Path: dir})
	if err != nil {
		t.Fatal(err)
	}

	tree, err := git.New("").AddWorktree(t.Context(), dir, "", "feature-x")
	if err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}

	// The only way to register a worktree sandbox without an sbx to create
	// it: written straight into the state file the next manager loads.
	sandboxes := []manager.Sandbox{{
		ID: "wt1", Name: "wt-sb", Agent: "shell", Workspace: tree.Path,
		ProjectID: proj.ID, IsWorktree: true, RepoRoot: tree.RepoRoot,
	}}
	data, err := json.Marshal(sandboxes)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "sandboxes.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, err := manager.New(sbx.New(filepath.Join(t.TempDir(), "no-such-sbx")), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{mgr: mgr, git: git.New("")}

	req := httptest.NewRequest(http.MethodDelete, "/api/projects/"+proj.ID, nil)
	req.SetPathValue("id", proj.ID)
	rec := httptest.NewRecorder()
	srv.handleDeleteProject(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (%s)", rec.Code, rec.Body)
	}

	if _, err := os.Lstat(tree.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree %s was left behind: %v", tree.Path, err)
	}
	if list := worktreeList(t, dir); strings.Contains(list, tree.Path) {
		t.Fatalf("the repository still lists the worktree:\n%s", list)
	}
}
