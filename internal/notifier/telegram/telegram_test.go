package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/duskrun/duskrun/internal/plugin"
)

// TestTelegramNotify points the base URL at a test server and checks the
// sendMessage payload carries the chat_id and a text mentioning the task.
func TestTelegramNotify(t *testing.T) {
	var path string
	var payload sendMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	cfg, _ := json.Marshal(Config{BaseURL: srv.URL, Token: "T0KEN", ChatID: "12345"})
	n, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ev := plugin.Event{Kind: plugin.EventSuccess, Task: "nightly", RunID: 7, Message: "done"}
	if err := n.Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	if path != "/botT0KEN/sendMessage" {
		t.Errorf("path = %q, want /botT0KEN/sendMessage", path)
	}
	if payload.ChatID != "12345" {
		t.Errorf("chat_id = %q, want 12345", payload.ChatID)
	}
	if !strings.Contains(payload.Text, "nightly") {
		t.Errorf("text = %q, want it to mention the task", payload.Text)
	}
}

func TestTelegramValidation(t *testing.T) {
	if _, err := New([]byte(`{"chat_id":"1"}`)); err == nil {
		t.Error("New without token succeeded, want error")
	}
	if _, err := New([]byte(`{"token":"t"}`)); err == nil {
		t.Error("New without chat_id succeeded, want error")
	}
	// Default base URL applied.
	n, err := New([]byte(`{"token":"t","chat_id":"1"}`))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := n.(*Notifier).cfg.BaseURL; got != DefaultBaseURL {
		t.Errorf("base url = %q, want default %q", got, DefaultBaseURL)
	}
}
