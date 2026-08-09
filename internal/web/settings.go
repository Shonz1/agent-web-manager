package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/oleksiiipatov/agent-web-manager/internal/notify"
)

// telegramTimeout covers a call to Telegram made while a browser waits on the
// response, which is every one of these: saving checks the credentials by
// using them.
const telegramTimeout = 20 * time.Second

// The settings endpoints accept a bearer credential and act on it, so unlike
// the rest of the API they check the origin. A page on another site cannot
// read the response either way, but it could otherwise point this manager's
// notifications at a chat of its own choosing.
func (s *Server) handleGetTelegramSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.notifier.Settings())
}

func (s *Server) handlePutTelegramSettings(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, errors.New("cross-origin request"))
		return
	}

	var req struct {
		Token    string `json:"token"`
		ChatID   string `json:"chatId"`
		LinkBase string `json:"linkBase"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	// Detached from the request: the browser giving up must not leave the
	// credentials half-applied, having sent the confirmation but not saved.
	ctx, cancel := context.WithTimeout(context.Background(), telegramTimeout)
	defer cancel()

	settings, err := s.notifier.Configure(ctx, req.Token, req.ChatID, req.LinkBase)
	if err != nil {
		writeError(w, settingsStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) handleDeleteTelegramSettings(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, errors.New("cross-origin request"))
		return
	}
	settings, err := s.notifier.Disable()
	if err != nil {
		writeError(w, settingsStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) handleTestTelegram(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, errors.New("cross-origin request"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), telegramTimeout)
	defer cancel()

	if err := s.notifier.Test(ctx); err != nil {
		writeError(w, settingsStatus(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func settingsStatus(err error) int {
	switch {
	case errors.Is(err, notify.ErrFromEnv):
		return http.StatusConflict
	case errors.Is(err, notify.ErrNotConfigured):
		return http.StatusNotFound
	default:
		// Everything else is either credentials the person got wrong or a
		// Telegram that would not take them, and both are their input.
		return http.StatusBadRequest
	}
}
