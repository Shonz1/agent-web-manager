// Package web serves the management UI and its JSON/WebSocket API.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/oleksiiipatov/agent-web-manager/internal/git"
	"github.com/oleksiiipatov/agent-web-manager/internal/manager"
	"github.com/oleksiiipatov/agent-web-manager/internal/notify"
	"github.com/oleksiiipatov/agent-web-manager/internal/sbx"
)

// createTimeout matches the manager's own budget for "sbx create", which can
// have to pull an agent image before it returns.
const createTimeout = 10 * time.Minute

// Server wires the sandbox manager to HTTP handlers.
type Server struct {
	mgr      *manager.Manager
	client   *sbx.Client
	notifier *notify.Service
	git      *git.Client
	static   fs.FS

	// shutdown is closed when the process is going away, to end the streams
	// that would otherwise hold it open. http.Server.Shutdown waits for
	// handlers to return and does not cancel the request contexts they are
	// watching, so a handler that only ever ends when the browser goes first
	// would sit there until the shutdown deadline ran out.
	closeOnce sync.Once
	shutdown  chan struct{}
}

func NewServer(mgr *manager.Manager, client *sbx.Client, notifier *notify.Service, gitClient *git.Client, static fs.FS) *Server {
	return &Server{
		mgr:      mgr,
		client:   client,
		notifier: notifier,
		git:      gitClient,
		static:   static,
		shutdown: make(chan struct{}),
	}
}

// Shutdown releases the handlers that stream indefinitely. Register it with
// http.Server.RegisterOnShutdown, which runs it as the shutdown begins rather
// than after the wait it is meant to cut short.
func (s *Server) Shutdown() {
	s.closeOnce.Do(func() { close(s.shutdown) })
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/agents", s.handleAgents)
	mux.HandleFunc("GET /api/fs/dirs", s.handleBrowse)
	mux.HandleFunc("GET /api/events", s.handleEvents)

	mux.HandleFunc("GET /api/settings/telegram", s.handleGetTelegramSettings)
	mux.HandleFunc("PUT /api/settings/telegram", s.handlePutTelegramSettings)
	mux.HandleFunc("DELETE /api/settings/telegram", s.handleDeleteTelegramSettings)
	mux.HandleFunc("POST /api/settings/telegram/test", s.handleTestTelegram)

	// Projects are the primary way of working: a session is started by
	// naming one rather than a sandbox, which is made or reused underneath.
	mux.HandleFunc("GET /api/projects", s.handleListProjects)
	mux.HandleFunc("POST /api/projects", s.handleCreateProject)
	mux.HandleFunc("GET /api/projects/{id}", s.handleGetProject)
	mux.HandleFunc("DELETE /api/projects/{id}", s.handleDeleteProject)
	mux.HandleFunc("POST /api/projects/{id}/sessions", s.handleStartProjectSession)

	// Sandboxes stay reachable directly too: a project's own sandboxes are
	// listed and managed through these once a session has made one.
	mux.HandleFunc("GET /api/sandboxes", s.handleListSandboxes)
	mux.HandleFunc("GET /api/sandboxes/{id}", s.handleGetSandbox)
	mux.HandleFunc("DELETE /api/sandboxes/{id}", s.handleDeleteSandbox)
	mux.HandleFunc("POST /api/sandboxes/{id}/stop", s.handleStopSandbox)
	mux.HandleFunc("POST /api/sandboxes/{id}/sessions", s.handleStartSession)
	mux.HandleFunc("POST /api/sandboxes/{id}/worktree", s.handleStartWorktreeSession)
	mux.HandleFunc("GET /api/sandboxes/{id}/diff", s.handleDiff)
	mux.HandleFunc("GET /api/sandboxes/{id}/diff/file", s.handleDiffFile)

	mux.HandleFunc("GET /api/sessions/{sid}", s.handleGetSession)
	mux.HandleFunc("DELETE /api/sessions/{sid}", s.handleCloseSession)
	mux.HandleFunc("POST /api/sessions/{sid}/interrupt", s.handleInterruptSession)
	mux.HandleFunc("POST /api/sessions/{sid}/restart", s.handleRestartSession)
	mux.HandleFunc("GET /api/sessions/{sid}/attach", s.handleAttach)

	mux.Handle("GET /", s.handleUI())

	return logRequests(mux)
}

// handleUI serves the embedded UI. The selection lives in the URL
// (/sandboxes/{id}, /sessions/{id}), so any path that is not a file in the
// bundle is answered with the index page and resolved by the client — a
// reload, or a pasted link, lands on the same view instead of a 404.
func (s *Server) handleUI() http.Handler {
	files := http.FileServer(http.FS(s.static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hasStatic(s.static, r.URL.Path) {
			r = r.Clone(r.Context())
			r.URL.Path = "/"
			r.URL.RawPath = ""
		}
		files.ServeHTTP(w, r)
	})
}

func hasStatic(fsys fs.FS, urlPath string) bool {
	name := strings.TrimPrefix(path.Clean(urlPath), "/")
	if name == "" || name == "." {
		return true // the index itself
	}
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	resp := map[string]any{"ok": true}
	if err := s.client.Available(ctx); err != nil {
		resp["ok"] = false
		resp["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"agents": sbx.Agents})
}

// --- sandboxes ---

func (s *Server) handleListSandboxes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": s.mgr.ListSandboxes(ctx)})
}

func (s *Server) handleGetSandbox(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	view, err := s.mgr.SandboxView(ctx, r.PathValue("id"))
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleStopSandbox(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	if err := s.mgr.StopSandbox(ctx, r.PathValue("id")); err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	view, err := s.mgr.SandboxView(ctx, r.PathValue("id"))
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handleDeleteSandbox destroys a sandbox and, when it was made for a
// worktree, the checkout underneath it — the same cleanup deleting a whole
// project does, because a worktree sandbox and its directory were made by one
// action and there is nothing left to use the directory for once the sandbox
// is gone.
func (s *Server) handleDeleteSandbox(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// Read before deleting: what has to be cleaned up on the host is only on
	// the record this is about to remove.
	sb, err := s.mgr.GetSandbox(r.PathValue("id"))
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	worktree := sb.IsWorktree
	path, repoRoot := sb.Workspace, sb.RepoRoot

	if err := s.mgr.DeleteSandbox(ctx, sb.ID); err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	if worktree {
		s.removeWorktree(git.Worktree{Path: path, RepoRoot: repoRoot})
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- sessions ---

func (s *Server) handleStartSession(w http.ResponseWriter, r *http.Request) {
	var req manager.StartSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	// Starting a session in a sandbox sbx has lost recreates it, so this
	// inherits the create budget rather than the ordinary one.
	ctx, cancel := context.WithTimeout(context.Background(), createTimeout)
	defer cancel()

	sess, err := s.mgr.StartSession(ctx, r.PathValue("id"), req)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, sess.View())
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.mgr.GetSession(r.PathValue("sid"))
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, sess.View())
}

func (s *Server) handleCloseSession(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.CloseSession(r.PathValue("sid")); err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRestartSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Cols uint16 `json:"cols"`
		Rows uint16 `json:"rows"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body)

	ctx, cancel := context.WithTimeout(context.Background(), createTimeout)
	defer cancel()

	sess, err := s.mgr.RestartSession(ctx, r.PathValue("sid"), body.Cols, body.Rows)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, sess.View())
}

func (s *Server) handleInterruptSession(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.InterruptSession(r.PathValue("sid")); err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, manager.ErrSandboxNotFound), errors.Is(err, manager.ErrSessionNotFound),
		errors.Is(err, manager.ErrProjectNotFound):
		return http.StatusNotFound
	case errors.Is(err, manager.ErrExists):
		return http.StatusConflict
	case errors.Is(err, manager.ErrBaseSandbox):
		// Not a request that arrived wrong: this one is understood and
		// refused, which is what the UI greys the base sandbox's buttons out
		// to say before it is ever sent.
		return http.StatusForbidden
	default:
		return http.StatusBadRequest
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}

// sameOrigin guards the WebSocket upgrade against cross-site connections from
// a page the user happens to have open; the manager can drive agents with
// full workspace access, so a drive-by connect must not be possible.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // non-browser client
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	originHost, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		originHost = u.Host
	}
	reqHost, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		reqHost = r.Host
	}
	return strings.EqualFold(u.Host, r.Host) || strings.EqualFold(originHost, reqHost)
}
