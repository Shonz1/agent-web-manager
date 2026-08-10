package notify

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Shonz1/agent-web-manager/internal/manager"
)

// newTestService builds a service over an empty state directory with the
// environment cleared, which is the state a fresh install is in.
func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	t.Setenv(EnvToken, "")
	t.Setenv(EnvChatID, "")

	dir := t.TempDir()
	s, err := NewService(dir)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return s, dir
}

func savedConfig(t *testing.T, dir string) config {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, telegramFile))
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse saved config: %v", err)
	}
	return cfg
}

func TestNotificationsStartOff(t *testing.T) {
	s, _ := newTestService(t)

	if got := s.Settings(); got.Enabled || got.FromEnv || got.ChatID != "" {
		t.Fatalf("a fresh install is already configured: %+v", got)
	}
}

func TestConfigureSavesAndTakesEffect(t *testing.T) {
	got := serve(t, `{"ok":true,"result":{"username":"my_agents_bot"}}`)
	s, dir := newTestService(t)

	settings, err := s.Configure(context.Background(), "abc:123", "42", "")
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if !settings.Enabled || settings.ChatID != "42" || settings.Bot != "my_agents_bot" {
		t.Fatalf("settings do not describe the bot that was set: %+v", settings)
	}

	// The token is checked and then used, so the person sees a message
	// confirming it rather than having to go and test it.
	<-got // getMe
	confirmation := <-got
	if text, _ := confirmation["text"].(string); !strings.Contains(text, "connected") {
		t.Fatalf("no confirmation was sent, got %q", text)
	}

	if saved := savedConfig(t, dir); saved.Token != "abc:123" || saved.ChatID != "42" {
		t.Fatalf("saved %+v, want the bot that was set", saved)
	}
	// Reloading is what the next start does, and it must come back on.
	reloaded, err := NewService(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.Settings().Enabled {
		t.Fatal("a saved bot did not survive a reload")
	}
}

// Credentials are a bearer token for the whole bot; the file they land in must
// not be readable by other accounts on the machine.
func TestSavedCredentialsArePrivate(t *testing.T) {
	serve(t, `{"ok":true,"result":{"username":"bot"}}`)
	s, dir := newTestService(t)

	if _, err := s.Configure(context.Background(), "abc:123", "42", ""); err != nil {
		t.Fatalf("configure: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, telegramFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("permissions %o, want 600", perm)
	}
}

// A configuration that does not work must not be saved, or the next start
// would come back up broken and silent.
func TestConfigureRejectsCredentialsTelegramRefuses(t *testing.T) {
	serve(t, `{"ok":false,"description":"Unauthorized"}`)
	s, dir := newTestService(t)

	if _, err := s.Configure(context.Background(), "wrong", "42", ""); err == nil {
		t.Fatal("bad credentials were accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, telegramFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("bad credentials were written to disk")
	}
	if s.Settings().Enabled {
		t.Fatal("bad credentials were switched on")
	}
}

// Whatever comes back from a failed save is shown to whoever typed the
// credentials, and may well be logged on the way past. Go puts the request URL
// — which carries the token — into transport errors, so the one place a token
// could escape is an error nobody thought about.
func TestConfigureErrorsDoNotCarryTheToken(t *testing.T) {
	previous := baseURL
	baseURL = "http://127.0.0.1:1" // nothing listening, so the transport fails
	defer func() { baseURL = previous }()

	s, _ := newTestService(t)
	const token = "123456:super-secret-token"

	_, err := s.Configure(context.Background(), token, "42", "")
	if err == nil {
		t.Fatal("expected the save to fail")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("the token leaked into the error: %v", err)
	}
}

func TestConfigureNeedsBothHalves(t *testing.T) {
	s, _ := newTestService(t)

	if _, err := s.Configure(context.Background(), "abc:123", "", ""); err == nil {
		t.Fatal("a token with no chat id was accepted")
	}
	if _, err := s.Configure(context.Background(), "", "42", ""); err == nil {
		t.Fatal("a chat id with no token was accepted")
	}
}

func TestDisableForgetsTheCredentials(t *testing.T) {
	serve(t, `{"ok":true,"result":{"username":"bot"}}`)
	s, dir := newTestService(t)

	if _, err := s.Configure(context.Background(), "abc:123", "42", ""); err != nil {
		t.Fatalf("configure: %v", err)
	}
	settings, err := s.Disable()
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if settings.Enabled || settings.ChatID != "" {
		t.Fatalf("still configured after disable: %+v", settings)
	}
	if _, err := os.Stat(filepath.Join(dir, telegramFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the credentials were left on disk")
	}
	// And it must not come back at the next start.
	reloaded, err := NewService(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Settings().Enabled {
		t.Fatal("a disabled bot came back after a reload")
	}
}

func TestDisableOnAFreshInstallIsNotAnError(t *testing.T) {
	s, _ := newTestService(t)
	if _, err := s.Disable(); err != nil {
		t.Fatalf("disable with nothing configured: %v", err)
	}
}

// The environment is for a deployment that would rather not have a credential
// written by a web page, so the page must not write one — and must be told
// why rather than appearing to work.
func TestTheEnvironmentIsReadOnly(t *testing.T) {
	t.Setenv(EnvToken, "from-env")
	t.Setenv(EnvChatID, "999")

	dir := t.TempDir()
	s, err := NewService(dir)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	settings := s.Settings()
	if !settings.Enabled || !settings.FromEnv || settings.ChatID != "999" {
		t.Fatalf("environment configuration was not picked up: %+v", settings)
	}

	if _, err := s.Configure(context.Background(), "abc:123", "42", ""); !errors.Is(err, ErrFromEnv) {
		t.Fatalf("configure error is %v, want ErrFromEnv", err)
	}
	if _, err := s.Disable(); !errors.Is(err, ErrFromEnv) {
		t.Fatalf("disable error is %v, want ErrFromEnv", err)
	}
	if _, err := os.Stat(filepath.Join(dir, telegramFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a file was written for a bot the environment owns")
	}
}

func TestHalfAnEnvironmentIsAnError(t *testing.T) {
	t.Setenv(EnvToken, "from-env")
	t.Setenv(EnvChatID, "")

	if _, err := NewService(t.TempDir()); err == nil {
		t.Fatal("a token with no chat id was accepted")
	}
}

func TestNewServiceRejectsAnUnreadableFile(t *testing.T) {
	t.Setenv(EnvToken, "")
	t.Setenv(EnvChatID, "")

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, telegramFile), []byte(`{"token": not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(dir); err == nil {
		t.Fatal("a malformed config file was accepted")
	}
}

func TestTestSendsWithoutConfiguredCredentials(t *testing.T) {
	s, _ := newTestService(t)
	if err := s.Test(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("error is %v, want ErrNotConfigured", err)
	}
}

// --- relay ---

func TestRelayPostsEveryEvent(t *testing.T) {
	got := serve(t, `{"ok":true,"result":{"username":"bot"}}`)
	s, _ := newTestService(t)
	if _, err := s.Configure(context.Background(), "abc:123", "42", ""); err != nil {
		t.Fatalf("configure: %v", err)
	}
	<-got // getMe
	<-got // the confirmation Configure sends

	events := make(chan manager.Event, 2)
	events <- manager.Event{Kind: manager.EventAttention, Title: "claude", SandboxName: "api"}
	events <- manager.Event{Kind: manager.EventDone, Title: "codex", SandboxName: "web"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Relay(ctx, events)

	for _, want := range []string{"needs you", "finished"} {
		select {
		case payload := <-got:
			if text, _ := payload["text"].(string); !strings.Contains(text, want) {
				t.Fatalf("message %q does not contain %q", text, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no message containing %q arrived", want)
		}
	}
}

// The relay outlives any particular configuration, so switching the bot off
// has to stop the next notification rather than the one after a restart.
func TestRelayStopsSendingOnceDisabled(t *testing.T) {
	got := serve(t, `{"ok":true,"result":{"username":"bot"}}`)
	s, _ := newTestService(t)
	if _, err := s.Configure(context.Background(), "abc:123", "42", ""); err != nil {
		t.Fatalf("configure: %v", err)
	}
	<-got // getMe
	<-got // confirmation

	if _, err := s.Disable(); err != nil {
		t.Fatalf("disable: %v", err)
	}

	events := make(chan manager.Event, 1)
	events <- manager.Event{Kind: manager.EventDone, Title: "codex", SandboxName: "web"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Relay(ctx, events)

	select {
	case payload := <-got:
		t.Fatalf("a disabled bot was still sent to: %v", payload["text"])
	case <-time.After(500 * time.Millisecond):
	}
}

// One send that fails must not take the relay down with it — the next event is
// the one that matters, and there will always be a next event.
func TestRelaySurvivesAFailedSend(t *testing.T) {
	got := serve(t, `{"ok":false,"description":"Bad Request: chat not found"}`)
	s, _ := newTestService(t)
	// Set directly rather than through Configure, which would refuse
	// credentials this server rejects. What is under test is a bot that was
	// working when it was saved and has stopped working since — someone
	// deleted the chat, or blocked the bot.
	s.set("abc:123", "42", "")

	events := make(chan manager.Event, 2)
	events <- manager.Event{Kind: manager.EventDone, Title: "first", SandboxName: "api"}
	events <- manager.Event{Kind: manager.EventDone, Title: "second", SandboxName: "api"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Relay(ctx, events)

	<-got
	select {
	case payload := <-got:
		if text, _ := payload["text"].(string); !strings.Contains(text, "second") {
			t.Fatalf("second message is %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the relay stopped after a failed send")
	}
}

// --- the link address ---

func TestConfigureSavesTheLinkAddress(t *testing.T) {
	got := serve(t, `{"ok":true,"result":{"username":"bot"}}`)
	s, dir := newTestService(t)

	settings, err := s.Configure(context.Background(), "abc:123", "42", "http://192.168.1.50:7788")
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if settings.LinkBase != "http://192.168.1.50:7788" {
		t.Fatalf("settings link %q", settings.LinkBase)
	}
	if saved := savedConfig(t, dir); saved.LinkBase != "http://192.168.1.50:7788" {
		t.Fatalf("saved link %q", saved.LinkBase)
	}

	// The confirmation carries the button, so an address Telegram will not
	// accept fails here rather than silently later.
	<-got // getMe
	confirmation := <-got
	if confirmation["reply_markup"] == nil {
		t.Fatal("the confirmation had no button to prove the address works")
	}
}

func TestConfigureRejectsAnUnusableLinkAddress(t *testing.T) {
	s, dir := newTestService(t)

	if _, err := s.Configure(context.Background(), "abc:123", "42", "192.168.1.50:7788"); err == nil {
		t.Fatal("an address with no scheme was accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, telegramFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an unusable address was written to disk")
	}
}

// Correcting the address is the commonest reason to come back to this form —
// set up on the laptop at localhost, then pointed at something the phone can
// reach. The token is never sent to the page, so requiring it again would mean
// going to find it.
func TestTheLinkCanBeChangedWithoutTheTokenAgain(t *testing.T) {
	serve(t, `{"ok":true,"result":{"username":"bot"}}`)
	s, dir := newTestService(t)

	if _, err := s.Configure(context.Background(), "abc:123", "42", "http://127.0.0.1:7788"); err != nil {
		t.Fatalf("configure: %v", err)
	}

	settings, err := s.Configure(context.Background(), "", "42", "http://192.168.1.50:7788")
	if err != nil {
		t.Fatalf("changing only the address: %v", err)
	}
	if settings.LinkBase != "http://192.168.1.50:7788" {
		t.Fatalf("address did not change: %+v", settings)
	}
	if saved := savedConfig(t, dir); saved.Token != "abc:123" {
		t.Fatalf("the stored token was lost: %q", saved.Token)
	}
}

func TestEventsCarryTheButtonThroughTheRelay(t *testing.T) {
	got := serve(t, `{"ok":true,"result":{"username":"bot"}}`)
	s, _ := newTestService(t)
	if _, err := s.Configure(context.Background(), "abc:123", "42", "http://box.local:7788"); err != nil {
		t.Fatalf("configure: %v", err)
	}
	<-got // getMe
	<-got // confirmation

	events := make(chan manager.Event, 1)
	events <- manager.Event{Kind: manager.EventAttention, SessionID: "s9", Title: "claude"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Relay(ctx, events)

	select {
	case payload := <-got:
		markup, _ := payload["reply_markup"].(map[string]any)
		if markup == nil {
			t.Fatalf("notification had no button: %v", payload)
		}
		rows, _ := markup["inline_keyboard"].([]any)
		row, _ := rows[0].([]any)
		button, _ := row[0].(map[string]any)
		if button["url"] != "http://box.local:7788/sessions/s9" {
			t.Fatalf("button goes to %v", button["url"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notification arrived")
	}
}
