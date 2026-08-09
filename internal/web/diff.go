package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/oleksiiipatov/agent-web-manager/internal/git"
)

// diffTimeout bounds one git invocation. A cold repository on a network mount
// is slow the first time and instant afterwards; anything past this is wedged.
const diffTimeout = 30 * time.Second

// changesResponse answers with what the workspace is rather than an error when
// there is no diff to show. A directory that is not a checkout is an ordinary
// thing for a workspace to be, and the UI has something to say about it — it
// is not a request that failed.
type changesResponse struct {
	Repo      bool         `json:"repo"`
	Workspace string       `json:"workspace"`
	Message   string       `json:"message,omitempty"`
	Changes   *git.Changes `json:"changes,omitempty"`
}

// handleDiff lists what has changed in a sandbox's workspace.
//
// The workspace is read on the host rather than through the sandbox: it is the
// same bind-mounted directory either way, and reading it here works whether or
// not the container is running.
func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, fmt.Errorf("cross-origin request rejected"))
		return
	}
	sb, err := s.mgr.GetSandbox(r.PathValue("id"))
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}

	resp := changesResponse{Workspace: sb.Workspace}
	if sb.Workspace == "" {
		resp.Message = "This sandbox has no workspace mounted, so there is nothing to compare."
		writeJSON(w, http.StatusOK, resp)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), diffTimeout)
	defer cancel()

	changes, err := s.gitClient().Changes(ctx, sb.Workspace, git.ParseBase(r.URL.Query().Get("base")))
	if errors.Is(err, git.ErrNotRepo) {
		resp.Message = "This workspace is not a git checkout, so there is nothing to compare."
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	resp.Repo = true
	resp.Changes = &changes
	writeJSON(w, http.StatusOK, resp)
}

// handleDiffFile returns one file's diff, parsed into hunks of numbered lines.
func (s *Server) handleDiffFile(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, fmt.Errorf("cross-origin request rejected"))
		return
	}
	sb, err := s.mgr.GetSandbox(r.PathValue("id"))
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	if sb.Workspace == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("this sandbox has no workspace mounted"))
		return
	}

	query := r.URL.Query()
	path := query.Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("path is required"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), diffTimeout)
	defer cancel()

	diff, err := s.gitClient().FileDiff(ctx, sb.Workspace, git.ParseBase(query.Get("base")), path, query.Get("old"))
	switch {
	case errors.Is(err, git.ErrNotRepo), errors.Is(err, git.ErrNoSuchFile):
		writeError(w, http.StatusNotFound, err)
	case err != nil:
		writeError(w, http.StatusBadRequest, err)
	default:
		writeJSON(w, http.StatusOK, diff)
	}
}

// gitClient returns the configured git client, or one using "git" from PATH
// for a Server assembled without one.
func (s *Server) gitClient() *git.Client {
	if s.git == nil {
		return git.New("")
	}
	return s.git
}
