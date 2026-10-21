// Package telegram implements a Notifier that sends run events to a Telegram
// chat via the Bot API sendMessage method. The base URL is injectable so tests
// (and self-hosted proxies) can point it at a local server.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/CoOre/duskrun/internal/plugin"
)

func init() {
	plugin.Notifiers.Register("telegram", New)
}

// DefaultBaseURL is the public Telegram Bot API endpoint.
const DefaultBaseURL = "https://api.telegram.org"

// Config is the telegram notifier config. token_ref is resolved upstream from a
// secret; token is the resolved value.
type Config struct {
	BaseURL  string `json:"base_url"`
	Token    string `json:"token"`
	TokenRef string `json:"token_ref"`
	ChatID   string `json:"chat_id"`
}

// New builds a telegram Notifier.
func New(raw []byte) (plugin.Notifier, error) {
	var c Config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("telegram: bad config: %w", err)
		}
	}
	if c.Token == "" {
		return nil, fmt.Errorf("telegram: token is required")
	}
	if c.ChatID == "" {
		return nil, fmt.Errorf("telegram: chat_id is required")
	}
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	return &Notifier{cfg: c, client: &http.Client{Timeout: 10 * time.Second}}, nil
}

// Notifier posts sendMessage requests to the Telegram Bot API.
type Notifier struct {
	cfg    Config
	client *http.Client
}

func (n *Notifier) Name() string { return "telegram" }

type sendMessage struct {
	ChatID string `json:"chat_id"`
	Text   string `json:"text"`
}

// Notify formats the event and posts it to sendMessage.
func (n *Notifier) Notify(ctx context.Context, ev plugin.Event) error {
	payload, err := json.Marshal(sendMessage{
		ChatID: n.cfg.ChatID,
		Text:   formatEvent(ev),
	})
	if err != nil {
		return fmt.Errorf("telegram: marshal: %w", err)
	}
	endpoint := n.cfg.BaseURL + "/bot" + n.cfg.Token + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("telegram: new request: %w", withoutURL(err))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: post: %w", withoutURL(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// formatEvent leads with the headline, so a chat list preview says what
// happened; the message and the run follow on their own lines.
func formatEvent(ev plugin.Event) string {
	var b strings.Builder
	b.WriteString(plugin.Title(ev))
	if ev.Message != "" {
		b.WriteString("\n")
		b.WriteString(ev.Message)
	}
	if ev.RunID != 0 {
		fmt.Fprintf(&b, "\nЗапуск #%d", ev.RunID)
	}
	return b.String()
}

// withoutURL drops the request URL from net/http errors: *url.Error prints it
// in full, and here the URL carries the credential. The cause is kept.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}
