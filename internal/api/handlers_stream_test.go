package api

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/duskrun/duskrun/internal/core"
)

// readSSE reads all `data:` frames from an SSE response body into ProgressEvents.
func readSSE(t *testing.T, resp *http.Response) []core.ProgressEvent {
	t.Helper()
	defer resp.Body.Close()
	var out []core.ProgressEvent
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev core.ProgressEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("bad SSE frame %q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

// TestStreamRunTerminalReplays checks a finished run replays its persisted log as
// log events followed by a terminal status, then the stream closes.
func TestStreamRunTerminalReplays(t *testing.T) {
	srv, db, st := seededServer(t)
	srv.Close() // rebuild with a Hub-carrying Deps
	s := httptest.NewServer(NewRouter(Deps{Store: st, Token: testToken, Hub: core.NewHub(nil)}))
	t.Cleanup(s.Close)

	mustExec(t, db, "INSERT INTO run (task_id, status, attempt, log, created_at) VALUES (1, 'success', 1, 'line one\nline two', unixepoch())")

	resp := authGet(t, s.URL+"/api/runs/1/events")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	events := readSSE(t, resp)

	var logs int
	var terminal *core.ProgressEvent
	for i := range events {
		switch events[i].Kind {
		case core.EventLog:
			logs++
		case core.EventStatus:
			terminal = &events[i]
		}
	}
	if logs != 2 {
		t.Fatalf("log events = %d, want 2", logs)
	}
	if terminal == nil || terminal.Status != core.StatusSuccess {
		t.Fatalf("terminal event = %+v, want status=success", terminal)
	}
}

// TestStreamRunLive subscribes to a running run and receives the live snapshot
// (phase/log/bytes/status), ending when the Hub closes the run.
func TestStreamRunLive(t *testing.T) {
	srv, db, st := seededServer(t)
	srv.Close()
	hub := core.NewHub(nil)
	s := httptest.NewServer(NewRouter(Deps{Store: st, Token: testToken, Hub: hub}))
	t.Cleanup(s.Close)

	mustExec(t, db, "INSERT INTO run (task_id, status, worker, attempt, started_at, created_at) VALUES (1, 'running', 'w0', 1, unixepoch(), unixepoch())")

	// Populate the live run before the request so the snapshot is deterministic.
	hub.Phase(1, core.PhaseStream)
	hub.Log(1, core.PhaseStream, "streaming dump")
	hub.Bytes(1, 4096)
	hub.Status(1, core.StatusSuccess)

	// End the stream shortly after the handler subscribes.
	go func() {
		time.Sleep(60 * time.Millisecond)
		hub.Close(1)
	}()

	resp := authGet(t, s.URL+"/api/runs/1/events")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	events := readSSE(t, resp)

	var sawLog, sawBytes, sawStatus bool
	for _, ev := range events {
		switch ev.Kind {
		case core.EventLog:
			if strings.Contains(ev.Message, "streaming dump") {
				sawLog = true
			}
		case core.EventBytes:
			if ev.Bytes == 4096 {
				sawBytes = true
			}
		case core.EventStatus:
			if ev.Status == core.StatusSuccess {
				sawStatus = true
			}
		}
	}
	if !sawLog || !sawBytes || !sawStatus {
		t.Fatalf("live events incomplete: log=%v bytes=%v status=%v (%d events)", sawLog, sawBytes, sawStatus, len(events))
	}
}
