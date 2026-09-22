package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/duskrun/duskrun/internal/core"
)

// watchdogAlertDTO is one stale task, as the dashboard renders it.
type watchdogAlertDTO struct {
	TaskID int64  `json:"task_id"`
	Task   string `json:"task"`
	// ThresholdSec is the resolved threshold — already derived from the cron
	// when the task leaves it on auto, so the UI never re-derives it.
	ThresholdSec int64      `json:"threshold_sec"`
	SinceSec     int64      `json:"since_sec"`
	LastSuccess  *time.Time `json:"last_success,omitempty"`
	Reason       string     `json:"reason"`
}

// registerWatchdogRoutes wires the watchdog endpoint.
func (s *server) registerWatchdogRoutes(r chi.Router) {
	s.get(r, "/watchdog", core.RoleViewer, s.listWatchdogAlerts)
}

// listWatchdogAlerts returns the tasks with no recent successful backup.
// Evaluated on read rather than replayed from the alert table, so the answer is
// current even between watchdog ticks.
func (s *server) listWatchdogAlerts(w http.ResponseWriter, r *http.Request) {
	if s.d.Watchdog == nil {
		writeError(w, http.StatusServiceUnavailable, "watchdog not configured")
		return
	}
	alerts, err := s.d.Watchdog.Alerts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]watchdogAlertDTO, 0, len(alerts))
	for _, a := range alerts {
		out = append(out, watchdogAlertDTO{
			TaskID: a.TaskID, Task: a.Task,
			ThresholdSec: int64(a.Threshold / time.Second),
			SinceSec:     int64(a.Since / time.Second),
			LastSuccess:  a.LastSuccess,
			Reason:       a.Reason(),
		})
	}
	writeJSON(w, http.StatusOK, out)
}
