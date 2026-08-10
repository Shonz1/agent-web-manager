package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Shonz1/agent-web-manager/internal/git"
	"github.com/Shonz1/agent-web-manager/internal/manager"
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
// A bind-mounted workspace is read on the host: it is the same directory
// either way, and reading it here works whether or not the container is
// running. A clone sandbox's workspace exists only inside the container, so
// that one is read through sbx; see gitFor.
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

	if why := s.unreadable(ctx, sb); why != "" {
		resp.Message = why
		writeJSON(w, http.StatusOK, resp)
		return
	}

	changes, err := s.gitFor(sb).Changes(ctx, sb.Workspace, git.ParseBase(r.URL.Query().Get("base")))
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

	if why := s.unreadable(ctx, sb); why != "" {
		writeError(w, http.StatusConflict, errors.New(why))
		return
	}

	diff, err := s.gitFor(sb).FileDiff(ctx, sb.Workspace, git.ParseBase(query.Get("base")), path, query.Get("old"))
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

// sbxRunning is the status sbx reports for a sandbox whose container is up.
const sbxRunning = "running"

// unreadable says why this sandbox's checkout cannot be read right now, or ""
// when it can be.
//
// A workspace mounted from the host is read on the host, whatever the
// container is doing. A clone sandbox's is inside the container, and reading
// it means "sbx exec" — which starts the container. The Changes view polls
// every few seconds, so a sandbox someone deliberately stopped would be woken
// by leaving that view open, and held awake for as long as it stayed open.
// Better to say what the view cannot show.
func (s *Server) unreadable(ctx context.Context, sb *manager.Sandbox) string {
	if !sb.Clone {
		return ""
	}
	view, err := s.mgr.SandboxView(ctx, sb.ID)
	if err != nil || view.Status == sbxRunning {
		// Whatever is wrong with a sandbox that cannot even be listed, git
		// running in it will say it better than this can.
		return ""
	}
	return fmt.Sprintf("This session works in a git clone inside %s, which is %s. "+
		"Start the sandbox to see what has changed in it.", sb.Name, view.Status)
}

// gitFor returns the client that can see this sandbox's checkout: the plain
// one for a workspace mounted from the host, and one running inside the
// container for a clone sandbox, whose checkout was made in there and is on
// the host nowhere at all.
func (s *Server) gitFor(sb *manager.Sandbox) *git.Client {
	if !sb.Clone {
		return s.gitClient()
	}
	return s.gitClient().InSandbox(s.sbxBin(), sb.Name)
}

// sbxBin is the sbx binary this server was configured with.
func (s *Server) sbxBin() string {
	if s.client == nil {
		return ""
	}
	return s.client.Bin
}
