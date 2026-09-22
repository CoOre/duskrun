// Package logn implements a Notifier that records run events to the structured
// logger — a zero-config default channel and a fallback when no external sink is
// configured. (Named logn to avoid shadowing the standard log package.)
package logn

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/duskrun/duskrun/internal/plugin"
)

func init() {
	plugin.Notifiers.Register("log", New)
}

// New builds a log Notifier writing to the default logger.
func New(raw []byte) (plugin.Notifier, error) {
	if len(raw) > 0 && !json.Valid(raw) {
		return nil, fmt.Errorf("log: bad config")
	}
	return &Notifier{log: slog.Default()}, nil
}

// NewWithLogger builds a log Notifier over a specific logger (for tests/wiring).
func NewWithLogger(l *slog.Logger) *Notifier {
	if l == nil {
		l = slog.Default()
	}
	return &Notifier{log: l}
}

// Notifier logs events via slog.
type Notifier struct {
	log *slog.Logger
}

func (n *Notifier) Name() string { return "log" }

// Notify emits the event at a level matching its kind (failures at ERROR).
func (n *Notifier) Notify(_ context.Context, ev plugin.Event) error {
	level := slog.LevelInfo
	if ev.Kind == plugin.EventFailure || ev.Kind == plugin.EventRetentionError {
		level = slog.LevelError
	}
	n.log.LogAttrs(context.Background(), level, "notification",
		slog.String("kind", string(ev.Kind)),
		slog.String("task", ev.Task),
		slog.Int64("run", ev.RunID),
		slog.String("message", ev.Message),
	)
	return nil
}
