package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/oleksiiipatov/agent-web-manager/internal/git"
	"github.com/oleksiiipatov/agent-web-manager/internal/manager"
)

// worktreeTimeout bounds the git half of the call. Adding a worktree writes a
// fresh checkout, which on a large repository is real work — and nothing like
// the create budget the sandbox half gets.
const worktreeTimeout = 2 * time.Minute

// startWorktreeRequest asks for a session in a worktree of a sandbox's
// workspace rather than in the workspace itself. The session half is the same
// request the plain endpoint takes, so a caller that has one already needs only
// to name a branch.
type startWorktreeRequest struct {
	manager.StartSessionRequest
	Branch string `json:"branch"`
	// Path is where the worktree goes. Empty puts it beside the repository.
	Path string `json:"path"`
	// NoPlugins starts the new sandbox without the Claude Code plugins the one
	// it was branched from has.
	NoPlugins bool `json:"noPlugins"`
}

// worktreeSessionResponse reports all three things that were made, because the
// caller asked for one and got a sandbox as well as a session.
type worktreeSessionResponse struct {
	Worktree git.Worktree        `json:"worktree"`
	Sandbox  manager.SandboxView `json:"sandbox"`
	Session  manager.SessionView `json:"session"`
}

// handleStartWorktreeSession spins work off into a worktree: a branch of the
// sandbox's workspace checked out in a directory of its own, a sandbox mounted
// on that, and a session started in there.
//
// A sandbox's mounts are fixed when it is created, so a session cannot be moved
// onto a worktree afterwards — which is why this makes a sandbox rather than
// reusing the one it was asked from, and why the choice is offered when a
// session is started rather than at any point after.
//
// The repository the worktree came from is mounted alongside it. A worktree's
// ".git" is a file pointing into the main repository by absolute path, and
// without the other end of it the agent has no git at all: no log, no commit,
// not even a branch name.
func (s *Server) handleStartWorktreeSession(w http.ResponseWriter, r *http.Request) {
	// This writes to the host filesystem, which no other create does, so it
	// takes the same guard as the WebSocket rather than trusting the method.
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, fmt.Errorf("cross-origin request rejected"))
		return
	}

	var req startWorktreeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	sb, err := s.mgr.GetSandbox(r.PathValue("id"))
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	if sb.Workspace == "" {
		writeError(w, http.StatusBadRequest,
			errors.New("this sandbox has no workspace mounted, so there is nothing to make a worktree of"))
		return
	}

	gitCtx, cancelGit := context.WithTimeout(r.Context(), worktreeTimeout)
	defer cancelGit()

	tree, err := s.gitClient().AddWorktree(gitCtx, sb.Workspace, req.Path, req.Branch)
	if errors.Is(err, git.ErrNotRepo) {
		writeError(w, http.StatusBadRequest,
			fmt.Errorf("workspace %s is not a git checkout, so it has no worktrees", sb.Workspace))
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	// Detached from the request, as the plain create is: an image pull must not
	// be cancelled because the browser gave up waiting for the answer.
	ctx, cancel := context.WithTimeout(context.Background(), createTimeout)
	defer cancel()

	created, err := s.mgr.CreateSandbox(manager.CreateSandboxRequest{
		// Named here rather than by the caller: what a worktree sandbox is
		// called is this manager's business, not the browser's.
		Name:      manager.DefaultWorktreeSandboxName(s.worktreeProjectName(sb)),
		Agent:     sb.Agent,
		Workspace: tree.Path,
		// What the source sandbox had, plus the repository the worktree belongs
		// to. Published ports are deliberately not carried over: two containers
		// cannot bind the same host port, and this one is meant to run beside
		// the sandbox it came from rather than instead of it.
		ExtraWorkspaces: withRepoMount(sb.ExtraWorkspaces, tree.RepoRoot),
		// A branch of a sandbox's work should be a branch of its tools too.
		// Plugins are the one part of a sandbox that a new one does not
		// inherit on its own, so they are copied across from the sandbox this
		// worktree was taken from rather than from the manager's own machine.
		PluginsFrom: sb.Name,
		NoPlugins:   req.NoPlugins,
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
		// "Start agent" tries the session again without redoing either.
		writeError(w, statusFor(err), err)
		return
	}

	view, err := s.mgr.SandboxView(ctx, created.ID)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, worktreeSessionResponse{
		Worktree: tree,
		Sandbox:  view,
		Session:  sess.View(),
	})
}

// removeWorktree rolls one back, reporting a failure to the log rather than to
// the caller: what the caller is being told about is the sandbox that failed,
// and a directory left behind is not the answer to that.
func (s *Server) removeWorktree(tree git.Worktree) {
	ctx, cancel := context.WithTimeout(context.Background(), worktreeTimeout)
	defer cancel()
	if err := s.gitClient().RemoveWorktree(ctx, tree.RepoRoot, tree.Path); err != nil {
		log.Printf("worktree: %s is left behind: %v", tree.Path, err)
	}
}

// withRepoMount adds the repository a worktree belongs to. An entry already
// naming it is dropped rather than kept, because one of them may be read-only:
// the worktree's own administrative files live inside that repository, and an
// agent that cannot write there cannot so much as commit.
func withRepoMount(extras []string, repoRoot string) []string {
	out := make([]string, 0, len(extras)+1)
	for _, e := range extras {
		if mountPath(e) == repoRoot {
			continue
		}
		out = append(out, e)
	}
	return append(out, repoRoot)
}

// mountPath is an extra workspace without the ":ro" it may carry.
func mountPath(entry string) string {
	return strings.TrimSuffix(strings.TrimSpace(entry), ":ro")
}

// worktreeProjectName is what to call a worktree sandbox after: the project
// sb belongs to, or — for a sandbox made outside any project — the directory
// its workspace sits in, which is the closest thing it has to one.
func (s *Server) worktreeProjectName(sb *manager.Sandbox) string {
	if sb.ProjectID != "" {
		if p, err := s.mgr.GetProject(sb.ProjectID); err == nil {
			return p.Name
		}
	}
	return filepath.Base(strings.TrimSuffix(sb.Workspace, string(filepath.Separator)))
}
