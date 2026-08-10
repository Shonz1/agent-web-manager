package manager

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/oleksiiipatov/agent-web-manager/internal/sbx"
)

// ErrProjectNotFound is returned when a project id names nothing this
// manager knows about.
var ErrProjectNotFound = errors.New("project not found")

// Project is a folder the user works in. It is the durable, user-facing
// container for sessions: unlike a Sandbox, it carries no agent of its own.
// A project has at most one sandbox mounted directly on its folder — made
// the first time a session is started in it without a worktree, and reused
// by every such session after — plus one sandbox per session given a
// worktree of its own. Both kinds are an implementation detail the project
// view hides; see EnsureProjectSandbox.
type Project struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"createdAt"`
	// LastActivityAt is the last time a person used any session in any of this
	// project's sandboxes — the same idea as Sandbox.LastActivityAt, and what
	// orders the project list most-recently-used first.
	LastActivityAt time.Time `json:"lastActivityAt"`
}

// ProjectView is the JSON-facing snapshot of a project: the persisted
// record, the sandboxes it owns, and every session running in any of them,
// flattened and ordered most-recently-active first — which is what the
// primary UI shows in place of the sandboxes themselves.
type ProjectView struct {
	Project
	Sandboxes []SandboxView `json:"sandboxes"`
	Sessions  []SessionView `json:"sessions"`
}

// CreateProjectRequest describes a project the user wants to create.
type CreateProjectRequest struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// CreateProject registers a project. No sandbox is made yet: one is created
// the first time a session is started in it, by EnsureProjectSandbox or the
// worktree session flow.
func (m *Manager) CreateProject(req CreateProjectRequest) (*Project, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, fmt.Errorf("project name is required")
	}
	path, err := resolveWorkspace(req.Path)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	p := &Project{
		ID:             newID(),
		Name:           name,
		Path:           path,
		CreatedAt:      now,
		LastActivityAt: now,
	}

	m.mu.Lock()
	m.projects[p.ID] = p
	m.mu.Unlock()

	if err := m.saveProjects(); err != nil {
		return nil, err
	}
	return p, nil
}

// GetProject returns a project by ID.
func (m *Manager) GetProject(id string) (*Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.projects[id]
	if !ok {
		return nil, ErrProjectNotFound
	}
	return p, nil
}

// ListProjects returns every project, most recently active first, with its
// live sandbox status and its sessions.
func (m *Manager) ListProjects(ctx context.Context) []ProjectView {
	status := m.sandboxStatuses(ctx)

	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]ProjectView, 0, len(m.projects))
	for _, p := range m.projects {
		out = append(out, m.projectViewLocked(p, status))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastActivityAt.After(out[j].LastActivityAt) })
	return out
}

// GetProjectView returns one project with its live sandbox status and
// sessions.
func (m *Manager) GetProjectView(ctx context.Context, id string) (ProjectView, error) {
	status := m.sandboxStatuses(ctx)

	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.projects[id]
	if !ok {
		return ProjectView{}, ErrProjectNotFound
	}
	return m.projectViewLocked(p, status), nil
}

// projectViewLocked assembles a project's view. The caller holds m.mu.
func (m *Manager) projectViewLocked(p *Project, status map[string]string) ProjectView {
	v := ProjectView{Project: *p, Sandboxes: []SandboxView{}, Sessions: []SessionView{}}
	for _, sb := range m.sandboxes {
		if sb.ProjectID != p.ID {
			continue
		}
		sv := m.viewLocked(sb, status)
		v.Sandboxes = append(v.Sandboxes, sv)
		v.Sessions = append(v.Sessions, sv.Sessions...)
	}
	sort.Slice(v.Sessions, func(i, j int) bool {
		return v.Sessions[i].LastActivityAt.After(v.Sessions[j].LastActivityAt)
	})
	return v
}

// DeleteProject stops and destroys every sandbox the project owns and
// forgets the project. It reports the worktree sandboxes it removed so the
// caller — which alone knows how to talk to git — can clean up their
// checkouts; those already removed are reported even if a later one fails.
func (m *Manager) DeleteProject(ctx context.Context, id string) ([]Sandbox, error) {
	if _, err := m.GetProject(id); err != nil {
		return nil, err
	}

	var worktrees []Sandbox
	for _, sb := range m.projectSandboxes(id) {
		if sb.IsWorktree {
			worktrees = append(worktrees, *sb)
		}
		if err := m.DeleteSandbox(ctx, sb.ID); err != nil {
			return worktrees, err
		}
	}

	m.mu.Lock()
	delete(m.projects, id)
	m.mu.Unlock()

	if err := m.saveProjects(); err != nil {
		return worktrees, err
	}
	return worktrees, nil
}

// projectSandboxes returns every sandbox belonging to a project.
func (m *Manager) projectSandboxes(projectID string) []*Sandbox {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Sandbox
	for _, sb := range m.sandboxes {
		if sb.ProjectID == projectID {
			out = append(out, sb)
		}
	}
	return out
}

// mainSandbox returns the project's non-worktree sandbox, or nil if it has
// not made one yet.
func (m *Manager) mainSandbox(projectID string) *Sandbox {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, sb := range m.sandboxes {
		if sb.ProjectID == projectID && !sb.IsWorktree {
			return sb
		}
	}
	return nil
}

// EnsureProjectSandbox returns the project's main sandbox, creating it with
// the given agent if this is the first session started in the project
// without a worktree. Every later call reuses that same sandbox regardless
// of the agent it names: a project has only one non-worktree agent, fixed by
// whichever session made the sandbox.
func (m *Manager) EnsureProjectSandbox(ctx context.Context, projectID, agent string) (*Sandbox, error) {
	if sb := m.mainSandbox(projectID); sb != nil {
		return sb, nil
	}
	p, err := m.GetProject(projectID)
	if err != nil {
		return nil, err
	}
	if !sbx.ValidAgent(agent) {
		return nil, fmt.Errorf("unknown agent %q", agent)
	}
	name, err := m.uniqueSandboxName(defaultName(agent, p.Path))
	if err != nil {
		return nil, err
	}
	sb, err := m.CreateSandbox(CreateSandboxRequest{
		Name:      name,
		Agent:     agent,
		Workspace: p.Path,
		ProjectID: p.ID,
	})
	if errors.Is(err, ErrExists) {
		// Two sessions started in the same project at once — the loser here
		// is not the loser overall, since the sandbox that won is exactly
		// what this call would have made.
		if existing := m.mainSandbox(projectID); existing != nil {
			return existing, nil
		}
	}
	return sb, err
}

// uniqueSandboxName appends a number to base until it names no sandbox this
// manager already knows about, the way uniqueTitleLocked does for session
// titles.
func (m *Manager) uniqueSandboxName(base string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, taken := m.byName[base]; !taken {
		return base, nil
	}
	for n := 2; n < 1000; n++ {
		candidate := fmt.Sprintf("%s-%d", base, n)
		if _, taken := m.byName[candidate]; !taken {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not find a free name starting with %q", base)
}
