package core

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// ParseCron parses a standard 5-field cron expression ("min hour dom mon dow").
// Seconds are intentionally not supported (TZ §7 uses standard cron); use
// cron.ParseStandard so a stray 6th field is rejected rather than silently
// reinterpreted.
func ParseCron(expr string) (cron.Schedule, error) {
	sch, err := cron.ParseStandard(expr)
	if err != nil {
		return nil, fmt.Errorf("cron %q: %w", expr, err)
	}
	return sch, nil
}

// NextRun returns the first activation of expr strictly after `from`. It is the
// building block the dispatcher uses to decide whether a task is due.
func NextRun(expr string, from time.Time) (time.Time, error) {
	sch, err := ParseCron(expr)
	if err != nil {
		return time.Time{}, err
	}
	return sch.Next(from), nil
}
