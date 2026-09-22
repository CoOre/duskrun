package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/store/sqlite"
)

// sseHeartbeat is how often a comment line is sent to keep proxies/browsers from
// dropping an idle stream.
// It is a var, not a const, only so tests can shorten it: the behaviour worth
// testing here is what happens across several heartbeats, and at 15s real time
// that is a test nobody runs.
var sseHeartbeat = 15 * time.Second

// streamRunEvents serves live run progress as Server-Sent Events. A terminal run
// is replayed from the store (its persisted log) and the stream closes. A live
// (queued/running) run is subscribed on the Hub: the current snapshot is flushed
// first, then subsequent events stream until the run reaches a terminal status.
//
// The endpoint stays under the token-guarded /api/* group; the frontend reads it
// with fetch + ReadableStream (EventSource can't send the Authorization header).
func (s *server) streamRunEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	run, err := s.d.Store.GetRun(r.Context(), id)
	if errors.Is(err, sqlite.ErrNotFound) {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering (nginx)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Terminal already → replay the persisted run and finish.
	if isTerminal(run.Status) {
		s.replayRun(w, flusher, run)
		return
	}

	// Live path: subscribe on the Hub. If the run isn't (or is no longer) live —
	// e.g. it finished in the race between GetRun and Subscribe, or no Hub is
	// wired — reload and replay whatever the store now holds.
	snapshot, ch, cancel, live := s.d.Hub.Subscribe(id)
	if !live {
		if fresh, ferr := s.d.Store.GetRun(r.Context(), id); ferr == nil {
			s.replayRun(w, flusher, fresh)
		}
		return
	}
	defer cancel()

	for _, ev := range snapshot {
		writeSSE(w, flusher, ev)
	}
	s.streamLive(r.Context(), w, flusher, ch, principalFrom(r.Context()))
}

// streamLive pumps events from ch to the client until the run finishes (the Hub
// closes ch after the terminal status) or the client disconnects. Heartbeats
// keep the connection alive during quiet stretches.
//
// Each heartbeat also revalidates the caller. A stream is authorised once, at
// accept, and then holds for as long as the run does; without this recheck an
// ended session or a disabled account would keep receiving live output — which
// is exactly what "access ends immediately" is supposed to prevent.
func (s *server) streamLive(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, ch <-chan core.ProgressEvent, p *principal) {
	ticker := time.NewTicker(sseHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return // Hub closed the stream: run reached a terminal status
			}
			writeSSE(w, flusher, ev)
		case <-ticker.C:
			if !s.sessionAlive(ctx, p) {
				return
			}
			_, _ = io.WriteString(w, ": keep-alive\n\n")
			flusher.Flush()
		}
	}
}

// replayRun emits a finished run's persisted log as log events followed by a
// final bytes event (artifact size, if any) and the terminal status, so the
// frontend renders a completed run through the same live component.
func (s *server) replayRun(w http.ResponseWriter, flusher http.Flusher, run *core.Run) {
	var seq int64
	next := func() int64 { seq++; return seq }
	phase := core.PhaseRecord
	if run.Log != "" {
		for _, line := range strings.Split(run.Log, "\n") {
			if line == "" {
				continue
			}
			writeSSE(w, flusher, core.ProgressEvent{
				RunID: run.ID, Seq: next(), Kind: core.EventLog, Phase: phase, Message: line, At: run.CreatedAt,
			})
		}
	}
	if art, err := s.d.Store.GetArtifactByRun(context.Background(), run.ID); err == nil && art.Size > 0 {
		writeSSE(w, flusher, core.ProgressEvent{
			RunID: run.ID, Seq: next(), Kind: core.EventBytes, Phase: phase, Bytes: art.Size, At: run.CreatedAt,
		})
	}
	writeSSE(w, flusher, core.ProgressEvent{
		RunID: run.ID, Seq: next(), Kind: core.EventStatus, Phase: phase, Status: run.Status, At: run.CreatedAt,
	})
}

// writeSSE serializes one event as a single SSE `data:` frame and flushes it.
func writeSSE(w http.ResponseWriter, flusher http.Flusher, ev core.ProgressEvent) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_, _ = io.WriteString(w, "data: ")
	_, _ = w.Write(b)
	_, _ = io.WriteString(w, "\n\n")
	flusher.Flush()
}

func isTerminal(status core.RunStatus) bool {
	switch status {
	case core.StatusSuccess, core.StatusFailed, core.StatusSkipped:
		return true
	default:
		return false
	}
}
