package plugin

import (
	"slices"
	"testing"
)

func TestTitle(t *testing.T) {
	for _, tc := range []struct {
		ev   Event
		want string
	}{
		{Event{Kind: EventFailure, Task: "pg", RunID: 3}, "✗ pg: бэкап не выполнен"},
		{Event{Kind: EventSuccess, Task: "pg", RunID: 3}, "✓ pg: бэкап выполнен"},
		{Event{Kind: EventSuccess, Task: "(проверка канала)"}, "Duskrun: тестовое уведомление"},
		{Event{Kind: EventStorageLow, Subject: "nas"}, "⚠ nas: заканчивается место"},
		{Event{Kind: EventRecovered, Subject: "nas"}, "✓ nas: снова в порядке"},
		{Event{Kind: "future_kind", Task: "pg"}, "Duskrun: future_kind: pg"},
	} {
		if got := Title(tc.ev); got != tc.want {
			t.Errorf("Title(%+v) = %q, want %q", tc.ev, got, tc.want)
		}
	}
}

func TestEventKindCatalogue(t *testing.T) {
	all := EventKinds()
	def := DefaultEventKinds()
	if slices.Contains(def, EventRetentionDone) {
		t.Error("the optional sweep report is on by default")
	}
	if !slices.Contains(def, EventStorageLow) || !slices.Contains(def, EventSecurity) {
		t.Error("an alert is off by default")
	}
	if len(def) != len(all)-1 {
		t.Errorf("defaults = %v, want all but retention_done", def)
	}
	if !EventStorageUnreachable.IsSystem() || EventFailure.IsSystem() {
		t.Error("system/task split is wrong")
	}
	if EventFailure.Severity() != SeverityAlert || EventRecovered.Severity() != SeverityInfo {
		t.Error("severity is wrong")
	}
	if !ValidEventKind("slow_run") || ValidEventKind("nope") {
		t.Error("ValidEventKind is wrong")
	}
}
