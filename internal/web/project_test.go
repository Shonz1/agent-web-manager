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

	raw, _ := json.Marshal(map[string]any{"name": "Demo", "path": dir, "agent": "claude"})
	req := httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(string(raw)))
	rec := httptest.NewRecorder()
	srv.handleCreateProject(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	created := decodeJSON[projectView](t, rec)
	if created.Name != "Demo" || created.Path != dir || created.Agent != "claude" {
		t.Fatalf("got %+v", created)
	}
	if created.Sessions == nil {
		t.Error("sessions should be an empty slice, not null — the UI iterates it without a guard")
	}
	// The base sandbox is built in the background, and there is no sbx here
	// to build it with: what matters is that the create answered without
	// waiting for one.
	if created.BaseSandbox != nil {
		t.Errorf("base sandbox = %+v, want none reported yet", created.BaseSandbox)
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

	raw, _ := json.Marshal(map[string]any{
		"name": "Demo", "path": filepath.Join(t.TempDir(), "does-not-exist"), "agent": "claude"})
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

// A session cannot start until the project has the base sandbox its own
// sandbox is cloned from, so a project whose base sandbox cannot be built
// fails here rather than starting something that inherited nothing.
func TestStartProjectSessionNeedsTheBaseSandbox(t *testing.T) {
	srv := projectServer(t)
	created, err := srv.mgr.CreateProject(manager.CreateProjectRequest{Name: "demo", Path: t.TempDir(), Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}

	rec := postProjectSession(t, srv, created.ID, map[string]any{"kind": "shell"})
	if rec.Code == http.StatusCreated {
		t.Fatalf("the session was started without an sbx to start it in: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "sbx") {
		t.Fatalf("the request failed before the sandbox was attempted: %s", rec.Body)
	}
	if sb := srv.mgr.BaseSandbox(created.ID); sb != nil {
		t.Fatalf("nothing should have been registered, got %+v", sb)
	}
}

// A plain session gets a clone sandbox of its own rather than sharing the
// project's, so the base sandbox is left with nothing running in it and the
// second session does not have to wait for the first.
func TestStartProjectSessionClonesRatherThanReusing(t *testing.T) {
	stateDir := t.TempDir()
	projDir := t.TempDir()

	bootstrap, err := manager.New(sbx.New(""), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := bootstrap.CreateProject(manager.CreateProjectRequest{Name: "demo", Path: projDir, Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}

	// The only way to register a sandbox without an sbx to create it: written
	// straight into the state file the next manager loads.
	sandboxes := []manager.Sandbox{
		{ID: "base1", Name: "base-sb", Agent: "claude", Workspace: projDir, ProjectID: proj.ID, IsBase: true},
	}
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

	rec := postProjectSession(t, srv, proj.ID, map[string]any{"kind": "shell"})
	// There is no sbx to make the clone with, so the request fails — what
	// matters is that it never tried to run the session in base1.
	if rec.Code == http.StatusCreated {
		t.Fatalf("the session was started without an sbx to start it in: %s", rec.Body)
	}

	view, err := mgr.GetProjectView(t.Context(), proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Sandboxes) != 1 || view.Sandboxes[0].ID != "base1" {
		t.Fatalf("got sandboxes %+v, want just the base one", view.Sandboxes)
	}
	if len(view.Sessions) != 0 {
		t.Fatalf("got sessions %+v in the base sandbox, want none ever", view.Sessions)
	}
}

// Working in the base sandbox or destroying it is refused wherever the ask
// comes from. Stopping it is not: it holds no work, and it is the only way
// anyone has of putting the container down.
func TestBaseSandboxActionsAreRefused(t *testing.T) {
	stateDir := t.TempDir()
	bootstrap, err := manager.New(sbx.New(""), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	projDir := t.TempDir()
	proj, err := bootstrap.CreateProject(manager.CreateProjectRequest{Name: "demo", Path: projDir, Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal([]manager.Sandbox{
		{ID: "base1", Name: "base-sb", Agent: "claude", Workspace: projDir, ProjectID: proj.ID, IsBase: true},
	})
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

	for _, tc := range []struct {
		name   string
		method string
		path   string
		call   func(http.ResponseWriter, *http.Request)
	}{
		{"start a session", http.MethodPost, "/sessions", srv.handleStartSession},
		{"delete", http.MethodDelete, "", srv.handleDeleteSandbox},
		{"branch a worktree off", http.MethodPost, "/worktree", srv.handleStartWorktreeSession},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/sandboxes/base1"+tc.path, strings.NewReader("{}"))
			req.SetPathValue("id", "base1")
			rec := httptest.NewRecorder()
			tc.call(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body)
			}
		})
	}

	// The sbx binary is not there in this test, so the stop fails — but on the
	// way to sbx rather than at the door.
	req := httptest.NewRequest(http.MethodPost, "/api/sandboxes/base1/stop", strings.NewReader("{}"))
	req.SetPathValue("id", "base1")
	rec := httptest.NewRecorder()
	srv.handleStopSandbox(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Errorf("stopping the base sandbox was refused: %s", rec.Body)
	}

	if _, err := mgr.GetSandbox("base1"); err != nil {
		t.Fatalf("the base sandbox should still be here: %v", err)
	}
}

func TestStartProjectSessionWorktreeNeedsARepo(t *testing.T) {
	srv := projectServer(t)
	proj, err := srv.mgr.CreateProject(manager.CreateProjectRequest{Name: "demo", Path: t.TempDir(), Agent: "claude"})
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
	proj, err := srv.mgr.CreateProject(manager.CreateProjectRequest{Name: "demo", Path: dir, Agent: "claude"})
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

// A sandbox with nothing running in it is exactly what a restart leaves
// behind — every session ends, every sandbox stays — so the project view has
// to carry its sandboxes whether or not any session is attached to them.
// Without this the worktree checkout below could not be found, let alone
// removed, short of deleting the whole project.
func TestProjectViewListsSandboxesWithNoSessions(t *testing.T) {
	projDir := committedRepo(t)
	stateDir := t.TempDir()

	bootstrap, err := manager.New(sbx.New(""), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := bootstrap.CreateProject(manager.CreateProjectRequest{Name: "demo", Path: projDir, Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}

	tree, err := git.New("").AddWorktree(t.Context(), projDir, "", "feature-x")
	if err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}

	// Registered through the state file, as the other sandbox tests here do:
	// there is no sbx to create one with.
	sandboxes := []manager.Sandbox{
		{ID: "wt1", Name: "demo-9f2c", Agent: "claude", Workspace: tree.Path,
			ProjectID: proj.ID, IsWorktree: true, RepoRoot: tree.RepoRoot},
		{ID: "base1", Name: "claude-demo", Agent: "claude", Workspace: projDir, ProjectID: proj.ID, IsBase: true},
	}
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

	req := httptest.NewRequest(http.MethodGet, "/api/projects/"+proj.ID, nil)
	req.SetPathValue("id", proj.ID)
	rec := httptest.NewRecorder()
	srv.handleGetProject(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}

	got := decodeJSON[projectView](t, rec)
	if len(got.Sessions) != 0 {
		t.Fatalf("got sessions %+v, want none", got.Sessions)
	}
	if len(got.Sandboxes) != 2 {
		t.Fatalf("got sandboxes %+v, want both of them", got.Sandboxes)
	}
	// The base sandbox leads, whatever order the manager's map happened to
	// yield them in.
	if got.Sandboxes[0].ID != "base1" || got.Sandboxes[1].ID != "wt1" {
		t.Fatalf("got order %s, %s; want base1 first", got.Sandboxes[0].ID, got.Sandboxes[1].ID)
	}
	if got.BaseSandbox == nil || got.BaseSandbox.ID != "base1" {
		t.Fatalf("base sandbox = %+v, want base1", got.BaseSandbox)
	}
	if !got.Sandboxes[0].IsBase {
		t.Error("the base sandbox is not marked as one, so the UI would offer actions the server refuses")
	}

	wt := got.Sandboxes[1]
	if !wt.IsWorktree {
		t.Error("the worktree sandbox is not marked as one, so the UI cannot warn what deleting it removes")
	}
	if wt.Workspace != tree.Path {
		t.Errorf("workspace = %q, want %q", wt.Workspace, tree.Path)
	}
	if wt.Branch != "feature-x" {
		t.Errorf("branch = %q, want feature-x", wt.Branch)
	}
	if wt.Sessions != 0 {
		t.Errorf("sessions = %d, want 0", wt.Sessions)
	}
}

// Deleting a project removes every sandbox it owns, including cleaning up
// the checkout behind a worktree one — the cleanup the manager's own
// DeleteSandbox does not do, since only the callers here talk to git.
func TestDeleteProjectCleansUpWorktrees(t *testing.T) {
	dir := committedRepo(t)
	stateDir := t.TempDir()

	bootstrap, err := manager.New(sbx.New(""), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := bootstrap.CreateProject(manager.CreateProjectRequest{Name: "demo", Path: dir, Agent: "claude"})
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
