package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// The model a project's sessions come up on is kept in its base sandbox, and
// both reading and writing it mean running a command in there — which starts
// that sandbox if it is stopped. So it is an endpoint of its own rather than
// another field of the project view: the project list is refreshed every few
// seconds, and this is asked for when somebody opens the thing that shows it.

// modelTimeout covers one of those commands, including the sandbox start it
// may have to wait for.
const modelTimeout = 2 * time.Minute

func (s *Server) handleGetProjectModel(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), modelTimeout)
	defer cancel()

	view, err := s.mgr.ProjectModel(ctx, r.PathValue("id"))
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handlePutProjectModel(w http.ResponseWriter, r *http.Request) {
	// This one changes what every session started from now on will run, from
	// a page that need not be this one otherwise, so it takes the same guard
	// the other settings endpoints do.
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, errors.New("cross-origin request"))
		return
	}

	var req struct {
		// Model is the name to set, or empty to take the setting out and
		// leave the sandbox on its default.
		Model string `json:"model"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	// Detached from the request: a browser that gives up waiting must not
	// cancel the write half way through the settings file it is replacing.
	ctx, cancel := context.WithTimeout(context.Background(), modelTimeout)
	defer cancel()

	view, err := s.mgr.SetProjectModel(ctx, r.PathValue("id"), req.Model)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}
