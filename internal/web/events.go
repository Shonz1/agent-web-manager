package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Shonz1/agent-web-manager/internal/manager"
)

// eventPing is how often a comment is written down an otherwise idle stream.
// Notifications can be an hour apart, and something in between — a proxy, a
// phone's radio settling — will drop a connection that says nothing for that
// long. The browser reconnects on its own, but only once it notices.
const eventPing = 25 * time.Second

// handleEvents streams the moments worth notifying someone about as
// server-sent events.
//
// The dwell that decides which those are lives in the manager rather than
// here, so that a browser and a Telegram bot watching the same manager
// announce the same things at the same time.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, errors.New("cross-origin request"))
		return
	}
	rc := http.NewResponseController(w)

	events, unsubscribe := s.mgr.Events()
	defer unsubscribe()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	// A proxy that buffers holds a notification until enough of them have
	// piled up to fill a block, which for this stream is never.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return
	}

	ping := time.NewTicker(eventPing)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case <-s.shutdown:
			// Every open tab holds one of these permanently, so without this
			// the process could not exit until they all timed out.
			return

		case ev := <-events:
			if err := writeEvent(w, ev); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}

		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

// writeEvent frames one event.
//
// The kind goes in the event field as well as the payload, so the browser can
// listen for the kinds it cares about rather than parsing every message to
// find out what it is.
func writeEvent(w io.Writer, ev manager.Event) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		// Nothing in an event is unmarshalable, but a frame with no data
		// would desynchronise the stream, so it is dropped rather than sent.
		return nil
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Kind, payload)
	return err
}
