package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Shonz1/agent-web-manager/internal/manager"
)

const (
	// EnvToken and EnvChatID configure the bot from the environment, for a
	// deployment that would rather not have a credential written to disk by a
	// web page. When they are set they win, and the settings page says so
	// instead of offering to change something it cannot.
	EnvToken  = "AWM_TELEGRAM_TOKEN"
	EnvChatID = "AWM_TELEGRAM_CHAT_ID"
	// EnvLinkBase is optional even when the other two are set: without it the
	// notifications simply have no button.
	EnvLinkBase = "AWM_LINK_BASE"

	// telegramFile is where the settings page persists the bot, under the
	// state directory beside the sandbox records.
	telegramFile = "telegram.json"
)

// Settings is what the UI is told about the current configuration.
//
// The token is deliberately not in here. It is a bearer credential for the
// whole bot, the page never needs it back to display the state it is in, and
// anything sent to the browser is one careless screenshot from being public.
type Settings struct {
	Enabled bool   `json:"enabled"`
	ChatID  string `json:"chatId,omitempty"`
	// LinkBase is the address the "Open session" button under each
	// notification points at. Empty means no button: there is no address worth
	// linking to, and a phone tapping 127.0.0.1 would reach itself.
	LinkBase string `json:"linkBase,omitempty"`
	// Bot is the @username the token belongs to, once it has been checked.
	Bot string `json:"bot,omitempty"`
	// FromEnv reports that the environment is in charge, which makes this
	// read-only: writing a file that the environment would go on shadowing
	// would be a setting that silently did nothing.
	FromEnv bool `json:"fromEnv"`
}

// Service owns the notification configuration and the relay that uses it.
//
// The configuration can change while the manager is running, so the relay
// reads it per event rather than capturing it: switching the bot off has to
// stop the next notification, not the one after the restart.
type Service struct {
	path string

	mu       sync.Mutex
	tg       *Telegram
	chatID   string
	linkBase string
	bot      string
	fromEnv  bool
}

// NewService loads whatever is already configured. It does not check the
// credentials — that is a network call, and the manager should not wait on
// Telegram to start serving.
func NewService(stateDir string) (*Service, error) {
	s := &Service{path: filepath.Join(stateDir, telegramFile)}

	token := strings.TrimSpace(os.Getenv(EnvToken))
	chatID := strings.TrimSpace(os.Getenv(EnvChatID))
	if token != "" || chatID != "" {
		if token == "" || chatID == "" {
			return nil, fmt.Errorf("telegram: %s and %s must be set together", EnvToken, EnvChatID)
		}
		s.fromEnv = true
		// The environment has no link address in it, so a deployment that uses
		// it gets notifications without a button. Somewhere to send them is the
		// part that matters; where to tap back to is a convenience.
		s.set(token, chatID, strings.TrimSpace(os.Getenv(EnvLinkBase)))
		return s, nil
	}

	cfg, err := readConfig(s.path)
	if err != nil {
		return nil, err
	}
	if cfg.Token != "" && cfg.ChatID != "" {
		s.set(cfg.Token, cfg.ChatID, cfg.LinkBase)
	}
	return s, nil
}

// set replaces the live bot. The caller must not hold s.mu.
func (s *Service) set(token, chatID, linkBase string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tg = newTelegram(token, chatID)
	s.chatID = chatID
	s.linkBase = linkBase
	s.bot = ""
}

// Settings reports what the UI should show.
func (s *Service) Settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Settings{
		Enabled:  s.tg != nil,
		ChatID:   s.chatID,
		LinkBase: s.linkBase,
		Bot:      s.bot,
		FromEnv:  s.fromEnv,
	}
}

// current returns the bot to send with, or nil when notifications are off.
func (s *Service) current() *Telegram {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tg
}

// ErrFromEnv is returned when the settings page tries to change a
// configuration that the environment owns.
var ErrFromEnv = fmt.Errorf("telegram is configured by %s and %s, so it cannot be changed here", EnvToken, EnvChatID)

// ErrNotConfigured is returned when there is no bot to act on.
var ErrNotConfigured = errors.New("telegram is not configured")

// Configure validates a bot and, if it works, persists it and starts using it.
//
// Validation is a real message rather than a getMe, because getMe only proves
// the token parses: the chat id is the half people get wrong, and the only way
// to know a bot can post to a chat is to post to it. It doubles as the
// confirmation that the person is looking for anyway.
func (s *Service) Configure(ctx context.Context, token, chatID, linkBase string) (Settings, error) {
	token = strings.TrimSpace(token)
	chatID = strings.TrimSpace(chatID)
	linkBase = strings.TrimSpace(linkBase)

	s.mu.Lock()
	fromEnv := s.fromEnv
	// An empty token against a bot that is already set up means "leave the
	// token alone". Changing where the button points, or which chat to use,
	// should not mean going to find a credential that is already here — and it
	// cannot be pre-filled in the form, because it is never sent to the page.
	if token == "" && s.tg != nil {
		token = s.tg.token
	}
	s.mu.Unlock()

	if fromEnv {
		return Settings{}, ErrFromEnv
	}
	if token == "" || chatID == "" {
		return Settings{}, errors.New("a bot token and a chat id are both needed")
	}
	if err := CheckLinkBase(linkBase); err != nil {
		return Settings{}, err
	}

	candidate := newTelegram(token, chatID)
	bot, err := candidate.Check(ctx)
	if err != nil {
		return Settings{}, err
	}
	// Sent with the button, so that an address Telegram will not accept is
	// found now — while someone is looking at the form — rather than by the
	// notifications quietly failing later.
	if err := candidate.Send(ctx, managerMessage(connectedMessage, linkBase)); err != nil {
		return Settings{}, err
	}

	// Only written once it is known to work, so a saved configuration is
	// always one that was working at the moment it was saved.
	cfg := config{Token: token, ChatID: chatID, LinkBase: linkBase}
	if err := writeConfig(s.path, cfg); err != nil {
		return Settings{}, err
	}

	s.mu.Lock()
	s.tg = candidate
	s.chatID = chatID
	s.linkBase = linkBase
	s.bot = bot
	s.mu.Unlock()

	return s.Settings(), nil
}

// managerMessage is one of the two messages about the manager itself, whose
// button goes to the manager rather than to any particular session.
func managerMessage(text, linkBase string) Message {
	m := Message{Text: text}
	if linkBase != "" {
		m.Link = strings.TrimSuffix(linkBase, "/")
		m.Label = "Open the manager"
	}
	return m
}

// Disable turns notifications off and forgets the credentials.
func (s *Service) Disable() (Settings, error) {
	s.mu.Lock()
	fromEnv := s.fromEnv
	s.mu.Unlock()
	if fromEnv {
		return Settings{}, ErrFromEnv
	}

	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Settings{}, fmt.Errorf("remove %s: %w", s.path, err)
	}

	s.mu.Lock()
	s.tg = nil
	s.chatID = ""
	s.linkBase = ""
	s.bot = ""
	s.mu.Unlock()

	return s.Settings(), nil
}

// Test sends a message with the configuration as it stands, which is the only
// way to find out that a bot someone has since blocked has stopped working.
func (s *Service) Test(ctx context.Context) error {
	s.mu.Lock()
	tg, linkBase := s.tg, s.linkBase
	s.mu.Unlock()
	if tg == nil {
		return ErrNotConfigured
	}
	// Carries the button too, so that tapping it is how you find out the
	// address works from the device you are holding.
	return tg.Send(ctx, managerMessage(testMessage, linkBase))
}

// Verify checks the configured token and records the bot it belongs to, so the
// log and the settings page can name it. A failure is reported, not stored:
// the configuration is still what the person set, and the network is the thing
// that is wrong.
func (s *Service) Verify(ctx context.Context) (string, error) {
	tg := s.current()
	if tg == nil {
		return "", ErrNotConfigured
	}
	bot, err := tg.Check(ctx)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.bot = bot
	s.mu.Unlock()
	return bot, nil
}

// Relay posts every event to whichever bot is configured at the moment it
// arrives, until the events stop or ctx ends.
//
// Failures are logged and dropped rather than retried. These are notifications
// about a state that is still changing: an event held in a queue for a minute
// describes a session that has moved on, and the next one will be along
// shortly.
func (s *Service) Relay(ctx context.Context, events <-chan manager.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			s.mu.Lock()
			tg, linkBase := s.tg, s.linkBase
			s.mu.Unlock()
			if tg == nil {
				continue // switched off since the event was raised
			}
			sendCtx, cancel := context.WithTimeout(ctx, requestTimeout)
			err := tg.Send(sendCtx, EventMessage(ev, linkBase))
			cancel()
			if err != nil {
				log.Printf("notify: %v", err)
			}
		}
	}
}

// --- persistence ---

// config is what telegramFile holds.
type config struct {
	Token    string `json:"token"`
	ChatID   string `json:"chatId"`
	LinkBase string `json:"linkBase,omitempty"`
}

func readConfig(path string) (config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return config{}, nil
	}
	if err != nil {
		return config{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.Token = strings.TrimSpace(cfg.Token)
	cfg.ChatID = strings.TrimSpace(cfg.ChatID)
	cfg.LinkBase = strings.TrimSpace(cfg.LinkBase)
	return cfg, nil
}

// writeConfig saves the credentials readable only by their owner, and by way
// of a rename so that an interrupted write cannot leave half a token behind.
func writeConfig(path string, cfg config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("save telegram settings: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("save telegram settings: %w", err)
	}
	return nil
}
