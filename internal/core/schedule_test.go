package core

import (
	"testing"
	"time"
)

// TestNextRunDaily checks a daily "0 2 * * *" schedule advances to the next 02:00.
func TestNextRunDaily(t *testing.T) {
	from := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	got, err := NextRun("0 2 * * *", from)
	if err != nil {
		t.Fatalf("NextRun: %v", err)
	}
	want := time.Date(2026, 7, 23, 2, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("NextRun = %s, want %s", got, want)
	}

	// From just before 02:00 the same day, the next run is that day's 02:00.
	from2 := time.Date(2026, 7, 22, 1, 59, 0, 0, time.UTC)
	got2, err := NextRun("0 2 * * *", from2)
	if err != nil {
		t.Fatalf("NextRun: %v", err)
	}
	want2 := time.Date(2026, 7, 22, 2, 0, 0, 0, time.UTC)
	if !got2.Equal(want2) {
		t.Fatalf("NextRun = %s, want %s", got2, want2)
	}
}

// TestParseCronInvalid rejects garbage and a 6-field (seconds) expression.
func TestParseCronInvalid(t *testing.T) {
	for _, expr := range []string{"not a cron", "* * * *", "0 2 * * * *", ""} {
		if _, err := ParseCron(expr); err == nil {
			t.Errorf("ParseCron(%q) = nil error, want error", expr)
		}
	}
}
