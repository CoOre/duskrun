package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/store/sqlite"

	// The channel endpoints validate against the plugin registry, so the tests
	// must link the same plugins the daemon does. Without telegram/webhook/smtp
	// here, "unknown notifier type" would mask the behaviour under test.
	_ "github.com/duskrun/duskrun/internal/notifier/smtp"
	_ "github.com/duskrun/duskrun/internal/notifier/telegram"
	_ "github.com/duskrun/duskrun/internal/notifier/webhook"
)

// notifierServer serves the channel endpoints over a fresh store. The migration
// seeds a "log" channel, so the list is never empty.
func notifierServer(t *testing.T) (*httptest.Server, *sqlite.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notifier.db")
	st, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	notes := core.NewNotifications(st, nil, core.NotificationsConfig{}, nil)
	srv := httptest.NewServer(NewRouter(Deps{
		Store: st, Token: testToken, Notify: notes,
		ToolCheck: func(string) error { return nil },
	}))
	t.Cleanup(srv.Close)
	return srv, st
}

// TestNotifierSeededLogChannel: the migration must leave existing tasks with a
// working channel rather than silently dropping their only one.
func TestNotifierSeededLogChannel(t *testing.T) {
	srv, _ := notifierServer(t)

	var channels []notifierDTO
	decodeInto(t, authGet(t, srv.URL+"/api/notifiers"), &channels)
	if len(channels) != 1 || channels[0].Name != "log" {
		t.Fatalf("channels = %+v, want the seeded log channel", channels)
	}
	if len(channels[0].Events) != 4 {
		t.Errorf("events = %v, want all four kinds", channels[0].Events)
	}
	if !channels[0].Enabled {
		t.Error("the seeded channel should be enabled")
	}
}

// TestCreateNotifierDefaultsToAllEvents: a channel created without an explicit
// subscription would be configured but deliver nothing, which reads as broken.
func TestCreateNotifierDefaultsToAllEvents(t *testing.T) {
	srv, _ := notifierServer(t)

	body := `{"name":"ops","type":"webhook","config":{"url":"https://example.invalid/hook"}}`
	resp := authPost(t, srv.URL+"/api/notifiers", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	var channels []notifierDTO
	decodeInto(t, authGet(t, srv.URL+"/api/notifiers"), &channels)
	for _, c := range channels {
		if c.Name != "ops" {
			continue
		}
		if len(c.Events) != 4 {
			t.Errorf("events = %v, want all four kinds by default", c.Events)
		}
		return
	}
	t.Fatal("created channel not listed")
}

func TestCreateNotifierRejectsUnknownTypeAndEvent(t *testing.T) {
	srv, _ := notifierServer(t)

	resp := authPost(t, srv.URL+"/api/notifiers", `{"name":"x","type":"carrier-pigeon"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown type status = %d, want 400", resp.StatusCode)
	}

	resp = authPost(t, srv.URL+"/api/notifiers", `{"name":"y","type":"log","events":["explosion"]}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown event status = %d, want 400", resp.StatusCode)
	}
}

// TestDeleteNotifierInUse: removing a channel a task still references would
// stop that task notifying with nothing to show for it.
func TestDeleteNotifierInUse(t *testing.T) {
	srv, st := notifierServer(t)
	ctx := context.Background()

	storageID, err := st.CreateStorage(ctx, core.Storage{Name: "s", Type: "localfs", Config: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	connID, err := st.CreateConnection(ctx, core.Connection{Name: "c", Engine: "postgres", ConnectorType: "direct", ConnectorConfig: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(ctx, core.Task{
		Name: "nightly", ConnectionID: connID, StorageID: storageID,
		DumperOpts: []byte(`{}`), CodecChain: []string{}, Cron: "0 2 * * *",
		Notifiers: []string{"log"}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	resp := authDelete(t, srv.URL+"/api/notifiers/1")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409 while a task still references the channel", resp.StatusCode)
	}

	// And the listing explains who is holding it.
	var channels []notifierDTO
	decodeInto(t, authGet(t, srv.URL+"/api/notifiers"), &channels)
	if len(channels[0].UsedBy) != 1 || channels[0].UsedBy[0] != "nightly" {
		t.Errorf("used_by = %v, want [nightly]", channels[0].UsedBy)
	}
}

// TestTaskRejectsUnconfiguredChannel: notifiers now name configured channels,
// so a plugin type that nobody set up is not a valid choice.
func TestTaskRejectsUnconfiguredChannel(t *testing.T) {
	srv, st := notifierServer(t)
	ctx := context.Background()
	if _, err := st.CreateStorage(ctx, core.Storage{Name: "s", Type: "localfs", Config: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateConnection(ctx, core.Connection{Name: "c", Engine: "postgres", ConnectorType: "direct", ConnectorConfig: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}

	body := `{"name":"t","connection_id":1,"storage_id":1,"cron":"0 2 * * *","codec_chain":[],"notifiers":["telegram"]}`
	resp := authPost(t, srv.URL+"/api/tasks", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unconfigured channel name", resp.StatusCode)
	}
}

// TestTestNotifierReportsFailure: the test endpoint answers 200 with a failed
// status rather than an HTTP error — the request worked, the delivery did not.
func TestTestNotifierReportsFailure(t *testing.T) {
	srv, st := notifierServer(t)
	ctx := context.Background()
	if _, err := st.CreateNotifier(ctx, core.NotifierChannel{
		Name: "broken", Type: "telegram", Config: []byte(`{}`), // no token/chat_id
		Events: []string{"success"}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	decodeInto(t, authPost(t, srv.URL+"/api/notifiers/2/test", ""), &got)
	if got["status"] != "failed" {
		t.Fatalf("status = %v, want failed for an unbuildable channel", got["status"])
	}
	if got["error"] == "" || got["error"] == nil {
		t.Error("a failed test should say why")
	}
}

// TestTestNotifierSucceeds: the log channel builds and accepts an event.
func TestTestNotifierSucceeds(t *testing.T) {
	srv, _ := notifierServer(t)

	var got map[string]any
	decodeInto(t, authPost(t, srv.URL+"/api/notifiers/1/test", ""), &got)
	if got["status"] != "ok" {
		t.Errorf("status = %v (%v), want ok", got["status"], got["error"])
	}
}

func TestListNotificationsRejectsBadLimit(t *testing.T) {
	srv, _ := notifierServer(t)
	for _, q := range []string{"limit=0", "limit=100000"} {
		resp := authGet(t, srv.URL+"/api/notifications?"+q)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, resp.StatusCode)
		}
	}
}

// TestListNotifiersMasksInlineSecrets: the API accepts an inline credential, so
// the listing must not hand it back — GET /notifiers is readable by anyone with
// the token, including a read-only UI session.
func TestListNotifiersMasksInlineSecrets(t *testing.T) {
	srv, st := notifierServer(t)
	if _, err := st.CreateNotifier(context.Background(), core.NotifierChannel{
		Name: "tg", Type: "telegram",
		Config: []byte(`{"token":"123:REAL-BOT-TOKEN","chat_id":"-100","token_ref":"secret://tg/bot"}`),
		Events: []string{"failure"}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	body := readBody(t, authGet(t, srv.URL+"/api/notifiers"))
	if strings.Contains(body, "REAL-BOT-TOKEN") {
		t.Fatalf("the bot token leaked in the listing: %s", body)
	}
	// Everything an operator needs to recognise the channel survives, and so
	// does the secret reference, which is not sensitive.
	for _, want := range []string{"-100", "secret://tg/bot", "••••"} {
		if !strings.Contains(body, want) {
			t.Errorf("listing should keep %q, got %s", want, body)
		}
	}
}

// TestPatchNotifierKeepsOmittedFields: PATCH replaces the row, so a client that
// does not resend events/enabled must not have a narrowed channel silently
// re-subscribed to everything or a muted one switched back on.
func TestPatchNotifierKeepsOmittedFields(t *testing.T) {
	srv, st := notifierServer(t)
	ctx := context.Background()
	id, err := st.CreateNotifier(ctx, core.NotifierChannel{
		Name: "narrow", Type: "log", Config: []byte(`{}`),
		Events: []string{"failure"}, Enabled: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp := authPatch(t, srv.URL+"/api/notifiers/"+itoa(id), `{"name":"narrow","type":"log"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	got, err := st.GetNotifier(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 1 || got.Events[0] != "failure" {
		t.Errorf("events = %v, want the narrowed subscription preserved", got.Events)
	}
	if got.Enabled {
		t.Error("enabled = true, want the muted channel left muted")
	}
}

// TestPatchNotifierRestoresMaskedSecret: the listing masks credentials and a
// client editing a channel resends the config it read. Persisting the mask would
// replace the working token with four bullets on an edit made for something else.
func TestPatchNotifierRestoresMaskedSecret(t *testing.T) {
	srv, st := notifierServer(t)
	ctx := context.Background()
	id, err := st.CreateNotifier(ctx, core.NotifierChannel{
		Name: "tg", Type: "telegram",
		Config: []byte(`{"token":"123:REAL-BOT-TOKEN","chat_id":"-100"}`),
		Events: []string{"failure"}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Exactly what the SPA sends back after reading the masked listing, with the
	// chat id edited.
	body := `{"name":"tg","type":"telegram","config":{"token":"••••","chat_id":"-200"}}`
	resp := authPatch(t, srv.URL+"/api/notifiers/"+itoa(id), body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	got, err := st.GetNotifier(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got.Config), "123:REAL-BOT-TOKEN") {
		t.Errorf("config = %s, want the stored token preserved", got.Config)
	}
	if !strings.Contains(string(got.Config), "-200") {
		t.Errorf("config = %s, want the edited chat_id applied", got.Config)
	}
}

// TestCreateNotifierRejectsUnbuildableConfig: the type check only says the
// plugin exists. A channel that cannot be constructed would otherwise sit in the
// list looking healthy and fail for the first time in the delivery log, after a
// backup has already failed with nobody notified.
func TestCreateNotifierRejectsUnbuildableConfig(t *testing.T) {
	srv, _ := notifierServer(t)

	// smtp without recipients: valid JSON, registered type, impossible channel.
	body := `{"name":"ops-mail","type":"smtp","config":{"host":"smtp.corp.io","from":"a@b.io"}}`
	resp := authPost(t, srv.URL+"/api/notifiers", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a config the plugin refuses", resp.StatusCode)
	}
	if msg := readBody(t, resp); !strings.Contains(msg, "to is required") {
		t.Errorf("error = %s, want the plugin's own reason", msg)
	}
}

// TestRenameNotifierCascadesToTasks: tasks reference channels by name, so a
// rename that did not cascade would leave every referencing task delivering to
// nothing, visible only as a warn line in the daemon log.
func TestRenameNotifierCascadesToTasks(t *testing.T) {
	srv, st := notifierServer(t)
	ctx := context.Background()

	storageID, err := st.CreateStorage(ctx, core.Storage{Name: "s", Type: "localfs", Config: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	connID, err := st.CreateConnection(ctx, core.Connection{Name: "c", Engine: "postgres", ConnectorType: "direct", ConnectorConfig: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	// Two tasks: one referencing the channel, one not — the rename must not
	// touch the second.
	taskID, err := st.CreateTask(ctx, core.Task{
		Name: "watched", ConnectionID: connID, StorageID: storageID,
		DumperOpts: []byte(`{}`), CodecChain: []string{}, Cron: "0 2 * * *",
		Notifiers: []string{"log"}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := st.CreateTask(ctx, core.Task{
		Name: "unwatched", ConnectionID: connID, StorageID: storageID,
		DumperOpts: []byte(`{}`), CodecChain: []string{}, Cron: "0 3 * * *",
		Notifiers: []string{}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp := authPatch(t, srv.URL+"/api/notifiers/1", `{"name":"journal","type":"log"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rename status = %d", resp.StatusCode)
	}

	task, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(task.Notifiers) != 1 || task.Notifiers[0] != "journal" {
		t.Errorf("notifiers = %v, want the reference renamed to journal", task.Notifiers)
	}
	other, err := st.GetTask(ctx, otherID)
	if err != nil {
		t.Fatal(err)
	}
	if len(other.Notifiers) != 0 {
		t.Errorf("unrelated task notifiers = %v, want untouched", other.Notifiers)
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestPatchNotifierKeepsConfig: the listing masks credentials, so a client that
// echoes back what it read must not persist "••••" over the real token. The UI
// toggles enabled/events without resending config, and the server keeps it.
func TestPatchNotifierKeepsConfig(t *testing.T) {
	srv, st := notifierServer(t)
	ctx := context.Background()
	id, err := st.CreateNotifier(ctx, core.NotifierChannel{
		Name: "tg", Type: "telegram",
		Config: []byte(`{"token":"123:REAL","chat_id":"-100"}`),
		Events: []string{"failure"}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp := authPatch(t, srv.URL+"/api/notifiers/"+itoa(id),
		`{"name":"tg","type":"telegram","enabled":false}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	got, err := st.GetNotifier(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got.Config), "123:REAL") {
		t.Errorf("config = %s, want the stored credential preserved", got.Config)
	}
	if got.Enabled {
		t.Error("the requested change (enabled=false) should still apply")
	}
}
