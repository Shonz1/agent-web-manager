package manager

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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
	p, err := m.CreateProject(CreateProjectRequest{Name: "  Demo  ", Path: projDir, Agent: "claude"})
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
	if got.Agent != "claude" {
		t.Errorf("agent = %q, want claude — every sandbox in the project is built for it", got.Agent)
	}
}

// A project written before the agent moved from each sandbox onto the project
// takes the agent its sandboxes are already built for, so the base sandbox
// made for it on the next start is the same kind of thing they are.
func TestLoadBackfillsProjectAgentFromItsSandboxes(t *testing.T) {
	stateDir := t.TempDir()

	projects := []Project{{ID: "p1", Name: "demo", Path: "/p", CreatedAt: time.Now()}}
	writeJSONFile(t, filepath.Join(stateDir, "projects.json"), projects)
	writeJSONFile(t, filepath.Join(stateDir, "sandboxes.json"), []Sandbox{
		{ID: "wt1", Name: "wt", Agent: "gemini", ProjectID: "p1", IsWorktree: true, CreatedAt: time.Now().Add(-time.Hour)},
		{ID: "main1", Name: "main", Agent: "codex", ProjectID: "p1", CreatedAt: time.Now()},
	})

	m, err := New(sbx.New(""), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := m.GetProject("p1")
	if err != nil {
		t.Fatal(err)
	}
	if p.Agent != "codex" {
		t.Fatalf("agent = %q, want codex — the sandbox on the project's own folder, not the worktree one", p.Agent)
	}
}

// A project with no sandbox to learn from still has to end up with an agent:
// without one it could never make a base sandbox at all.
func TestLoadBackfillsProjectAgentWithoutSandboxes(t *testing.T) {
	stateDir := t.TempDir()
	writeJSONFile(t, filepath.Join(stateDir, "projects.json"),
		[]Project{{ID: "p1", Name: "demo", Path: "/p", CreatedAt: time.Now()}})

	m, err := New(sbx.New(""), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := m.GetProject("p1")
	if err != nil {
		t.Fatal(err)
	}
	if !sbx.ValidAgent(p.Agent) {
		t.Fatalf("agent = %q, which sbx would refuse", p.Agent)
	}
}

// fakeSbx writes a stand-in for the sbx binary that reports the named
// sandboxes as running and accepts everything else without doing anything. It
// is enough for the calls that only ask sbx what is there.
func fakeSbx(t *testing.T, running ...string) string {
	t.Helper()
	boxes := make([]sbx.Sandbox, 0, len(running))
	for _, name := range running {
		boxes = append(boxes, sbx.Sandbox{Name: name, ID: name, Agent: "claude", Status: "running"})
	}
	listing, err := json.Marshal(map[string]any{"sandboxes": boxes})
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "sbx")
	script := "#!/bin/sh\nif [ \"$1\" = \"ls\" ]; then printf '%s' '" + string(listing) + "'; fi\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// slowSbx writes a stand-in for the sbx binary whose "create" waits: it
// touches started and then blocks until release appears, so a test can do
// something else while a create is in flight. Everything else succeeds
// silently, and "ls" reports nothing at all.
func slowSbx(t *testing.T, started, release string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sbx")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"create\" ]; then\n" +
		"  : > " + started + "\n" +
		"  while [ ! -f " + release + " ]; do sleep 0.01; done\n" +
		"fi\n" +
		"if [ \"$1\" = \"ls\" ]; then printf '%s' '{\"sandboxes\":[]}'; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// waitForFile blocks until the path exists, or fails the test.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

// A create is an image pull's worth of minutes and DeleteProject does not wait
// for one, so a project can go while its base sandbox is being built. The
// sandbox registered afterwards would be one nothing ever removes: it belongs
// to no project, and DeleteSandbox refuses a base sandbox.
func TestEnsureBaseSandboxOutlivedByItsProject(t *testing.T) {
	dir := t.TempDir()
	started, release := filepath.Join(dir, "creating"), filepath.Join(dir, "release")

	m, err := New(sbx.New(slowSbx(t, started, release)), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := m.CreateProject(CreateProjectRequest{Name: "demo", Path: t.TempDir(), Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := m.EnsureBaseSandbox(context.Background(), p.ID)
		done <- err
	}()

	waitForFile(t, started)
	if _, err := m.DeleteProject(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := <-done; !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("EnsureBaseSandbox = %v, want ErrProjectNotFound", err)
	}
	if left := m.projectSandboxes(p.ID); len(left) != 0 {
		t.Errorf("left behind %+v, which nothing can delete now the project has gone", left)
	}
}

// The only way to register state without an sbx to create it: written
// straight into the file the next manager loads.
func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
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

func TestCreateProjectRequiresNamePathAndAgent(t *testing.T) {
	stateDir := t.TempDir()
	m, err := New(sbx.New(""), stateDir)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.CreateProject(CreateProjectRequest{Name: "  ", Path: t.TempDir(), Agent: "claude"}); err == nil {
		t.Error("want an error for a blank name")
	}
	if _, err := m.CreateProject(CreateProjectRequest{
		Name: "demo", Path: filepath.Join(t.TempDir(), "missing"), Agent: "claude"}); err == nil {
		t.Error("want an error for a path that does not exist")
	}
	// The agent is fixed for the life of the project, so a bad one has to be
	// caught here rather than at the first session.
	if _, err := m.CreateProject(CreateProjectRequest{Name: "demo", Path: t.TempDir()}); err == nil {
		t.Error("want an error for a missing agent")
	}
	if _, err := m.CreateProject(CreateProjectRequest{Name: "demo", Path: t.TempDir(), Agent: "not-an-agent"}); err == nil {
		t.Error("want an error for an unknown agent")
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
// would shell out to sbx. The point is that a project with a base sandbox
// already gets that one back rather than a second — the create races the
// first session, and both must land on the same sandbox.
func TestEnsureBaseSandboxReusesTheOneAlreadyThere(t *testing.T) {
	m, err := New(sbx.New(fakeSbx(t, "base-sb")), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	projDir := t.TempDir()
	p, err := m.CreateProject(CreateProjectRequest{Name: "demo", Path: projDir, Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}

	sb := &Sandbox{ID: "base1", Name: "base-sb", Agent: "claude", ProjectID: p.ID, Workspace: projDir, IsBase: true}
	m.sandboxes[sb.ID] = sb
	m.byName[sb.Name] = sb.ID

	got, err := m.EnsureBaseSandbox(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("EnsureBaseSandbox: %v", err)
	}
	if got.ID != "base1" {
		t.Fatalf("got sandbox %q, want the existing base1", got.ID)
	}
	if n := len(m.projectSandboxes(p.ID)); n != 1 {
		t.Fatalf("the project has %d sandboxes, want just the one it started with", n)
	}
}

// "Missing" means the container as well as the record: a base sandbox sbx no
// longer has is rebuilt in place rather than left for the session to fail on,
// and no second record is made for it.
func TestEnsureBaseSandboxRebuildsAContainerSbxHasLost(t *testing.T) {
	m, err := New(sbx.New(fakeSbx(t)), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	projDir := t.TempDir()
	p, err := m.CreateProject(CreateProjectRequest{Name: "demo", Path: projDir, Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	sb := &Sandbox{ID: "base1", Name: "base-sb", Agent: "claude", ProjectID: p.ID, Workspace: projDir, IsBase: true}
	m.sandboxes[sb.ID] = sb
	m.byName[sb.Name] = sb.ID

	got, err := m.EnsureBaseSandbox(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("EnsureBaseSandbox: %v", err)
	}
	if got.ID != "base1" {
		t.Fatalf("got sandbox %q, want the rebuilt base1", got.ID)
	}
	if n := len(m.projectSandboxes(p.ID)); n != 1 {
		t.Fatalf("the project has %d sandboxes, want only base1 rebuilt", n)
	}
}

// A plain session's sandbox: its own, in clone mode, and cloned from the
// project's base sandbox rather than made from scratch.
func TestCreateSessionSandboxClonesTheBase(t *testing.T) {
	m, err := New(sbx.New(fakeSbx(t, "base-sb")), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	projDir := t.TempDir()
	p, err := m.CreateProject(CreateProjectRequest{Name: "demo", Path: projDir, Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	base := &Sandbox{ID: "base1", Name: "base-sb", Agent: "claude", ProjectID: p.ID, Workspace: projDir, IsBase: true}
	m.sandboxes[base.ID] = base
	m.byName[base.Name] = base.ID

	sb, err := m.CreateSessionSandbox(context.Background(), p.ID, true)
	if err != nil {
		t.Fatalf("CreateSessionSandbox: %v", err)
	}
	if !sb.Clone {
		t.Error("a session sandbox has to be in clone mode: several of them share one project folder")
	}
	if sb.IsBase {
		t.Error("a session sandbox is not the base one")
	}
	if sb.Agent != "claude" || sb.Workspace != projDir || sb.ProjectID != p.ID {
		t.Errorf("got %+v, want the project's agent, folder and id", sb)
	}
	if sb.Name == base.Name {
		t.Error("a session sandbox needs a name of its own")
	}
	// Each session gets another: they are what keeps two sessions in one
	// project out of each other's way.
	second, err := m.CreateSessionSandbox(context.Background(), p.ID, true)
	if err != nil {
		t.Fatalf("second CreateSessionSandbox: %v", err)
	}
	if second.ID == sb.ID {
		t.Error("the second session reused the first session's sandbox")
	}

	// A project folder that is not a checkout has nothing to clone, and the
	// session is mounted on the folder itself rather than refused.
	plain, err := m.CreateSessionSandbox(context.Background(), p.ID, false)
	if err != nil {
		t.Fatalf("CreateSessionSandbox without a repo: %v", err)
	}
	if plain.Clone {
		t.Error("there was nothing to clone, so the sandbox should be mounted on the folder")
	}
	if plain.Workspace != projDir {
		t.Errorf("workspace = %q, want the project folder %q", plain.Workspace, projDir)
	}
}

// A sandbox this manager never made is not one it can clone from either, so
// it does not count as the project's base one.
func TestEnsureBaseSandboxIgnoresAProjectsOtherSandboxes(t *testing.T) {
	// An sbx that does not exist: the create is expected to fail, and what is
	// being checked is that it was attempted at all.
	m, err := New(sbx.New(filepath.Join(t.TempDir(), "no-such-sbx")), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	projDir := t.TempDir()
	p, err := m.CreateProject(CreateProjectRequest{Name: "demo", Path: projDir, Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	clone := &Sandbox{ID: "c1", Name: "clone-sb", Agent: "claude", ProjectID: p.ID, Workspace: projDir, Clone: true}
	m.sandboxes[clone.ID] = clone
	m.byName[clone.Name] = clone.ID

	if _, err := m.EnsureBaseSandbox(context.Background(), p.ID); err == nil {
		t.Fatal("want the create to have been attempted, and to have failed without an sbx")
	}
	if m.BaseSandbox(p.ID) != nil {
		t.Fatal("nothing should have been registered as the base sandbox")
	}
}

// Nothing may be run in or deleted on the base sandbox: it is what every
// session sandbox is cloned from, and both would change what the next session
// inherits. Stopping it is ordinary — nothing in there is working, and a
// project would otherwise hold a container up for as long as it existed.
func TestBaseSandboxRefusesEverythingButProjectDeletion(t *testing.T) {
	m, err := New(sbx.New(filepath.Join(t.TempDir(), "no-such-sbx")), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := &Sandbox{ID: "base1", Name: "base-sb", Agent: "claude", IsBase: true}
	m.sandboxes[base.ID] = base
	m.byName[base.Name] = base.ID

	ctx := context.Background()
	if _, err := m.StartSession(ctx, base.ID, StartSessionRequest{Kind: KindShell}); !errors.Is(err, ErrBaseSandbox) {
		t.Errorf("StartSession: %v, want ErrBaseSandbox", err)
	}
	// The stop reaches sbx, which is not there in this test: what matters is
	// that it was not turned away before it got that far.
	if err := m.StopSandbox(ctx, base.ID); errors.Is(err, ErrBaseSandbox) {
		t.Error("StopSandbox refused the base sandbox; stopping one is allowed")
	}
	if err := m.DeleteSandbox(ctx, base.ID); !errors.Is(err, ErrBaseSandbox) {
		t.Errorf("DeleteSandbox: %v, want ErrBaseSandbox", err)
	}
	if _, err := m.GetSandbox(base.ID); err != nil {
		t.Errorf("the base sandbox should still be here: %v", err)
	}
}

// Deleting the project is the one thing that does take the base sandbox with
// it: there is nothing left for it to be the base of.
func TestDeleteProjectRemovesTheBaseSandbox(t *testing.T) {
	m, err := New(sbx.New(filepath.Join(t.TempDir(), "no-such-sbx")), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := m.CreateProject(CreateProjectRequest{Name: "demo", Path: t.TempDir(), Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	base := &Sandbox{ID: "base1", Name: "base-sb", Agent: "claude", ProjectID: p.ID, IsBase: true}
	m.sandboxes[base.ID] = base
	m.byName[base.Name] = base.ID

	if _, err := m.DeleteProject(context.Background(), p.ID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if _, err := m.GetSandbox(base.ID); !errors.Is(err, ErrSandboxNotFound) {
		t.Fatalf("base sandbox should be gone, got %v", err)
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
	p, err := m.CreateProject(CreateProjectRequest{Name: "demo", Path: projDir, Agent: "claude"})
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
	p, err := m.CreateProject(CreateProjectRequest{Name: "demo", Path: projDir, Agent: "claude"})
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
