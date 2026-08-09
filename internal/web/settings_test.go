package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oleksiiipatov/agent-web-manager/internal/manager"
	"github.com/oleksiiipatov/agent-web-manager/internal/notify"
	"github.com/oleksiiipatov/agent-web-manager/internal/sbx"
)

// testServer builds a server over empty state with no notifications
// configured, which is where a fresh install starts.
func testServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv(notify.EnvToken, "")
	t.Setenv(notify.EnvChatID, "")

	dir := t.TempDir()
	mgr, err := manager.New(sbx.New("sbx"), dir)
	if err != nil {
		t.Fatal(err)
	}
	notifier, err := notify.NewService(dir)
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(mgr, sbx.New("sbx"), notifier, StaticFS())
}

func TestTelegramSettingsStartEmpty(t *testing.T) {
	rec := httptest.NewRecorder()
	testServer(t).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/settings/telegram", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	var got notify.Settings
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Enabled {
		t.Fatalf("a fresh install reports notifications on: %+v", got)
	}
}

// The token is a bearer credential for the whole bot, and the page never needs
// it back — so there must be nowhere in this response for one to appear. The
// other half of this, that a failure explaining bad credentials does not quote
// them, is tested in the notify package where the API endpoint can be pointed
// somewhere other than Telegram.
func TestTelegramSettingsCannotCarryAToken(t *testing.T) {
	rec := httptest.NewRecorder()
	testServer(t).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/settings/telegram", nil))

	var fields map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
		t.Fatalf("parse: %v", err)
	}
	for name := range fields {
		if strings.Contains(strings.ToLower(name), "token") {
			t.Fatalf("settings expose a %q field", name)
		}
	}
}

func TestTelegramSettingsRejectHalfAConfiguration(t *testing.T) {
	body := strings.NewReader(`{"token":"abc:123","chatId":""}`)
	req := httptest.NewRequest(http.MethodPut, "/api/settings/telegram", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	testServer(t).Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "chat id") {
		t.Fatalf("error does not say what is missing: %s", rec.Body.String())
	}
}

// These endpoints take a credential and act on it, so unlike the rest of the
// API they refuse a request from a page on another site.
func TestTelegramSettingsRejectCrossOrigin(t *testing.T) {
	for _, tc := range []struct {
		method string
		path   string
		body   io.Reader
	}{
		{http.MethodPut, "/api/settings/telegram", strings.NewReader(`{"token":"a","chatId":"1"}`)},
		{http.MethodDelete, "/api/settings/telegram", nil},
		{http.MethodPost, "/api/settings/telegram/test", nil},
	} {
		req := httptest.NewRequest(tc.method, tc.path, tc.body)
		req.Header.Set("Origin", "https://elsewhere.example")
		rec := httptest.NewRecorder()

		testServer(t).Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s: status %d, want 403", tc.method, tc.path, rec.Code)
		}
	}
}

func TestTestingAnUnconfiguredBotSaysSo(t *testing.T) {
	rec := httptest.NewRecorder()
	testServer(t).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/settings/telegram/test", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}

// A deployment that sets the environment does not want the page writing
// credentials to disk, and the page has to be told rather than appearing to
// work.
func TestSettingsAreReadOnlyWhenTheEnvironmentOwnsThem(t *testing.T) {
	t.Setenv(notify.EnvToken, "from-env")
	t.Setenv(notify.EnvChatID, "999")

	dir := t.TempDir()
	mgr, err := manager.New(sbx.New("sbx"), dir)
	if err != nil {
		t.Fatal(err)
	}
	notifier, err := notify.NewService(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(mgr, sbx.New("sbx"), notifier, StaticFS())

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/settings/telegram", nil))
	var got notify.Settings
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !got.Enabled || !got.FromEnv {
		t.Fatalf("environment configuration was not reported: %+v", got)
	}

	body := strings.NewReader(`{"token":"abc:123","chatId":"42"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/settings/telegram", body)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409", rec.Code)
	}
}
