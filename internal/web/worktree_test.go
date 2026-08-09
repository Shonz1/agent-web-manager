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

// worktreeServer returns a server holding one sandbox on the given workspace,
// and that sandbox's id.
//
// Its sbx binary deliberately does not exist: every one of these tests is about
// what happens on this side of "sbx create", and a machine that has sbx
// installed must not have a sandbox made on it by a test run.
func worktreeServer(t *testing.T, workspace string) (*Server, string) {
	t.Helper()
	stateDir := t.TempDir()
	const id = "wt000001"
	state := []manager.Sandbox{{ID: id, Name: "box", Agent: "claude", Workspace: workspace}}
	data, err := json.Marshal(state)
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
	return &Server{mgr: mgr, git: git.New("")}, id
}

func worktreeRequest(t *testing.T, srv *Server, id string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sandboxes/"+id+"/worktree", strings.NewReader(string(raw)))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	srv.handleStartWorktreeSession(rec, req)
	return rec
}

// committedRepo is a checkout with something in it: a worktree is a checkout of
// a commit, and a repository with none has nothing to branch from.
func committedRepo(t *testing.T) string {
	t.Helper()
	dir := gitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-m", "initial"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return dir
}

func worktreeList(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "worktree", "list")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git worktree list: %v\n%s", err, out)
	}
	return string(out)
}

func TestWorktreeSessionNeedsAWorkspace(t *testing.T) {
	srv, id := worktreeServer(t, "")
	rec := worktreeRequest(t, srv, id, map[string]any{"branch": "feature-x"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "no workspace") {
		t.Fatalf("error does not say the sandbox has no workspace: %s", rec.Body)
	}
}

func TestWorktreeSessionNeedsARepo(t *testing.T) {
	srv, id := worktreeServer(t, t.TempDir())
	rec := worktreeRequest(t, srv, id, map[string]any{"branch": "feature-x"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "not a git checkout") {
		t.Fatalf("error does not say the workspace is not a checkout: %s", rec.Body)
	}
}

func TestWorktreeSessionRefusesABranchGitWould(t *testing.T) {
	dir := committedRepo(t)
	srv, id := worktreeServer(t, dir)

	rec := worktreeRequest(t, srv, id, map[string]any{"branch": "-x"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body)
	}
	if strings.Contains(worktreeList(t, dir), "-x") {
		t.Fatalf("a worktree was made anyway:\n%s", worktreeList(t, dir))
	}
}

// A worktree made for a sandbox that never came up is a checkout nobody asked
// for, and the next attempt at the same branch would fail on the directory it
// left behind.
func TestWorktreeSessionRollsBackWhenTheSandboxFails(t *testing.T) {
	dir := committedRepo(t)
	srv, id := worktreeServer(t, dir)

	rec := worktreeRequest(t, srv, id, map[string]any{"branch": "feature-x"})
	if rec.Code == http.StatusCreated {
		t.Fatalf("the session was started without an sbx to start it in: %s", rec.Body)
	}
	// It has to have got as far as the sandbox for this to be the rollback
	// rather than a worktree that was never made in the first place.
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

func TestWithRepoMount(t *testing.T) {
	for _, tc := range []struct {
		name   string
		extras []string
		repo   string
		want   []string
	}{
		{"added last", []string{"/docs"}, "/src/app", []string{"/docs", "/src/app"}},
		{"no extras", nil, "/src/app", []string{"/src/app"}},
		// A repository already mounted read-only is replaced rather than kept:
		// the worktree's administrative files live in there, and an agent that
		// cannot write to them cannot commit.
		{"read-only replaced", []string{"/src/app:ro"}, "/src/app", []string{"/src/app"}},
		{"not doubled up", []string{"/src/app", "/docs"}, "/src/app", []string{"/docs", "/src/app"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := withRepoMount(tc.extras, tc.repo)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("withRepoMount(%q, %q) = %q, want %q", tc.extras, tc.repo, got, tc.want)
			}
		})
	}
}
