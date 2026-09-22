package logn

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/duskrun/duskrun/internal/plugin"
)

// TestLogNotify checks the event is written to the logger with its fields.
func TestLogNotify(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	n := NewWithLogger(logger)

	ev := plugin.Event{Kind: plugin.EventFailure, Task: "orders", RunID: 9, Message: "boom"}
	if err := n.Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"orders", "failure", "boom", `"level":"ERROR"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q\n%s", want, out)
		}
	}
}

func TestLogRegistered(t *testing.T) {
	if !plugin.Notifiers.Has("log") {
		t.Fatal(`notifier "log" not registered`)
	}
}
