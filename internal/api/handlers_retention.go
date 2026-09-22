package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/duskrun/duskrun/internal/core"
)

// maxSweepLimit caps GET /retention/sweeps?limit=. The history is a summary
// view; anything beyond a few hundred rows is a scan, not a page.
const maxSweepLimit = 500

// sweepDTO is the JSON shape for one retention sweep.
type sweepDTO struct {
	ID           int64     `json:"id"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	Status       string    `json:"status"`
	Source       string    `json:"source"`
	DeletedCount int       `json:"deleted_count"`
	FreedBytes   int64     `json:"freed_bytes"`
	OrphanCount  int       `json:"orphan_count"`
	Error        string    `json:"error,omitempty"`
}

func toSweepDTO(sw core.Sweep) sweepDTO {
	return sweepDTO{
		ID: sw.ID, StartedAt: sw.StartedAt, FinishedAt: sw.FinishedAt,
		Status: string(sw.Status), Source: string(sw.Source),
		DeletedCount: sw.DeletedCount, FreedBytes: sw.FreedBytes,
		OrphanCount: sw.OrphanCount, Error: sw.Error,
	}
}

// retentionTaskDTO is one row of the per-task policy table: what the task keeps
// and what it currently occupies in the catalog.
type retentionTaskDTO struct {
	TaskID    int64          `json:"task_id"`
	Name      string         `json:"name"`
	Enabled   bool           `json:"enabled"`
	Retention core.Retention `json:"retention"`
	Artifacts int            `json:"artifacts"`
	Bytes     int64          `json:"bytes"`
}

// retentionDTO is the retention page's summary payload.
type retentionDTO struct {
	// Cron is the configured sweep schedule; empty means the scheduled sweep is
	// disabled and retention only runs on demand.
	Cron string `json:"cron"`
	// NextSweep is the next activation of Cron in UTC, omitted when disabled.
	NextSweep *time.Time         `json:"next_sweep,omitempty"`
	LastSweep *sweepDTO          `json:"last_sweep,omitempty"`
	Tasks     []retentionTaskDTO `json:"tasks"`
}

// registerRetentionRoutes wires the retention endpoints.
func (s *server) registerRetentionRoutes(r chi.Router) {
	s.get(r, "/retention", core.RoleViewer, s.getRetention)
	s.get(r, "/retention/sweeps", core.RoleViewer, s.listSweeps)
	s.post(r, "/retention/sweep", core.RoleOperator, s.runSweep)
}

// getRetention returns the schedule, the last sweep, and the per-task policy
// table with each task's current catalog footprint.
func (s *server) getRetention(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	tasks, err := s.d.Store.ListTasks(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	stats, err := s.d.Store.ArtifactStatsByTask(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Only the newest sweep is needed here; the full history has its own route.
	sweeps, err := s.d.Store.ListSweeps(ctx, 1)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := retentionDTO{Cron: s.d.RetentionCron, Tasks: make([]retentionTaskDTO, 0, len(tasks))}
	if s.d.RetentionCron != "" {
		if next, err := core.NextRun(s.d.RetentionCron, time.Now()); err == nil {
			utc := next.UTC()
			out.NextSweep = &utc
		}
	}
	if len(sweeps) > 0 {
		dto := toSweepDTO(sweeps[0])
		out.LastSweep = &dto
	}
	for _, t := range tasks {
		st := stats[t.ID]
		out.Tasks = append(out.Tasks, retentionTaskDTO{
			TaskID: t.ID, Name: t.Name, Enabled: t.Enabled, Retention: t.Retention,
			Artifacts: st.Count, Bytes: st.Bytes,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// listSweeps returns the sweep history, newest first.
func (s *server) listSweeps(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		// Bounded on purpose: an unbounded limit makes the store materialise the
		// entire history and this handler allocate a DTO slice to match.
		if n > maxSweepLimit {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("limit must be at most %d", maxSweepLimit))
			return
		}
		limit = n
	}
	sweeps, err := s.d.Store.ListSweeps(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]sweepDTO, 0, len(sweeps))
	for _, sw := range sweeps {
		out = append(out, toSweepDTO(sw))
	}
	writeJSON(w, http.StatusOK, out)
}

// runSweep applies retention now and returns the resulting sweep record.
//
// It runs synchronously because the caller wants the outcome, but deliberately
// NOT on the request's context: a closed tab or a reverse-proxy read timeout
// would otherwise abort a healthy sweep half-way through the task list and
// record it as failed. Its own timeout bounds the work instead.
//
// A sweep that failed part-way still returns 200 with its record — status and
// error describe what happened, and the deletions in the same record are real
// work that did land.
func (s *server) runSweep(w http.ResponseWriter, r *http.Request) {
	if s.d.Sweeper == nil {
		writeError(w, http.StatusServiceUnavailable, "retention sweeper not configured")
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), core.ManualSweepTimeout)
	defer cancel()

	sw, ok, err := s.d.Sweeper.TrySweep(ctx, core.SweepManual)
	if !ok {
		// A scheduled sweep holds the lock. Blocking here would stall the request
		// for its full duration with nothing to show for it.
		writeError(w, http.StatusConflict, "a retention sweep is already running")
		return
	}
	if err != nil {
		s.d.Log.Error("api: manual retention sweep reported errors", "err", err)
	}
	// A sweep with no ID never reached the store — there is no record to return.
	if sw.ID == 0 {
		writeError(w, http.StatusInternalServerError, "sweep could not be recorded")
		return
	}
	writeJSON(w, http.StatusOK, toSweepDTO(sw))
}
