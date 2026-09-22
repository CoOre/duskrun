package core

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/duskrun/duskrun/internal/plugin"
)

// NotifyStore is the store surface delivery needs.
type NotifyStore interface {
	ListNotifiers(ctx context.Context) ([]NotifierChannel, error)
	GetSecretByRef(ctx context.Context, ref string) (*Secret, error)
	InsertNotification(ctx context.Context, n Notification) (int64, error)
}

// Notifications delivers events to a task's configured channels. It is the one
// place that knows how a channel is built, so the worker, the retention sweeper
// and the watchdog all deliver the same way.
type Notifications struct {
	store   NotifyStore
	res     *SecretResolver
	now     func() time.Time
	log     *slog.Logger
	resolve func(ctx context.Context, task *Task, kind plugin.EventKind) []namedNotifier
}

// namedNotifier pairs a constructed channel with the configured name it came
// from, so a delivery failure names the channel an operator configured rather
// than the plugin type shared by several of them.
type namedNotifier struct {
	name     string
	notifier plugin.Notifier
}

// NotificationsConfig configures delivery. Zero values fall back to defaults.
type NotificationsConfig struct {
	Now func() time.Time
	// Resolve overrides channel construction. Tests inject fakes through it;
	// production leaves it nil and resolves from the store.
	Resolve func(ctx context.Context, task *Task, kind plugin.EventKind) []plugin.Notifier
}

// NewNotifications builds the delivery service. box may be nil, in which case
// channels whose config needs a secret cannot be built and are reported.
func NewNotifications(store NotifyStore, box SecretOpener, cfg NotificationsConfig, log *slog.Logger) *Notifications {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	n := &Notifications{store: store, res: NewSecretResolver(store, box), now: cfg.Now, log: log}
	if cfg.Resolve != nil {
		n.resolve = func(ctx context.Context, task *Task, kind plugin.EventKind) []namedNotifier {
			out := []namedNotifier{}
			for _, ch := range cfg.Resolve(ctx, task, kind) {
				out = append(out, namedNotifier{name: ch.Name(), notifier: ch})
			}
			return out
		}
	} else {
		n.resolve = n.channelsFor
	}
	return n
}

// Send delivers one event to every channel the task subscribes to that also
// forwards this kind. Delivery problems are logged and recorded, never
// propagated: the backup (or sweep, or watchdog finding) stands on its own
// regardless of whether a chat message got through.
func (n *Notifications) Send(ctx context.Context, task *Task, kind plugin.EventKind, runID int64, msg string) {
	if n == nil || task == nil {
		return
	}
	// Detached before resolving, not after: channel construction also writes to
	// the delivery log, and a run cancelled by timeout would otherwise lose the
	// very entry explaining that nothing was delivered.
	dctx := context.WithoutCancel(ctx)

	channels := n.resolve(dctx, task, kind)
	if len(channels) == 0 {
		return
	}

	ev := plugin.Event{Kind: kind, Task: task.Name, RunID: runID, Message: msg, At: n.now()}
	for _, ch := range channels {
		err := ch.notifier.Notify(dctx, ev)
		if err != nil {
			n.log.Error("notify failed", "channel", ch.name, "task", task.Name, "kind", string(kind), "err", err)
		}
		n.record(dctx, ev, ch.name, err)
	}
}

// record appends the delivery attempt to the notification log.
func (n *Notifications) record(ctx context.Context, ev plugin.Event, channel string, cause error) {
	if n.store == nil {
		return
	}
	entry := Notification{
		Kind: string(ev.Kind), Task: ev.Task, Channel: channel,
		Status: NotificationSent, CreatedAt: ev.At,
	}
	if ev.RunID != 0 {
		runID := ev.RunID
		entry.RunID = &runID
	}
	if cause != nil {
		entry.Status = NotificationFailed
		entry.Error = cause.Error()
	}
	if _, err := n.store.InsertNotification(ctx, entry); err != nil {
		n.log.Error("notify: record delivery failed", "channel", channel, "err", err)
	}
}

// channelsFor builds the task's channels that forward this event kind.
//
// A name the task references but the channel list does not contain is reported:
// it means someone deleted or renamed a channel and the task has been quietly
// delivering to nothing ever since.
func (n *Notifications) channelsFor(ctx context.Context, task *Task, kind plugin.EventKind) []namedNotifier {
	if len(task.Notifiers) == 0 || n.store == nil {
		return nil
	}
	configured, err := n.store.ListNotifiers(ctx)
	if err != nil {
		n.log.Error("notify: list channels failed", "task", task.Name, "err", err)
		// Resolution is a database read, and a read that fails must not make the
		// event disappear: record one failed attempt per referenced channel so the
		// delivery log shows the gap instead of an alert nobody was ever told
		// about.
		cause := fmt.Errorf("channel list unavailable: %w", err)
		for _, name := range task.Notifiers {
			n.record(ctx, plugin.Event{Kind: kind, Task: task.Name, At: n.now()}, name, cause)
		}
		return nil
	}
	byName := make(map[string]NotifierChannel, len(configured))
	for _, c := range configured {
		byName[c.Name] = c
	}

	var out []namedNotifier
	for _, name := range task.Notifiers {
		ch, ok := byName[name]
		if !ok {
			n.log.Warn("notify: task references an unknown channel", "task", task.Name, "channel", name)
			continue
		}
		if !ch.Wants(string(kind)) {
			continue // disabled, or not subscribed to this kind
		}
		built, err := n.build(ctx, ch)
		if err != nil {
			n.log.Error("notify: channel unavailable, event not delivered",
				"channel", ch.Name, "type", ch.Type, "task", task.Name, "err", err)
			n.record(ctx, plugin.Event{Kind: kind, Task: task.Name, At: n.now()}, ch.Name, err)
			continue
		}
		out = append(out, namedNotifier{name: ch.Name, notifier: built})
	}
	return out
}

// Test delivers a synthetic event through one channel, along the same path a
// real event takes: same construction, same secret resolution, same delivery
// log. Recording it matters — a successful test that left no trace would leave
// the notification page contradicting the result it just showed.
func (n *Notifications) Test(ctx context.Context, ch NotifierChannel) error {
	// Detached like Send, and for the same reason: a browser tab closed mid-test
	// would otherwise abort the conversation in flight and then lose the log
	// entry saying so. TestTimeout keeps that detachment finite.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), TestTimeout)
	defer cancel()

	ev := plugin.Event{
		Kind:    plugin.EventSuccess,
		Task:    testEventTask,
		Message: "test notification from duskrun",
		At:      n.now(),
	}
	built, err := n.build(dctx, ch)
	if err == nil {
		err = built.Notify(dctx, ev)
	}
	n.record(dctx, ev, ch.Name, err)
	return err
}

// TestTimeout bounds a channel test. The test no longer ends when the caller
// walks away, so the deadline has to come from somewhere: without it a channel
// plugin without a timeout of its own could hold the request open indefinitely.
const TestTimeout = 30 * time.Second

// Validate builds the channel exactly as a delivery would — resolving secret
// references, then running the plugin's own constructor — without sending
// anything. It is what lets a config that can never work be refused when it is
// written, instead of surfacing in the delivery log after a backup has already
// failed with nobody notified.
func (n *Notifications) Validate(ctx context.Context, ch NotifierChannel) error {
	_, err := n.build(ctx, ch)
	return err
}

// testEventTask labels a delivery the operator triggered by hand, so the log
// distinguishes it from an event a real run produced.
const testEventTask = "(проверка канала)"

func (n *Notifications) build(ctx context.Context, ch NotifierChannel) (plugin.Notifier, error) {
	cfg, err := n.res.Resolve(ctx, ch.Config)
	if err != nil {
		return nil, err
	}
	return plugin.Notifiers.Create(ch.Type, cfg)
}
