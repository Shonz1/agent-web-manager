package manager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Shonz1/agent-web-manager/internal/sbx"
)

// ErrProjectNotFound is returned when a project id names nothing this
// manager knows about.
var ErrProjectNotFound = errors.New("project not found")

// Project is a folder the user works in. It is the durable, user-facing
// container for sessions, and it owns three kinds of sandbox, all of which
// the project view treats as an implementation detail:
//
//   - the base sandbox, mounted on the project's folder, made with the
//     project and never worked in — see EnsureBaseSandbox;
//   - one clone sandbox per plain session, cloned from the base, holding a
//     git clone of the folder rather than the folder itself — see
//     CreateSessionSandbox;
//   - one sandbox per session given a worktree of its own.
//
// The agent is the project's, not the sandbox's: it is chosen when the
// project is created and every sandbox under it is built for it.
type Project struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	Agent     string    `json:"agent"`
	CreatedAt time.Time `json:"createdAt"`
	// LastActivityAt is the last time a person used any session in any of this
	// project's sandboxes — the same idea as Sandbox.LastActivityAt, and what
	// orders the project list most-recently-used first.
	LastActivityAt time.Time `json:"lastActivityAt"`
	// NoPlugins stops this project's sandboxes being given a copy of the Claude
	// Code plugins the base sandbox has — see plugins.go for why they are
	// copied at all. Stated negatively so that the zero value is the copy: a
	// project written before this setting existed asked for nothing, and what
	// it had was the plugins.
	NoPlugins bool `json:"noPlugins,omitempty"`
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
	// Agent every sandbox in the project is built for. It is asked for here
	// rather than per session because a session's sandbox is a clone of the
	// project's base one, and a clone cannot be of a different agent than
	// what it was cloned from.
	Agent string `json:"agent"`
}

// CreateProject registers a project. Its base sandbox is not made here — see
// EnsureBaseSandbox, which the caller runs once the project exists, so that
// an image pull does not hold up the answer to "create this project".
func (m *Manager) CreateProject(req CreateProjectRequest) (*Project, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, fmt.Errorf("project name is required")
	}
	agent := strings.TrimSpace(req.Agent)
	if !sbx.ValidAgent(agent) {
		return nil, fmt.Errorf("unknown agent %q", agent)
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
		Agent:          agent,
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

// SetProjectPlugins says whether the sandboxes this project makes from now on
// are given a copy of the Claude Code plugins the base sandbox has.
//
// It reaches nothing that already exists, for the reason SetProjectModel does
// not: a sandbox was filled when it was made, and an agent running in one has
// long since read what it found. Turning the copy off leaves the plugins where
// they are and stops the next sandbox being given any; turning it back on
// fills the one after that, not the ones already here.
func (m *Manager) SetProjectPlugins(id string, noPlugins bool) (*Project, error) {
	m.mu.Lock()
	p, ok := m.projects[id]
	if !ok {
		m.mu.Unlock()
		return nil, ErrProjectNotFound
	}
	p.NoPlugins = noPlugins
	m.mu.Unlock()

	if err := m.saveProjects(); err != nil {
		return nil, err
	}
	if noPlugins {
		log.Printf("plugins: %s keeps none, so the sandboxes it makes from now on start with none", p.Name)
	} else {
		log.Printf("plugins: %s passes them on again, to the sandboxes it makes from now on", p.Name)
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
		// deleteSandbox rather than DeleteSandbox: the base sandbox is
		// refused everywhere else, and here is where it is meant to go.
		if err := m.deleteSandbox(ctx, sb); err != nil {
			return worktrees, err
		}
	}

	m.mu.Lock()
	delete(m.projects, id)
	m.mu.Unlock()

	m.baseMu.Lock()
	delete(m.baseLocks, id)
	m.baseMu.Unlock()

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

// BaseSandbox returns the project's base sandbox, or nil if it has not been
// made yet — which is only ever briefly, while EnsureBaseSandbox is still
// building it.
func (m *Manager) BaseSandbox(projectID string) *Sandbox {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, sb := range m.sandboxes {
		if sb.ProjectID == projectID && sb.IsBase {
			return sb
		}
	}
	return nil
}

// EnsureBaseSandbox returns the project's base sandbox, making it if the
// project has none — at project creation, and again at the start of any
// session that finds it gone, since it is what every session sandbox is
// cloned from. A record that survived while the container behind it did not
// is rebuilt rather than duplicated.
//
// Calls for one project are serialised: the first session started in a brand
// new project races the create that made it, and both want the same sandbox.
//
// The project is looked up again on either side of the create, because a
// create is an image pull's worth of minutes and DeleteProject does not wait
// for one. A base sandbox registered after its project has gone is a sandbox
// nothing will ever collect: DeleteSandbox refuses a base sandbox, and there
// is no longer a project to delete it with.
func (m *Manager) EnsureBaseSandbox(ctx context.Context, projectID string) (*Sandbox, error) {
	lock := m.baseLock(projectID)
	lock.Lock()
	defer lock.Unlock()

	p, err := m.GetProject(projectID)
	if err != nil {
		return nil, err
	}

	if sb := m.BaseSandbox(projectID); sb != nil {
		// The record is here; the container may not be, if sbx was pruned or
		// the sandbox removed by hand.
		if err := m.ensureSandbox(ctx, sb); err != nil {
			return nil, err
		}
		return sb, nil
	}
	// Unnamed: CreateSandbox derives "<agent>-<dir>" from the workspace and
	// numbers it past anything already holding that name.
	sb, err := m.CreateSandbox(CreateSandboxRequest{
		Agent:     p.Agent,
		Workspace: p.Path,
		ProjectID: p.ID,
		IsBase:    true,
		// The base sandbox takes its plugins from this machine, and it is what
		// every session sandbox is then filled from. A project that wants none
		// wants none here first of all: filling this one would put them back
		// into every session by the ordinary route.
		NoPlugins: p.NoPlugins,
	})
	if err != nil {
		return nil, err
	}
	if _, err := m.GetProject(projectID); err != nil {
		// The project went while this was being built, after DeleteProject had
		// already been past the list this sandbox has just been added to. It
		// is deleted here or it is never deleted at all — on a context of its
		// own, since the one that asked for the sandbox is as likely as not
		// the request that has just been answered.
		cleanup, cancel := context.WithTimeout(context.Background(), removeTimeout)
		defer cancel()
		if derr := m.deleteSandbox(cleanup, sb); derr != nil {
			log.Printf("project %s: its base sandbox %s outlived it: %v", projectID, sb.Name, derr)
		}
		return nil, err
	}
	return sb, nil
}

// EnsureBaseSandboxes makes the base sandbox of every project that is
// missing one. It runs at startup, so a project created against an sbx that
// was not working — or one from before base sandboxes existed — gets its
// base sandbox without waiting for someone to start a session in it.
//
// Failures are returned per project rather than stopping the sweep: one
// project whose folder has gone must not leave every other project without a
// base sandbox.
func (m *Manager) EnsureBaseSandboxes(ctx context.Context) map[string]error {
	m.mu.RLock()
	ids := make([]string, 0, len(m.projects))
	for id := range m.projects {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	sort.Strings(ids)

	errs := map[string]error{}
	for _, id := range ids {
		if _, err := m.EnsureBaseSandbox(ctx, id); err != nil {
			errs[id] = err
		}
	}
	return errs
}

// CreateSessionSandbox makes the sandbox a plain session runs in: a copy of
// the project's base one, in sbx's clone mode, so its workspace is a git
// clone of the project folder rather than the folder itself.
//
// That is what lets every session have a sandbox of its own without a
// worktree apiece: several clones of one folder can run at once, and nothing
// any of them does reaches the host until someone fetches it from the
// sandbox.
//
// clone is false for a project folder that is not a git checkout, which has
// nothing to clone: those sessions get a sandbox of their own mounted on the
// folder itself, and share it the way two shells on one machine do. Only the
// caller can tell, which is why it is asked rather than worked out here.
//
// kits are the sbx kits the session asked for, by name. They belong to the
// session rather than to the project because they are chosen with it, and
// because a kit can only ever go on a sandbox being made — which, for a
// session, is this one.
func (m *Manager) CreateSessionSandbox(ctx context.Context, projectID string, clone bool, kits []string) (*Sandbox, error) {
	p, err := m.GetProject(projectID)
	if err != nil {
		return nil, err
	}
	base, err := m.EnsureBaseSandbox(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return m.CreateSandbox(CreateSandboxRequest{
		Name:      DefaultProjectSandboxName(p.Name),
		Agent:     p.Agent,
		Workspace: p.Path,
		ProjectID: p.ID,
		Clone:     clone,
		Kits:      kits,
		// The whole point of the base sandbox: what a session inherits comes
		// from a settled sandbox of this project's own rather than from this
		// machine, or from whichever session sandbox happened to be first.
		PluginsFrom: base.Name,
		NoPlugins:   p.NoPlugins,
	})
}

// baseLock returns the mutex serialising base sandbox creation for one
// project, making it on first use.
func (m *Manager) baseLock(projectID string) *sync.Mutex {
	m.baseMu.Lock()
	defer m.baseMu.Unlock()
	lock, ok := m.baseLocks[projectID]
	if !ok {
		lock = &sync.Mutex{}
		m.baseLocks[projectID] = lock
	}
	return lock
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
