package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CoOre/duskrun/internal/plugin"
)

// TestWebhookNotify posts an event and verifies the receiving server sees the
// expected JSON body and content type.
func TestWebhookNotify(t *testing.T) {
	want := plugin.Event{
		Kind: plugin.EventFailure, Task: "orders", RunID: 42,
		Message: "pg_dump failed", At: time.Date(2026, 7, 22, 2, 0, 0, 0, time.UTC),
	}

	var gotCT string
	var got struct {
		plugin.Event
		Title string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, err := New([]byte(`{"url":"` + srv.URL + `"}`))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := n.Notify(context.Background(), want); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotCT)
	}
	if got.Task != want.Task || got.RunID != want.RunID || got.Kind != want.Kind || got.Message != want.Message {
		t.Fatalf("received event = %+v, want %+v", got, want)
	}
	if got.Title != "✗ orders: бэкап не выполнен" {
		t.Errorf("title = %q, want the shared headline", got.Title)
	}
}

// TestWebhookNotifyErrorStatus surfaces a non-2xx response as an error.
func TestWebhookNotifyErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	n, _ := New([]byte(`{"url":"` + srv.URL + `"}`))
	if err := n.Notify(context.Background(), plugin.Event{Task: "t"}); err == nil {
		t.Fatal("Notify returned nil on 500, want error")
	}
}

// TestWebhookErrorHidesURL: Slack/Discord webhook URLs are credentials
// themselves, so a transport error must not echo the URL.
func TestWebhookErrorHidesURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	hook := srv.URL + "/services/T000/B000/SECRETPATH"
	srv.Close() // nothing listens there now: Do fails with *url.Error

	n, err := New([]byte(`{"url":"` + hook + `"}`))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = n.Notify(context.Background(), plugin.Event{Task: "t"})
	if err == nil {
		t.Fatal("Notify to a closed server succeeded, want error")
	}
	if strings.Contains(err.Error(), "SECRETPATH") {
		t.Errorf("error leaks the webhook URL: %s", err)
	}
}
