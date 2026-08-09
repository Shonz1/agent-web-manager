package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oleksiiipatov/agent-web-manager/internal/manager"
)

func TestWriteEventFramesTheKindAndPayload(t *testing.T) {
	var out strings.Builder
	ev := manager.Event{
		Kind:        manager.EventAttention,
		SessionID:   "s1",
		SandboxName: "my-app",
		Title:       "claude",
		Detail:      "Add a rate limiter",
	}

	if err := writeEvent(&out, ev); err != nil {
		t.Fatalf("write: %v", err)
	}
	frame := out.String()

	if !strings.HasPrefix(frame, "event: attention\ndata: ") {
		t.Fatalf("frame does not name its kind: %q", frame)
	}
	// A frame that is not terminated by a blank line is one the browser holds
	// on to, waiting for the rest of it.
	if !strings.HasSuffix(frame, "\n\n") {
		t.Fatalf("frame is not terminated: %q", frame)
	}
	// A newline anywhere in the payload would split it across two data fields
	// and change what the browser parses.
	data := strings.TrimSuffix(strings.SplitN(frame, "data: ", 2)[1], "\n\n")
	if strings.Contains(data, "\n") {
		t.Fatalf("payload spans lines: %q", data)
	}

	var got manager.Event
	if err := json.Unmarshal([]byte(data), &got); err != nil {
		t.Fatalf("payload is not json: %v", err)
	}
	if got.SessionID != "s1" || got.Title != "claude" || got.Detail != "Add a rate limiter" {
		t.Fatalf("payload lost something: %+v", got)
	}
}

func eventServer(t *testing.T) *Server {
	t.Helper()
	return testServer(t)
}

func TestHandleEventsOpensAStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(eventServer(t).handleEvents))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q, want text/event-stream", ct)
	}
	// The headers arriving at all is the point: they are only readable here
	// because the handler flushed them without waiting for a first event,
	// which is what lets the browser call the connection open.
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control %q, want no-store", cc)
	}

	// Closing the request context is what the handler watches to know the
	// browser has gone; a stream that ignored it would leak a subscription.
	cancel()
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err == nil {
		t.Fatal("the stream outlived the request")
	}
}

// Every open tab holds a stream permanently, and http.Server.Shutdown waits
// for handlers to return without cancelling the contexts they watch — so a
// stream that only ended when the browser did would hold up every exit.
func TestHandleEventsEndsOnShutdown(t *testing.T) {
	srv := eventServer(t)
	rec := httptest.NewRecorder()

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		srv.handleEvents(rec, httptest.NewRequest(http.MethodGet, "/api/events", nil))
	}()

	// Let the handler reach its loop before the signal, so this tests the
	// select rather than a channel that was already closed on the way in.
	time.Sleep(100 * time.Millisecond)
	srv.Shutdown()

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream outlived the shutdown")
	}

	// Registered with RegisterOnShutdown, which Go may call more than once.
	srv.Shutdown()
}

// The stream carries what every agent in every sandbox is doing, so a page on
// another origin must not be able to open one. EventSource enforces this on
// its own; this is the belt to that pair of braces.
func TestHandleEventsRejectsCrossOrigin(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	req.Header.Set("Origin", "https://elsewhere.example")

	eventServer(t).handleEvents(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
}
