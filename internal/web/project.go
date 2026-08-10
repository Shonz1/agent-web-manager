package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/oleksiiipatov/agent-web-manager/internal/git"
	"github.com/oleksiiipatov/agent-web-manager/internal/manager"
	"github.com/oleksiiipatov/agent-web-manager/internal/sbx"
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
	IsWorktree bool   `json:"isWorktree,omitempty"`
}

// sandboxSummary is as much of a sandbox as the project view exposes: enough
// for the "new session" dialog to say whether a project already has a main
// sandbox and, if so, what agent it is fixed to.
type sandboxSummary struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Agent  string `json:"agent"`
	Status string `json:"status"`
}

// projectView answers a project request with its sessions decorated by
// branch, in place of the sandboxes manager.ProjectView carries them in —
// the project tree has no use for the sandboxes themselves, only for what
// is running in them and, for the main one, what agent a new non-worktree
// session would join.
type projectView struct {
	manager.Project
	MainSandbox *sandboxSummary      `json:"mainSandbox,omitempty"`
	Sessions    []projectSessionView `json:"sessions"`
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
	branchFor := func(workspace string) string {
		if workspace == "" {
			return ""
		}
		if b, ok := branches[workspace]; ok {
			return b
		}
		b := g.Branch(ctx, workspace)
		branches[workspace] = b
		return b
	}

	out := projectView{Project: v.Project, Sessions: make([]projectSessionView, 0, len(v.Sessions))}
	for _, sb := range v.Sandboxes {
		if sb.IsWorktree {
			continue
		}
		out.MainSandbox = &sandboxSummary{ID: sb.ID, Name: sb.Name, Agent: sb.Agent, Status: sb.Status}
		break
	}
	for _, sess := range v.Sessions {
		sb := byID[sess.SandboxID]
		out.Sessions = append(out.Sessions, projectSessionView{
			SessionView: sess,
			Branch:      branchFor(sb.Workspace),
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

// startProjectSessionRequest asks for a session in a project: either in its
// main sandbox — made the first time this is asked for, and reused by every
// such request after — or, with Worktree set, in a fresh sandbox mounted on
// a branch and checkout of its own.
type startProjectSessionRequest struct {
	manager.StartSessionRequest
	// Agent picks the sandbox's agent. It only matters the first time a
	// project's main sandbox is made, and always for a worktree session,
	// which gets a sandbox — and so an agent — of its own every time.
	Agent string `json:"agent"`
	// Worktree, when true, gives the session a branch and checkout of its
	// own rather than running in the project's main sandbox.
	Worktree bool   `json:"worktree"`
	Branch   string `json:"branch"`
	// Path is where the worktree goes. Empty puts it beside the repository.
	Path string `json:"path"`
	// Name overrides a new worktree sandbox's default name. Ignored outside
	// the worktree case: the project's main sandbox already has whatever
	// name it was given when it was made.
	Name string `json:"name"`
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

	// Starting a session can take minutes: EnsureProjectSandbox may have to
	// pull an agent image the first time, exactly like a plain sandbox create.
	ctx, cancel := context.WithTimeout(context.Background(), createTimeout)
	defer cancel()

	sb, err := s.mgr.EnsureProjectSandbox(ctx, proj.ID, strings.TrimSpace(req.Agent))
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	sess, err := s.mgr.StartSession(ctx, sb.ID, req.StartSessionRequest)
	if err != nil {
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
	agent := strings.TrimSpace(req.Agent)
	if !sbx.ValidAgent(agent) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("unknown agent %q", agent))
		return
	}

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

	created, err := s.mgr.CreateSandbox(manager.CreateSandboxRequest{
		Name:      strings.TrimSpace(req.Name),
		Agent:     agent,
		Workspace: tree.Path,
		// The repository the worktree belongs to, so the agent has git at
		// all — a worktree's ".git" is a file pointing back into it.
		ExtraWorkspaces: withRepoMount(nil, tree.RepoRoot),
		ProjectID:       proj.ID,
		IsWorktree:      true,
		RepoRoot:        tree.RepoRoot,
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
