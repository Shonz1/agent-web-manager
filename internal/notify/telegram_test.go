package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oleksiiipatov/agent-web-manager/internal/manager"
)

// serve stands in for the Telegram API and hands back what it was sent.
//
// It points baseURL at itself for the duration of the test, so anything the
// package builds — including the bots a Service makes for itself while saving
// settings — talks to this rather than to Telegram.
func serve(t *testing.T, reply string) <-chan map[string]any {
	t.Helper()
	got := make(chan map[string]any, 8)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// getMe carries no body at all, so this starts empty rather than
		// coming out of the decode.
		payload := map[string]any{"_path": r.URL.Path}
		if len(body) > 0 {
			_ = json.Unmarshal(body, &payload)
		}
		got <- payload
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)

	previous := baseURL
	baseURL = srv.URL
	t.Cleanup(func() { baseURL = previous })

	return got
}

func TestSendPostsToTheChat(t *testing.T) {
	got := serve(t, `{"ok":true,"result":{}}`)
	tg := newTelegram("secret-token", "42")

	if err := tg.Send(context.Background(), Message{Text: "hello"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	payload := <-got
	if payload["chat_id"] != "42" {
		t.Fatalf("chat_id %v, want 42", payload["chat_id"])
	}
	if payload["text"] != "hello" {
		t.Fatalf("text %v, want hello", payload["text"])
	}
	if path, _ := payload["_path"].(string); !strings.Contains(path, "secret-token") {
		t.Fatalf("token not in the request path: %q", path)
	}
}

// Telegram answers 200 with ok:false for a bad token or a chat the bot cannot
// post to, so the status code alone says nothing.
func TestSendReportsAnAPIRefusal(t *testing.T) {
	serve(t, `{"ok":false,"description":"Forbidden: bot was blocked by the user"}`)
	tg := newTelegram("secret-token", "42")

	err := tg.Send(context.Background(), Message{Text: "hello"})
	if err == nil {
		t.Fatal("a refused send reported success")
	}
	// This reaches the settings page as it is, so it has to read as an
	// explanation rather than as a status code.
	if !strings.Contains(err.Error(), "bot was blocked") {
		t.Fatalf("error does not say why: %v", err)
	}
}

func TestCheckReturnsTheBotName(t *testing.T) {
	serve(t, `{"ok":true,"result":{"username":"my_agents_bot"}}`)
	tg := newTelegram("secret-token", "42")

	name, err := tg.Check(context.Background())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if name != "my_agents_bot" {
		t.Fatalf("username %q, want my_agents_bot", name)
	}
}

// The token is in the request path, and Go puts the request URL into transport
// errors — so an error logged verbatim, or handed to the settings page, would
// be a leaked credential.
func TestTransportErrorsDoNotCarryTheToken(t *testing.T) {
	previous := baseURL
	// Nothing is listening here, so the transport fails and Go builds a
	// *url.Error around the full URL.
	baseURL = "http://127.0.0.1:1"
	defer func() { baseURL = previous }()

	tg := newTelegram("secret-token", "42")
	err := tg.Send(context.Background(), Message{Text: "hello"})
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("the token leaked into the error: %v", err)
	}
	if !strings.Contains(err.Error(), "<token>") {
		t.Fatalf("error was not redacted: %v", err)
	}
}

func TestMessageEscapesWhatCameOffTheTerminal(t *testing.T) {
	msg := EventMessage(manager.Event{
		Kind:        manager.EventAttention,
		Title:       "claude",
		Detail:      "Fix <script> & the parser",
		SandboxName: "my-app",
	}, "").Text

	if strings.Contains(msg, "<script>") {
		t.Fatalf("markup from the terminal was not escaped: %q", msg)
	}
	if !strings.Contains(msg, "&lt;script&gt; &amp; the parser") {
		t.Fatalf("detail is missing or wrongly escaped: %q", msg)
	}
	// The session's own name is the bold part, and must survive escaping.
	if !strings.Contains(msg, "<b>claude</b>") {
		t.Fatalf("title is not marked up: %q", msg)
	}
	if !strings.Contains(msg, "needs you") {
		t.Fatalf("attention message does not say so: %q", msg)
	}
}

func TestMessageSaysWhichKindItIs(t *testing.T) {
	done := EventMessage(manager.Event{Kind: manager.EventDone, Title: "codex", SandboxName: "api"}, "").Text
	if !strings.Contains(done, "finished") {
		t.Fatalf("done message does not say so: %q", done)
	}
	if strings.Contains(done, "needs you") {
		t.Fatalf("done message reads as an attention one: %q", done)
	}
}

// --- the button ---

func TestEventMessageLinksToTheSession(t *testing.T) {
	m := EventMessage(manager.Event{
		Kind:      manager.EventAttention,
		SessionID: "f705aa",
		Title:     "claude",
	}, "http://192.168.1.50:7788")

	if m.Link != "http://192.168.1.50:7788/sessions/f705aa" {
		t.Fatalf("link %q does not open the session", m.Link)
	}
	if m.Label == "" {
		t.Fatal("a button with no label on it")
	}
}

// A trailing slash on the address is the likeliest thing to be pasted in, and
// must not produce a doubled one in the middle of the link.
func TestSessionURLTakesTheAddressAsPasted(t *testing.T) {
	got := SessionURL("http://box.local:7788/", "abc")
	if got != "http://box.local:7788/sessions/abc" {
		t.Fatalf("url %q", got)
	}
}

// With nowhere worth linking to there is no button, rather than one that goes
// nowhere — Telegram refuses the whole message over a bad URL, which would
// mean no notification at all.
func TestNoAddressMeansNoButton(t *testing.T) {
	m := EventMessage(manager.Event{Kind: manager.EventDone, Title: "codex"}, "")
	if m.Link != "" || m.Label != "" {
		t.Fatalf("a button was added with no address: %+v", m)
	}
}

func TestSendPutsTheButtonInTheKeyboard(t *testing.T) {
	got := serve(t, `{"ok":true,"result":{}}`)
	tg := newTelegram("secret-token", "42")

	err := tg.Send(context.Background(), Message{
		Text:  "hello",
		Link:  "http://box.local:7788/sessions/abc",
		Label: "Open session",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	payload := <-got
	markup, ok := payload["reply_markup"].(map[string]any)
	if !ok {
		t.Fatalf("no reply_markup in %v", payload)
	}
	rows, ok := markup["inline_keyboard"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("inline_keyboard is %v", markup["inline_keyboard"])
	}
	row, _ := rows[0].([]any)
	if len(row) != 1 {
		t.Fatalf("row is %v", row)
	}
	button, _ := row[0].(map[string]any)
	if button["url"] != "http://box.local:7788/sessions/abc" {
		t.Fatalf("button url is %v", button["url"])
	}
	if button["text"] != "Open session" {
		t.Fatalf("button text is %v", button["text"])
	}
}

// A message with no button must not carry an empty keyboard, which Telegram
// renders as a stray blank row.
func TestSendOmitsTheKeyboardWithoutALink(t *testing.T) {
	got := serve(t, `{"ok":true,"result":{}}`)
	tg := newTelegram("secret-token", "42")

	if err := tg.Send(context.Background(), Message{Text: "hello"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if payload := <-got; payload["reply_markup"] != nil {
		t.Fatalf("a keyboard was sent for a message with no button: %v", payload["reply_markup"])
	}
}

// Telegram refuses a button whose URL it cannot parse, and refuses the message
// with it — so a bad address has to be caught while someone is looking at the
// form, not at three in the morning.
func TestCheckLinkBaseRejectsWhatTelegramWould(t *testing.T) {
	for _, bad := range []string{
		"192.168.1.50:7788", // no scheme
		"ftp://box.local",   // not a web address
		"http://",           // no host
		"just some words",
	} {
		if err := CheckLinkBase(bad); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}

	// A bare host and port is the mistake people make, and the message has to
	// say what to do about it rather than describing a stray colon.
	err := CheckLinkBase("192.168.1.50:7788")
	if !strings.Contains(err.Error(), "http://192.168.1.50:7788") {
		t.Fatalf("error does not suggest the fix: %v", err)
	}
	for _, good := range []string{
		"",
		"http://192.168.1.50:7788",
		"https://agents.example.com",
		"http://box.local:7788/",
	} {
		if err := CheckLinkBase(good); err != nil {
			t.Fatalf("%q was rejected: %v", good, err)
		}
	}
}
