// Package notify sends the manager's events somewhere the person will see
// them when they are not looking at the page.
//
// Nothing here runs unless a bot has been configured. Until then this manager
// makes no outbound connections at all, which is worth keeping true by default
// for a tool whose whole job is running agents against your source.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/oleksiiipatov/agent-web-manager/internal/manager"
)

const requestTimeout = 15 * time.Second

// The two messages the manager sends about itself rather than about a session.
const (
	connectedMessage = "✅ <b>agent-web-manager</b> is connected. This is where your agents will reach you."
	testMessage      = "🔔 <b>agent-web-manager</b> test message. Notifications are working."
)

// Message is one Telegram message: what it says, and where the button under it
// goes.
type Message struct {
	Text string
	// Link is the button's destination. Empty for no button, which is what a
	// manager with no address worth linking to sends.
	Link string
	// Label is the button's text.
	Label string
}

// Telegram posts messages to one chat as one bot.
type Telegram struct {
	token  string
	chatID string
	client *http.Client
}

func newTelegram(token, chatID string) *Telegram {
	return &Telegram{
		token:  token,
		chatID: chatID,
		client: &http.Client{Timeout: requestTimeout},
	}
}

// Check reports the bot's username, which is both a test that the token works
// and something to name in the log and on the settings page.
func (t *Telegram) Check(ctx context.Context) (string, error) {
	var me struct {
		Username string `json:"username"`
	}
	if err := t.call(ctx, "getMe", nil, &me); err != nil {
		return "", err
	}
	return me.Username, nil
}

// Send posts one message to the configured chat.
func (t *Telegram) Send(ctx context.Context, m Message) error {
	body := map[string]any{
		"chat_id":    t.chatID,
		"text":       m.Text,
		"parse_mode": "HTML",
	}
	if m.Link != "" {
		body["reply_markup"] = map[string]any{
			"inline_keyboard": [][]map[string]any{{
				{"text": m.Label, "url": m.Link},
			}},
		}
	}
	return t.call(ctx, "sendMessage", body, nil)
}

// apiResponse is the envelope every Telegram method answers with.
type apiResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

func (t *Telegram) call(ctx context.Context, method string, body, out any) error {
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("telegram %s: %w", method, err)
		}
		payload = bytes.NewReader(data)
	}

	url := fmt.Sprintf("%s/bot%s/%s", baseURL, t.token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, payload)
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, t.redact(err))
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, t.redact(err))
	}
	defer resp.Body.Close()

	// Bounded: an error page from something that is not Telegram at all — a
	// captive portal, a proxy — should not be read into memory unbounded.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, t.redact(err))
	}

	var envelope apiResponse
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("telegram %s: unexpected reply (%s)", method, resp.Status)
	}
	if !envelope.OK {
		return fmt.Errorf("telegram: %s", strings.TrimPrefix(envelope.Description, "Unauthorized: "))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, out); err != nil {
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	return nil
}

// baseURL is the API root, overridden by tests.
var baseURL = "https://api.telegram.org"

// redact keeps the bot token out of the logs and out of anything the API hands
// back to the browser.
//
// Telegram authenticates by putting the token in the request path, and Go puts
// the request URL into every error it returns from a transport — so the
// obvious log of a failed send would write a live credential into the log
// every time the network hiccupped.
func (t *Telegram) redact(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if !strings.Contains(msg, t.token) {
		return err
	}
	return errors.New(strings.ReplaceAll(msg, t.token, "<token>"))
}

// EventMessage renders an event as the message announcing it, with a button
// that opens the session it is about.
//
// Sent as HTML, so everything that came off a terminal — the session's title,
// the name an agent gave its own conversation, a shell's last command — is
// escaped. An agent that writes "<b>" into its conversation title is not
// unusual, and Telegram rejects the whole message if the markup does not
// parse.
func EventMessage(ev manager.Event, linkBase string) Message {
	icon, verb := "✅", "finished"
	if ev.Kind == manager.EventAttention {
		icon, verb = "⚠️", "needs you"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>%s</b> %s", icon, html.EscapeString(ev.Title), verb)
	if ev.Detail != "" {
		fmt.Fprintf(&b, "\n%s", html.EscapeString(ev.Detail))
	}
	fmt.Fprintf(&b, "\n<i>%s</i>", html.EscapeString(ev.SandboxName))

	m := Message{Text: b.String()}
	if linkBase != "" {
		m.Link = SessionURL(linkBase, ev.SessionID)
		m.Label = "Open session"
	}
	return m
}

// SessionURL is where a session lives in the UI, under the address the manager
// is reachable at.
func SessionURL(linkBase, sessionID string) string {
	return strings.TrimSuffix(linkBase, "/") + "/sessions/" + sessionID
}

// CheckLinkBase reports whether an address is one Telegram will accept as a
// button, and one that will resolve to this manager when it is tapped.
//
// Telegram refuses a button whose URL it cannot parse, and it refuses the whole
// message with it — so a bad address here would mean no notifications at all
// rather than notifications without a button.
func CheckLinkBase(linkBase string) error {
	if linkBase == "" {
		return nil // no button, which is allowed
	}
	// Checked before parsing, because a bare host and port is the mistake
	// people actually make and url.Parse describes it as a stray colon in a
	// path segment, which explains nothing to anyone.
	if !strings.HasPrefix(linkBase, "http://") && !strings.HasPrefix(linkBase, "https://") {
		return fmt.Errorf("the address has to start with http:// or https:// — try %q", "http://"+linkBase)
	}
	u, err := url.Parse(linkBase)
	if err != nil {
		return fmt.Errorf("%q is not an address: %w", linkBase, err)
	}
	if u.Host == "" {
		return fmt.Errorf("%q has no host in it", linkBase)
	}
	return nil
}
