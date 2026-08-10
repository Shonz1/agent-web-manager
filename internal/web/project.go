package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/Shonz1/agent-web-manager/internal/git"
	"github.com/Shonz1/agent-web-manager/internal/manager"
)

// projectTimeout bounds a project read: assembling a view walks its
// sandboxes and, for each one, runs a git command to read its branch.
const projectTimeout = 15 * time.Second

// projectSessionView is a session as the project tree shows it: the same
// view a sandbox exposes, plus the git branch of the sandbox running it —
// what the tree shows as a session's subtitle in place of the sandbox that
// carries it, which the tree does not show at all.
type projectSessionView struct {
	manager.SessionView
	Branch     string `json:"branch,omitempty"`
	Clone      bool   `json:"clone,omitempty"`
	IsWorktree bool   `json:"isWorktree,omitempty"`
}

// sandboxSummary is as much of a sandbox as the project view exposes:
// enough for the "new session" dialog to say what the project's base sandbox
// is doing, and enough for the project panel to list the sandboxes
// themselves — which is the only place a sandbox left behind by a session
// that has since ended can be seen, and removed.
type sandboxSummary struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Agent     string `json:"agent"`
	Status    string `json:"status"`
	Workspace string `json:"workspace,omitempty"`
	Branch    string `json:"branch,omitempty"`
	// IsBase, Clone and IsWorktree are what the UI marks a sandbox by, and
	// what tells it which actions to refuse: nothing can be done to a base
	// sandbox on its own.
	IsBase     bool `json:"isBase,omitempty"`
	Clone      bool `json:"clone,omitempty"`
	IsWorktree bool `json:"isWorktree,omitempty"`
	// Kits it was built with, by name. A kit cannot be added to a sandbox
	// afterwards, so this is not a setting to change from here — it is the
	// only place a session's kits can be seen once it is running.
	Kits []string `json:"kits,omitempty"`
	// Sessions counts what is running in the sandbox right now, which is what
	// says whether removing it would take a live terminal with it.
	Sessions int `json:"sessions"`
}

// projectView answers a project request with its sessions decorated by
// branch, and its sandboxes summarised. Sessions are what the project tree
// shows; the sandboxes are there because they outlive every session in them
// — a manager restart ends the sessions and leaves the sandboxes — and
// without them a worktree sandbox would be invisible from the moment its
// session ended.
type projectView struct {
	manager.Project
	// BaseSandbox is the one every session sandbox is cloned from. It is
	// absent only in the moments between a project being created and its base
	// sandbox finishing — which the UI says out loud, since no session can
	// start until it is there.
	BaseSandbox *sandboxSummary      `json:"baseSandbox,omitempty"`
	Sandboxes   []sandboxSummary     `json:"sandboxes"`
	Sessions    []projectSessionView `json:"sessions"`
	// Repo says whether the project's folder is a git checkout, which decides
	// both of the things the new-session dialog has to say: whether a session
	// gets a clone of it, and whether a worktree can be offered at all.
	Repo bool `json:"repo"`
}

// decorateProject reads each of a project's sandboxes' branches — once per
// distinct workspace, since a sandbox can carry several sessions — and
// attaches them to the sessions running in it.
func decorateProject(ctx context.Context, g *git.Client, v manager.ProjectView) projectView {
	byID := make(map[string]manager.SandboxView, len(v.Sandboxes))
	for _, sb := range v.Sandboxes {
		byID[sb.ID] = sb
	}
	branches := make(map[string]string, len(v.Sandboxes))
	// A clone sandbox has a branch of its own, but only inside the container:
	// its workspace path on the host holds the project folder, whose branch
	// is somebody else's. Reading the real one would mean an "sbx exec" per
	// sandbox on every refresh of the project list — and starting whichever
	// of them are stopped — so it goes unsaid rather than misreported.
	branchFor := func(sb manager.SandboxView) string {
		if sb.Workspace == "" || sb.Clone {
			return ""
		}
		if b, ok := branches[sb.Workspace]; ok {
			return b
		}
		b := g.Branch(ctx, sb.Workspace)
		branches[sb.Workspace] = b
		return b
	}

	out := projectView{
		Project:   v.Project,
		Sandboxes: make([]sandboxSummary, 0, len(v.Sandboxes)),
		Sessions:  make([]projectSessionView, 0, len(v.Sessions)),
		Repo:      g.IsRepo(ctx, v.Path),
	}
	// The manager holds its sandboxes in a map, so an order has to be put on
	// them here: the base sandbox first — it is what the others came from —
	// then the session sandboxes, most recently used first.
	boxes := append([]manager.SandboxView(nil), v.Sandboxes...)
	sort.SliceStable(boxes, func(i, j int) bool {
		if boxes[i].IsBase != boxes[j].IsBase {
			return boxes[i].IsBase
		}
		return boxes[i].LastActivityAt.After(boxes[j].LastActivityAt)
	})
	for _, sb := range boxes {
		out.Sandboxes = append(out.Sandboxes, sandboxSummary{
			ID:         sb.ID,
			Name:       sb.Name,
			Agent:      sb.Agent,
			Status:     sb.Status,
			Workspace:  sb.Workspace,
			Branch:     branchFor(sb),
			IsBase:     sb.IsBase,
			Clone:      sb.Clone,
			IsWorktree: sb.IsWorktree,
			Kits:       sb.Kits,
			Sessions:   len(sb.Sessions),
		})
	}
	for i := range out.Sandboxes {
		if !out.Sandboxes[i].IsBase {
			continue
		}
		base := out.Sandboxes[i]
		out.BaseSandbox = &base
		break
	}
	for _, sess := range v.Sessions {
		sb := byID[sess.SandboxID]
		out.Sessions = append(out.Sessions, projectSessionView{
			SessionView: sess,
			Branch:      branchFor(sb),
			Clone:       sb.Clone,
			IsWorktree:  sb.IsWorktree,
		})
	}
	return out
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), projectTimeout)
	defer cancel()

	views := s.mgr.ListProjects(ctx)
	out := make([]projectView, 0, len(views))
	for _, v := range views {
		out = append(out, decorateProject(ctx, s.gitClient(), v))
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": out})
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var req manager.CreateProjectRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p, err := s.mgr.CreateProject(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// The base sandbox is made in the background: it may have an agent image
	// to pull, and holding the answer to "create this project" for minutes to
	// report a sandbox the user did not ask about by name would be a poor
	// trade. The project appears with it still building, and a session
	// started before it is ready waits for the same create rather than
	// starting a second one.
	s.startBaseSandbox(p.ID)

	ctx, cancel := context.WithTimeout(r.Context(), projectTimeout)
	defer cancel()
	view, err := s.mgr.GetProjectView(ctx, p.ID)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, decorateProject(ctx, s.gitClient(), view))
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), projectTimeout)
	defer cancel()

	view, err := s.mgr.GetProjectView(ctx, r.PathValue("id"))
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, decorateProject(ctx, s.gitClient(), view))
}

// handleDeleteProject removes a project along with every sandbox it owns.
// This is irreversible, and — for a project with worktree sessions —
// deletes the checkouts those worktrees made on the host.
func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	worktrees, err := s.mgr.DeleteProject(ctx, r.PathValue("id"))
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	for _, sb := range worktrees {
		s.removeWorktree(git.Worktree{Path: sb.Workspace, RepoRoot: sb.RepoRoot})
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePutProjectPlugins turns the plugin copy on or off for the sandboxes a
// project makes from now on. It writes nothing into a sandbox and starts none:
// the answer is a field of the project, so it is in the project view already
// and there is nothing here to read back.
func (s *Server) handlePutProjectPlugins(w http.ResponseWriter, r *http.Request) {
	// It changes what every sandbox made from now on is given, from a page that
	// need not be this one otherwise, so it takes the guard the other settings
	// endpoints take.
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, errors.New("cross-origin request"))
		return
	}

	var req struct {
		// NoPlugins stops the copy. Stated the same way round as the field it
		// sets, so that nothing between here and the file has to remember which
		// way a bool means.
		NoPlugins bool `json:"noPlugins"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	p, err := s.mgr.SetProjectPlugins(r.PathValue("id"), req.NoPlugins)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	if !req.NoPlugins {
		// The base sandbox this project fills its sessions from may have been
		// built while the copy was off, in which case it has none to pass on.
		// Filling it is what makes turning this back on mean anything.
		s.fillBasePlugins(p.ID)
	}
	writeJSON(w, http.StatusOK, p)
}

// fillBasePlugins tops up a project's base sandbox without holding up the
// request that asked for it — a plugin mirror is a run of git clones, and the
// answer to "turn this on" should not wait for them. A failure is logged, for
// the reason startBaseSandbox logs its own: there is nobody still waiting to
// be told, and the next sandbox this project makes reports it to someone who
// is.
func (s *Server) fillBasePlugins(projectID string) {
	go func() {
		if err := s.mgr.FillBasePlugins(projectID); err != nil {
			log.Printf("plugins for project %s: %v", projectID, err)
		}
	}()
}

// startBaseSandbox makes a project's base sandbox without holding up the
// request that asked for it. A failure is logged rather than reported: the
// next session started in the project tries again, and reports it then to
// somebody who is waiting for an answer.
func (s *Server) startBaseSandbox(projectID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), createTimeout)
		defer cancel()
		if _, err := s.mgr.EnsureBaseSandbox(ctx, projectID); err != nil {
			log.Printf("base sandbox for project %s: %v", projectID, err)
		}
	}()
}

// startProjectSessionRequest asks for a session in a project: either in a
// clone of its base sandbox — a fresh one every time — or, with Worktree
// set, in a sandbox mounted on a branch and checkout of its own on the host.
//
// Neither names an agent: that belongs to the project, and both kinds of
// sandbox are built for it.
type startProjectSessionRequest struct {
	manager.StartSessionRequest
	// Worktree, when true, gives the session a checkout of its own on the
	// host rather than a clone inside the sandbox.
	Worktree bool   `json:"worktree"`
	Branch   string `json:"branch"`
	// Path is where the worktree goes. Empty puts it beside the repository.
	Path string `json:"path"`
	// Kits names the sbx kits to build the session's sandbox with, from the
	// listing at /api/kits. Asked for here because a kit goes on a sandbox as
	// it is created and never afterwards, and this is where the session's
	// sandbox is made — whichever kind of workspace it is given.
	Kits []string `json:"kits,omitempty"`
}

// startProjectSessionResponse reports what starting a project session made:
// always a session and the sandbox it runs in, plus the worktree behind that
// sandbox when one was asked for.
type startProjectSessionResponse struct {
	Worktree *git.Worktree       `json:"worktree,omitempty"`
	Sandbox  manager.SandboxView `json:"sandbox"`
	Session  manager.SessionView `json:"session"`
}

// handleStartProjectSession is the primary way a session gets started: named
// by project rather than by sandbox, so the sandbox underneath — the
// project's main one, or a fresh one on a worktree — is made or found for
// the caller instead of having to be picked first.
func (s *Server) handleStartProjectSession(w http.ResponseWriter, r *http.Request) {
	// A worktree request writes to the host filesystem, so this takes the
	// same guard as the WebSocket and the sandbox-scoped worktree endpoint
	// rather than trusting the method for every request through here.
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, fmt.Errorf("cross-origin request rejected"))
		return
	}

	var req startProjectSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	proj, err := s.mgr.GetProject(r.PathValue("id"))
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}

	if req.Worktree {
		s.startWorktreeProjectSession(w, r, proj, req)
		return
	}

	// Starting a session can take minutes: the base sandbox may still be
	// building, and the clone made from it may have an image to pull.
	ctx, cancel := context.WithTimeout(context.Background(), createTimeout)
	defer cancel()

	// A sandbox of its own for every session, cloned from the project's base
	// one: several sessions can then work on the project at once without
	// either sharing a checkout or needing a worktree apiece. A project
	// folder that is not a checkout has nothing to clone, and those sessions
	// are mounted on the folder itself instead.
	sb, err := s.mgr.CreateSessionSandbox(ctx, proj.ID, s.gitClient().IsRepo(ctx, proj.Path), req.Kits)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	sess, err := s.mgr.StartSession(ctx, sb.ID, req.StartSessionRequest)
	if err != nil {
		// The sandbox stays: it is what was asked for, and "Start agent"
		// retries the session in it without making another.
		writeError(w, statusFor(err), err)
		return
	}
	view, err := s.mgr.SandboxView(ctx, sb.ID)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, startProjectSessionResponse{Sandbox: view, Session: sess.View()})
}

// startWorktreeProjectSession gives a session a branch of the project's
// repository checked out in a directory of its own, a sandbox mounted on
// that, and starts the session in it — the project-scoped counterpart of
// handleStartWorktreeSession, which does the same for a sandbox already
// chosen rather than a project.
func (s *Server) startWorktreeProjectSession(w http.ResponseWriter, r *http.Request, proj *manager.Project, req startProjectSessionRequest) {
	gitCtx, cancelGit := context.WithTimeout(r.Context(), worktreeTimeout)
	defer cancelGit()

	tree, err := s.gitClient().AddWorktree(gitCtx, proj.Path, req.Path, req.Branch)
	if errors.Is(err, git.ErrNotRepo) {
		writeError(w, http.StatusBadRequest,
			fmt.Errorf("project %s is not a git checkout, so it has no worktrees", proj.Path))
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	// Detached from the request, as the plain create is: an image pull must
	// not be cancelled because the browser gave up waiting for the answer.
	ctx, cancel := context.WithTimeout(context.Background(), createTimeout)
	defer cancel()

	// The base sandbox is what this one takes its plugins and its model from,
	// the same as a clone session's sandbox does. A worktree made for a
	// session that then cannot start is rolled back, exactly as it is when
	// the sandbox itself fails.
	base, err := s.mgr.EnsureBaseSandbox(ctx, proj.ID)
	if err != nil {
		s.removeWorktree(tree)
		writeError(w, statusFor(err), err)
		return
	}

	created, err := s.mgr.CreateSandbox(manager.CreateSandboxRequest{
		// Named after the project plus a random slug, never after the branch
		// or by the browser — see manager.DefaultProjectSandboxName.
		Name:      manager.DefaultProjectSandboxName(proj.Name),
		Agent:     proj.Agent,
		Workspace: tree.Path,
		// The repository the worktree belongs to, so the agent has git at
		// all — a worktree's ".git" is a file pointing back into it.
		ExtraWorkspaces: withRepoMount(nil, tree.RepoRoot),
		ProjectID:       proj.ID,
		IsWorktree:      true,
		RepoRoot:        tree.RepoRoot,
		// The kits chosen with the session, as a clone session's sandbox gets
		// them: which workspace a session was given says nothing about what it
		// should have been built with.
		Kits: req.Kits,
		// From the project's base sandbox, as a clone session's is: what a
		// session inherits should not depend on which kind of workspace it
		// was given.
		PluginsFrom: base.Name,
	})
	if err != nil {
		// The worktree was made for a sandbox that never came up. Leaving it
		// would only make the next attempt at the same branch fail on a
		// directory that already exists.
		s.removeWorktree(tree)
		writeError(w, statusFor(err), err)
		return
	}

	sess, err := s.mgr.StartSession(ctx, created.ID, req.StartSessionRequest)
	if err != nil {
		// The sandbox and its worktree stay: they are what was asked for, and
		// retrying the session does not need either redone.
		writeError(w, statusFor(err), err)
		return
	}

	view, err := s.mgr.SandboxView(ctx, created.ID)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, startProjectSessionResponse{
		Worktree: &tree,
		Sandbox:  view,
		Session:  sess.View(),
	})
}
