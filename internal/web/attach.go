package web

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/oleksiiipatov/agent-web-manager/internal/manager"
)

const (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = 25 * time.Second
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 32 * 1024,
	CheckOrigin:     sameOrigin,
}

// clientMsg is what the browser sends over the terminal socket.
type clientMsg struct {
	Type string `json:"type"` // "input" | "resize"
	Data string `json:"data"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// handleAttach streams a session's PTY over a WebSocket. Output is sent as
// binary frames; status changes as text (JSON) frames.
func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request) {
	sess, err := s.mgr.GetSession(r.PathValue("sid"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already wrote a response
	}
	defer conn.Close()

	scrollback, out, unsubscribe := sess.Subscribe()
	defer unsubscribe()

	changed, unwatch := sess.Watch()
	defer unwatch()

	// A single writer goroutine owns the connection, as gorilla requires.
	// `writes` is never closed — several goroutines feed it, and they stop
	// when `done` closes.
	writes := make(chan wsFrame, 256)
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.writeLoop(conn, writes, quit)
	}()

	send := func(f wsFrame) bool {
		select {
		case writes <- f:
			return true
		case <-done:
			return false
		}
	}

	send(statusFrame(sess))
	if len(scrollback) > 0 {
		send(wsFrame{kind: websocket.BinaryMessage, data: scrollback})
	}

	// Relay PTY output, view changes, and session-exit notifications.
	go func() {
		exited := sess.Done()
		for {
			select {
			case chunk, ok := <-out:
				if !ok {
					send(statusFrame(sess))
					return
				}
				if !send(wsFrame{kind: websocket.BinaryMessage, data: chunk}) {
					return
				}
			case <-changed:
				// What the session is called has moved — an agent titled its
				// conversation, or a shell was given a command. The status
				// frame carries the whole view, so the browser can redraw
				// without waiting for its next poll.
				if !send(statusFrame(sess)) {
					return
				}
			case <-exited:
				exited = nil // report once, then keep draining `out`
				send(statusFrame(sess))
			case <-done:
				return
			}
		}
	}()

	conn.SetReadLimit(1 << 20)
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if mt != websocket.TextMessage {
			continue
		}
		var msg clientMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "input":
			if err := sess.Write([]byte(msg.Data)); err != nil {
				send(errorFrame(err.Error()))
			}
		case "resize":
			if err := sess.Resize(msg.Cols, msg.Rows); err != nil {
				log.Printf("resize session %s: %v", sess.ID, err)
			}
		}
	}
	close(quit)
	<-done
}

type wsFrame struct {
	kind int
	data []byte
}

func (s *Server) writeLoop(conn *websocket.Conn, frames <-chan wsFrame, quit <-chan struct{}) {
	ping := time.NewTicker(pingPeriod)
	defer ping.Stop()

	for {
		select {
		case <-quit:
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			_ = conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			return
		case f := <-frames:
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(f.kind, f.data); err != nil {
				return
			}
		case <-ping.C:
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func statusFrame(sess *manager.Session) wsFrame {
	payload, _ := json.Marshal(map[string]any{
		"type":    "status",
		"session": sess.View(),
	})
	return wsFrame{kind: websocket.TextMessage, data: payload}
}

func errorFrame(msg string) wsFrame {
	payload, _ := json.Marshal(map[string]any{"type": "error", "message": msg})
	return wsFrame{kind: websocket.TextMessage, data: payload}
}
