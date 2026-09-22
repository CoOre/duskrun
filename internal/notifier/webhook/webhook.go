// Package webhook implements a Notifier that POSTs the run Event as JSON to a
// configured URL — the generic integration point for Slack/Discord/custom sinks.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/duskrun/duskrun/internal/plugin"
)

func init() {
	plugin.Notifiers.Register("webhook", New)
}

// Config is the webhook notifier config.
type Config struct {
	URL string `json:"url"`
}

// New builds a webhook Notifier from JSON config.
func New(raw []byte) (plugin.Notifier, error) {
	var c Config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("webhook: bad config: %w", err)
		}
	}
	if c.URL == "" {
		return nil, fmt.Errorf("webhook: url is required")
	}
	return &Notifier{url: c.URL, client: &http.Client{Timeout: 10 * time.Second}}, nil
}

// Notifier POSTs events to a URL.
type Notifier struct {
	url    string
	client *http.Client
}

func (n *Notifier) Name() string { return "webhook" }

// Notify sends the event as a JSON body. A non-2xx response is an error.
func (n *Notifier) Notify(ctx context.Context, ev plugin.Event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("webhook: marshal event: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook: unexpected status %d", resp.StatusCode)
	}
	return nil
}
