package manager

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/oleksiiipatov/agent-web-manager/internal/sbx"
)

func TestCreateProjectRoundTrip(t *testing.T) {
	stateDir := t.TempDir()
	m, err := New(sbx.New(""), stateDir)
	if err != nil {
		t.Fatal(err)
	}

	projDir := t.TempDir()
	p, err := m.CreateProject(CreateProjectRequest{Name: "  Demo  ", Path: projDir})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if p.Name != "Demo" {
		t.Errorf("name = %q, want trimmed \"Demo\"", p.Name)
	}
	if p.Path != projDir {
		t.Errorf("path = %q, want %q", p.Path, projDir)
	}

	reloaded, err := New(sbx.New(""), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reloaded.GetProject(p.ID)
	if err != nil {
		t.Fatalf("GetProject after reload: %v", err)
	}
	if got.Name != "Demo" || got.Path != projDir {
		t.Errorf("got %+v", got)
	}
}

// The UI no longer offers a name box, so a caller that leaves the name to
// CreateSandbox cannot work a collision around by supplying one of its own:
// the default has to number itself past whatever is already there. Two
// projects in folders of the same name are the ordinary way to arrive here.
func TestUniqueSandboxNameNumbersPastCollisions(t *testing.T) {
	m, err := New(sbx.New(""), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	first, err := m.uniqueSandboxName(defaultName("claude", "/w/app"))
	if err != nil {
		t.Fatal(err)
	}
	if first != "claude-app" {
		t.Fatalf("name = %q, want claude-app while nothing holds it", first)
	}

	// Registered by hand: CreateSandbox would shell out to sbx.
	for _, name := range []string{"claude-app", "claude-app-2"} {
		m.byName[name] = "id-" + name
	}

	got, err := m.uniqueSandboxName(defaultName("claude", "/other/app"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "claude-app-3" {
		t.Fatalf("name = %q, want claude-app-3", got)
	}
}

func TestCreateProjectRequiresNameAndValidPath(t *testing.T) {
	stateDir := t.TempDir()
	m, err := New(sbx.New(""), stateDir)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.CreateProject(CreateProjectRequest{Name: "  ", Path: t.TempDir()}); err == nil {
		t.Error("want an error for a blank name")
	}
	if _, err := m.CreateProject(CreateProjectRequest{Name: "demo", Path: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Error("want an error for a path that does not exist")
	}
}

// A project ordered like a sandbox: whichever had a session do something most
// recently sorts first, not whichever was created first.
func TestListProjectsOrderedByLastActivity(t *testing.T) {
	stateDir := t.TempDir()
	m, err := New(sbx.New(""), stateDir)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	old := &Project{ID: "old", Name: "old", Path: "/p1", CreatedAt: now.Add(-time.Hour), LastActivityAt: now.Add(-time.Hour)}
	recent := &Project{ID: "recent", Name: "recent", Path: "/p2", CreatedAt: now.Add(-time.Minute), LastActivityAt: now}
	m.projects[old.ID] = old
	m.projects[recent.ID] = recent

	out := m.ListProjects(context.Background())
	if len(out) != 2 || out[0].ID != "recent" || out[1].ID != "old" {
		t.Fatalf("got order %v, want [recent old]", []string{out[0].ID, out[1].ID})
	}
}

// Registering the sandbox by hand, as TestSandboxRoundTrip does: CreateSandbox
// would shell out to sbx.
func TestEnsureProjectSandboxReusesMainRegardlessOfAgent(t *testing.T) {
	stateDir := t.TempDir()
	m, err := New(sbx.New(""), stateDir)
	if err != nil {
		t.Fatal(err)
	}

	projDir := t.TempDir()
	p, err := m.CreateProject(CreateProjectRequest{Name: "demo", Path: projDir})
	if err != nil {
		t.Fatal(err)
	}

	sb := &Sandbox{ID: "main1", Name: "main-sb", Agent: "claude", ProjectID: p.ID, Workspace: projDir}
	m.sandboxes[sb.ID] = sb
	m.byName[sb.Name] = sb.ID

	got, err := m.EnsureProjectSandbox(context.Background(), p.ID, "codex")
	if err != nil {
		t.Fatalf("EnsureProjectSandbox: %v", err)
	}
	if got.ID != "main1" {
		t.Fatalf("got sandbox %q, want the existing main1 — a project's main agent is fixed by whichever session made it", got.ID)
	}
}

func TestEnsureProjectSandboxRejectsUnknownAgent(t *testing.T) {
	stateDir := t.TempDir()
	m, err := New(sbx.New(""), stateDir)
	if err != nil {
		t.Fatal(err)
	}

	projDir := t.TempDir()
	p, err := m.CreateProject(CreateProjectRequest{Name: "demo", Path: projDir})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.EnsureProjectSandbox(context.Background(), p.ID, "not-a-real-agent"); err == nil {
		t.Fatal("want an error for an unknown agent")
	}
	if sb := m.mainSandbox(p.ID); sb != nil {
		t.Fatalf("no sandbox should have been made, got %+v", sb)
	}
}

// DeleteProject removes every sandbox the project owns, and reports the
// worktree ones separately: only the caller — which talks to git — knows how
// to clean up their checkouts.
func TestDeleteProjectReportsWorktreeSandboxes(t *testing.T) {
	stateDir := t.TempDir()
	m, err := New(sbx.New(filepath.Join(t.TempDir(), "no-such-sbx")), stateDir)
	if err != nil {
		t.Fatal(err)
	}

	projDir := t.TempDir()
	p, err := m.CreateProject(CreateProjectRequest{Name: "demo", Path: projDir})
	if err != nil {
		t.Fatal(err)
	}

	main := &Sandbox{ID: "main1", Name: "main-sb", Agent: "claude", ProjectID: p.ID, Workspace: projDir}
	wt := &Sandbox{ID: "wt1", Name: "wt-sb", Agent: "claude", ProjectID: p.ID, IsWorktree: true, RepoRoot: "/repo", Workspace: "/repo-branch"}
	for _, sb := range []*Sandbox{main, wt} {
		m.sandboxes[sb.ID] = sb
		m.byName[sb.Name] = sb.ID
	}

	worktrees, err := m.DeleteProject(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if len(worktrees) != 1 || worktrees[0].ID != "wt1" {
		t.Fatalf("got worktrees %+v, want just wt1", worktrees)
	}

	if _, err := m.GetProject(p.ID); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("project should be gone, got %v", err)
	}
	if _, err := m.GetSandbox("main1"); !errors.Is(err, ErrSandboxNotFound) {
		t.Error("main sandbox should be gone")
	}
	if _, err := m.GetSandbox("wt1"); !errors.Is(err, ErrSandboxNotFound) {
		t.Error("worktree sandbox should be gone")
	}
}

// A session's activity is what keeps a sandbox's place in the list current
// across a restart; a project sits above one and has to move the same way.
func TestTouchSandboxActivityBumpsOwningProject(t *testing.T) {
	stateDir := t.TempDir()
	m, err := New(sbx.New(""), stateDir)
	if err != nil {
		t.Fatal(err)
	}

	projDir := t.TempDir()
	p, err := m.CreateProject(CreateProjectRequest{Name: "demo", Path: projDir})
	if err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-time.Hour)
	p.LastActivityAt = stale

	sb := &Sandbox{ID: "sb1", Name: "sb", Agent: "claude", ProjectID: p.ID, LastActivityAt: stale}
	m.sandboxes[sb.ID] = sb
	m.byName[sb.Name] = sb.ID

	m.touchSandboxActivity(sb.ID)

	if !m.projects[p.ID].LastActivityAt.After(stale) {
		t.Fatal("project's LastActivityAt should have moved forward with its sandbox's")
	}
}
