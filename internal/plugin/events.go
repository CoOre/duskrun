package plugin

import "strings"

const (
	EventSuccess        EventKind = "success"
	EventFailure        EventKind = "failure"
	EventRetentionError EventKind = "retention_error"
	EventWatchdog       EventKind = "watchdog" // v1

	// EventRecovered closes an episode one of the alerts opened: a task that
	// failed or went stale has a fresh success, a storage is reachable again or
	// has room again.
	EventRecovered EventKind = "recovered"
	// EventSizeAnomaly is a successful backup far smaller than the task's usual
	// size — the run that "worked" but dumped an empty or truncated database.
	EventSizeAnomaly EventKind = "size_anomaly"
	// EventSlowRun is a run still going well past the task's usual duration.
	EventSlowRun EventKind = "slow_run"
	// EventInterrupted is a run the daemon found unfinished at startup: the
	// process stopped mid-backup and the run was marked failed.
	EventInterrupted EventKind = "interrupted"
	// EventRetentionDone reports what a sweep deleted for a task. Optional:
	// routine, and off for new channels.
	EventRetentionDone EventKind = "retention_done"

	// System events have no task: they go to every channel subscribed to them.
	EventStorageLow         EventKind = "storage_low"
	EventStorageUnreachable EventKind = "storage_unreachable"
	EventSecurity           EventKind = "security"
)

// Severity says how loudly a channel should present an event.
type Severity int

const (
	SeverityInfo  Severity = iota // routine: a success, a recovery, a sweep report
	SeverityWarn                  // worth a look: may turn into a lost backup
	SeverityAlert                 // a backup is lost or something needs a human now
)

type kindInfo struct {
	kind     EventKind
	system   bool
	optional bool // off by default on a new channel
	severity Severity
	title    string // "%s" is the task or subject
}

// kinds is the closed list of event kinds, in the order they are presented.
var kinds = []kindInfo{
	{kind: EventSuccess, severity: SeverityInfo, title: "✓ %s: бэкап выполнен"},
	{kind: EventFailure, severity: SeverityAlert, title: "✗ %s: бэкап не выполнен"},
	{kind: EventRecovered, severity: SeverityInfo, title: "✓ %s: снова в порядке"},
	{kind: EventSizeAnomaly, severity: SeverityWarn, title: "⚠ %s: подозрительно маленький бэкап"},
	{kind: EventSlowRun, severity: SeverityWarn, title: "⏱ %s: бэкап идёт дольше обычного"},
	{kind: EventInterrupted, severity: SeverityAlert, title: "✗ %s: бэкап прерван перезапуском"},
	{kind: EventRetentionDone, optional: true, severity: SeverityInfo, title: "%s: удалены старые бэкапы"},
	{kind: EventRetentionError, severity: SeverityAlert, title: "⚠ %s: ошибка ротации"},
	{kind: EventWatchdog, severity: SeverityWarn, title: "⚠ %s: нет свежего бэкапа"},
	{kind: EventStorageLow, system: true, severity: SeverityWarn, title: "⚠ %s: заканчивается место"},
	{kind: EventStorageUnreachable, system: true, severity: SeverityAlert, title: "✗ %s: хранилище недоступно"},
	{kind: EventSecurity, system: true, severity: SeverityAlert, title: "🔒 %s: безопасность"},
}

func lookup(kind EventKind) (kindInfo, bool) {
	for _, k := range kinds {
		if k.kind == kind {
			return k, true
		}
	}
	return kindInfo{}, false
}

// EventKinds lists every kind a channel may subscribe to.
func EventKinds() []EventKind {
	out := make([]EventKind, len(kinds))
	for i, k := range kinds {
		out[i] = k.kind
	}
	return out
}

// DefaultEventKinds is what a new channel subscribes to when its creator does
// not say: everything except the optional routine reports.
func DefaultEventKinds() []EventKind {
	var out []EventKind
	for _, k := range kinds {
		if !k.optional {
			out = append(out, k.kind)
		}
	}
	return out
}

// ValidEventKind reports whether kind is one of EventKinds.
func ValidEventKind(kind string) bool {
	_, ok := lookup(EventKind(kind))
	return ok
}

// IsSystem reports whether kind is about the instance rather than a task.
func (k EventKind) IsSystem() bool {
	info, _ := lookup(k)
	return info.system
}

// Severity of an event kind; an unknown kind is treated as a warning.
func (k EventKind) Severity() Severity {
	info, ok := lookup(k)
	if !ok {
		return SeverityWarn
	}
	return info.severity
}

// IsTest reports whether ev is a channel test rather than a real event: a
// success always names its run, and only the test sends one without.
func (ev Event) IsTest() bool {
	return ev.Kind == EventSuccess && ev.RunID == 0
}

// About is what the event concerns: the task, or the subject of a system event.
func (ev Event) About() string {
	if ev.Task != "" {
		return ev.Task
	}
	if ev.Subject != "" {
		return ev.Subject
	}
	return "Duskrun"
}

// Title is the one-line human headline every channel leads with.
func Title(ev Event) string {
	if ev.IsTest() {
		return "Duskrun: тестовое уведомление"
	}
	info, ok := lookup(ev.Kind)
	if !ok {
		return "Duskrun: " + string(ev.Kind) + ": " + ev.About()
	}
	return strings.Replace(info.title, "%s", ev.About(), 1)
}
