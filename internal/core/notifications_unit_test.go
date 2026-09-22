package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/duskrun/duskrun/internal/plugin"
)

var noteNow = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

// fakeNotifyStore is an in-memory NotifyStore.
type fakeNotifyStore struct {
	channels []NotifierChannel
	secrets  map[string]*Secret
	recorded []Notification
	listErr  error
}

func (f *fakeNotifyStore) ListNotifiers(context.Context) ([]NotifierChannel, error) {
	return f.channels, f.listErr
}
func (f *fakeNotifyStore) GetSecretByRef(_ context.Context, ref string) (*Secret, error) {
	sec, ok := f.secrets[ref]
	if !ok {
		return nil, errors.New("no such secret")
	}
	return sec, nil
}
func (f *fakeNotifyStore) InsertNotification(_ context.Context, n Notification) (int64, error) {
	f.recorded = append(f.recorded, n)
	return int64(len(f.recorded)), nil
}

// plainOpener returns the ciphertext as-is, standing in for decryption.
type plainOpener struct{}

func (plainOpener) Open(s Secret) ([]byte, error) { return s.Ciphertext, nil }

func newNotifications(store *fakeNotifyStore, box SecretOpener) *Notifications {
	return NewNotifications(store, box, NotificationsConfig{
		Now: func() time.Time { return noteNow },
	}, nil)
}

// chan_ builds a channel row subscribed to the given kinds.
func chan_(name, typ, config string, kinds ...string) NotifierChannel {
	return NotifierChannel{
		Name: name, Type: typ, Config: json.RawMessage(config),
		Events: kinds, Enabled: true,
	}
}

// TestSendFiltersByEventKind: a channel only hears the kinds it subscribed to.
// This is what makes the mockup's event×channel matrix real.
func TestSendFiltersByEventKind(t *testing.T) {
	registerSpyNotifier(t)
	store := &fakeNotifyStore{channels: []NotifierChannel{
		chan_("only-failures", "spy-plugin", `{}`, "failure"),
	}}
	task := &Task{Name: "t", Notifiers: []string{"only-failures"}}
	n := newNotifications(store, nil)

	spyEvents = nil
	n.Send(context.Background(), task, plugin.EventSuccess, 1, "ok")
	if len(spyEvents) != 0 {
		t.Fatalf("events = %+v, want none — the channel is not subscribed to success", spyEvents)
	}

	n.Send(context.Background(), task, plugin.EventFailure, 1, "boom")
	if len(spyEvents) != 1 || spyEvents[0].Kind != plugin.EventFailure {
		t.Fatalf("events = %+v, want the failure delivered", spyEvents)
	}
}

// TestSendSkipsDisabledChannel: the toggle on the channel card mutes it without
// having to edit every task that references it.
func TestSendSkipsDisabledChannel(t *testing.T) {
	registerSpyNotifier(t)
	ch := chan_("muted", "spy-plugin", `{}`, "success")
	ch.Enabled = false
	store := &fakeNotifyStore{channels: []NotifierChannel{ch}}

	spyEvents = nil
	newNotifications(store, nil).Send(context.Background(),
		&Task{Name: "t", Notifiers: []string{"muted"}}, plugin.EventSuccess, 1, "ok")

	if len(spyEvents) != 0 {
		t.Errorf("events = %+v, want none from a disabled channel", spyEvents)
	}
}

// TestSendResolvesSecretRefs: a "<key>_ref" in the config is replaced by the
// decrypted secret, which is what lets a bot token live in the secret store
// rather than in plaintext beside the chat id.
func TestSendResolvesSecretRefs(t *testing.T) {
	registerSpyNotifier(t)
	store := &fakeNotifyStore{
		channels: []NotifierChannel{chan_("tg", "spy-plugin", `{"token_ref":"secret://tg/bot"}`, "success")},
		secrets:  map[string]*Secret{"secret://tg/bot": {Name: "tg/bot", Ciphertext: []byte("BOT-TOKEN")}},
	}

	spyConfigs = nil
	newNotifications(store, plainOpener{}).Send(context.Background(),
		&Task{Name: "t", Notifiers: []string{"tg"}}, plugin.EventSuccess, 1, "ok")

	if len(spyConfigs) != 1 {
		t.Fatalf("built %d channels, want 1", len(spyConfigs))
	}
	var cfg map[string]any
	if err := json.Unmarshal(spyConfigs[0], &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["token"] != "BOT-TOKEN" {
		t.Errorf("token = %v, want the resolved secret", cfg["token"])
	}
	if cfg["token_ref"] != "secret://tg/bot" {
		t.Errorf("token_ref should survive alongside the resolved value, got %v", cfg["token_ref"])
	}
}

// TestSendKeepsInlineValueOverRef: an explicitly configured value is not
// overwritten by a stale reference sitting next to it.
func TestSendKeepsInlineValueOverRef(t *testing.T) {
	registerSpyNotifier(t)
	store := &fakeNotifyStore{
		channels: []NotifierChannel{chan_("tg", "spy-plugin", `{"token":"INLINE","token_ref":"secret://tg/bot"}`, "success")},
		secrets:  map[string]*Secret{"secret://tg/bot": {Ciphertext: []byte("FROM-STORE")}},
	}

	spyConfigs = nil
	newNotifications(store, plainOpener{}).Send(context.Background(),
		&Task{Name: "t", Notifiers: []string{"tg"}}, plugin.EventSuccess, 1, "ok")

	var cfg map[string]any
	if err := json.Unmarshal(spyConfigs[0], &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["token"] != "INLINE" {
		t.Errorf("token = %v, want the inline value preserved", cfg["token"])
	}
}

// TestSendRecordsDeliveryLog: both outcomes land in the log, so an operator can
// answer "did it go out?" without reading daemon logs.
func TestSendRecordsDeliveryLog(t *testing.T) {
	registerSpyNotifier(t)
	spyErr = errors.New("chat not found")
	t.Cleanup(func() { spyErr = nil })

	store := &fakeNotifyStore{channels: []NotifierChannel{chan_("tg", "spy-plugin", `{}`, "failure")}}
	newNotifications(store, nil).Send(context.Background(),
		&Task{Name: "orders", Notifiers: []string{"tg"}}, plugin.EventFailure, 42, "boom")

	if len(store.recorded) != 1 {
		t.Fatalf("recorded = %+v, want one entry", store.recorded)
	}
	got := store.recorded[0]
	if got.Status != NotificationFailed || got.Error != "chat not found" {
		t.Errorf("entry = %+v, want a failed delivery carrying the cause", got)
	}
	if got.Channel != "tg" || got.Task != "orders" || got.RunID == nil || *got.RunID != 42 {
		t.Errorf("entry = %+v, want it attributed to the channel, task and run", got)
	}
}

// TestSendRecordsUnbuildableChannel: a channel whose config is broken must not
// fail silently — that was the whole problem with the previous nil-config
// construction.
func TestSendRecordsUnbuildableChannel(t *testing.T) {
	registerSpyNotifier(t)
	store := &fakeNotifyStore{
		channels: []NotifierChannel{chan_("broken", "spy-plugin", `{"fail":true}`, "success")},
	}
	newNotifications(store, nil).Send(context.Background(),
		&Task{Name: "t", Notifiers: []string{"broken"}}, plugin.EventSuccess, 0, "ok")

	if len(store.recorded) != 1 || store.recorded[0].Status != NotificationFailed {
		t.Fatalf("recorded = %+v, want a failed entry for the unbuildable channel", store.recorded)
	}
}

// TestSendIgnoresUnknownChannelName: a task referencing a deleted channel does
// not crash and does not deliver.
func TestSendIgnoresUnknownChannelName(t *testing.T) {
	registerSpyNotifier(t)
	store := &fakeNotifyStore{channels: []NotifierChannel{chan_("kept", "spy-plugin", `{}`, "success")}}

	spyEvents = nil
	newNotifications(store, nil).Send(context.Background(),
		&Task{Name: "t", Notifiers: []string{"deleted"}}, plugin.EventSuccess, 0, "ok")

	if len(spyEvents) != 0 {
		t.Errorf("events = %+v, want none", spyEvents)
	}
}

// TestSendRecordsWhenChannelListFails: resolving channels is a database read
// now, and a read that fails must not make the event vanish from every
// operator-facing surface at once.
func TestSendRecordsWhenChannelListFails(t *testing.T) {
	registerSpyNotifier(t)
	store := &fakeNotifyStore{listErr: errors.New("database is locked")}

	newNotifications(store, nil).Send(context.Background(),
		&Task{Name: "orders", Notifiers: []string{"tg", "log"}}, plugin.EventFailure, 7, "boom")

	if len(store.recorded) != 2 {
		t.Fatalf("recorded = %+v, want one failed entry per referenced channel", store.recorded)
	}
	for _, got := range store.recorded {
		if got.Status != NotificationFailed || !strings.Contains(got.Error, "database is locked") {
			t.Errorf("entry = %+v, want a failed delivery naming the cause", got)
		}
	}
}

// TestTestSurvivesCallerCancellation: the operator's browser closing mid-test
// must not take the delivery log entry with it — that entry is the only answer
// to "did it actually go out?".
func TestTestSurvivesCallerCancellation(t *testing.T) {
	registerSpyNotifier(t)
	ch := chan_("tg", "spy-plugin", `{}`, "success")
	store := &fakeNotifyStore{channels: []NotifierChannel{ch}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller is already gone

	spyEvents = nil
	if err := newNotifications(store, nil).Test(ctx, ch); err != nil {
		t.Fatalf("Test = %v, want the delivery to go ahead on a detached context", err)
	}
	if len(spyEvents) != 1 {
		t.Errorf("events = %+v, want the synthetic event delivered", spyEvents)
	}
	if len(store.recorded) != 1 || store.recorded[0].Status != NotificationSent {
		t.Errorf("recorded = %+v, want the test recorded as sent", store.recorded)
	}
}

// TestValidateRefusesUnbuildableChannel: the write path uses this to reject a
// config that could never deliver, instead of storing it and finding out in the
// log after a backup has already failed unnoticed.
func TestValidateRefusesUnbuildableChannel(t *testing.T) {
	registerSpyNotifier(t)
	n := newNotifications(&fakeNotifyStore{}, nil)

	if err := n.Validate(context.Background(), chan_("broken", "spy-plugin", `{"fail":true}`)); err == nil {
		t.Error("Validate = nil, want the plugin's own refusal")
	}
	if err := n.Validate(context.Background(), chan_("fine", "spy-plugin", `{}`)); err != nil {
		t.Errorf("Validate = %v, want a buildable channel accepted", err)
	}
}

// ---- spy plugin -----------------------------------------------------------

var (
	spyEvents  []plugin.Event
	spyConfigs []json.RawMessage
	spyErr     error
)

// registerSpyNotifier registers a plugin that records the config it was built
// with and the events it received. Registration is process-global, so it is
// guarded to run once.
func registerSpyNotifier(t *testing.T) {
	t.Helper()
	if plugin.Notifiers.Has("spy-plugin") {
		return
	}
	plugin.Notifiers.Register("spy-plugin", func(raw []byte) (plugin.Notifier, error) {
		var cfg map[string]any
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &cfg)
		}
		if fail, _ := cfg["fail"].(bool); fail {
			return nil, errors.New("spy-plugin: refusing to build")
		}
		spyConfigs = append(spyConfigs, append(json.RawMessage(nil), raw...))
		return spyNotifierPlugin{}, nil
	})
}

type spyNotifierPlugin struct{}

func (spyNotifierPlugin) Name() string { return "spy-plugin" }
func (spyNotifierPlugin) Notify(_ context.Context, ev plugin.Event) error {
	spyEvents = append(spyEvents, ev)
	return spyErr
}
