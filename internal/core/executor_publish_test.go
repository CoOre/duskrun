package core_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/duskrun/duskrun/internal/core"
)

// recordPub is a core.Publisher that records everything it receives, for asserting
// the executor's live-progress emissions.
type recordPub struct {
	mu     sync.Mutex
	phases []core.Phase
	logs   []string
	bytes  []int64
	status core.RunStatus
	closed bool
}

func (p *recordPub) Phase(_ int64, ph core.Phase) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.phases = append(p.phases, ph)
}
func (p *recordPub) Log(_ int64, _ core.Phase, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logs = append(p.logs, msg)
}
func (p *recordPub) Bytes(_ int64, sent int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bytes = append(p.bytes, sent)
}
func (p *recordPub) Total(_ int64, _ int64) {}
func (p *recordPub) Status(_ int64, s core.RunStatus) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status = s
}
func (p *recordPub) Close(_ int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
}

// TestExecutorPublishesProgress asserts a successful run emits the phase sequence,
// at least one byte tick and log line, and a terminal success + Close.
func TestExecutorPublishesProgress(t *testing.T) {
	st, path := openStore(t)
	dir := t.TempDir()
	taskID := seedFakeTask(t, path, dir, "fakeexec")

	task, err := st.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	runID, err := st.StartManualRun(context.Background(), taskID)
	if err != nil {
		t.Fatalf("StartManualRun: %v", err)
	}

	pub := &recordPub{}
	exec := core.NewExecutor(st, nil, func() time.Time {
		return time.Date(2026, 7, 22, 2, 0, 0, 0, time.UTC)
	})
	exec.SetPublisher(pub)
	if _, err := exec.Run(context.Background(), task, runID); err != nil {
		t.Fatalf("Executor.Run: %v", err)
	}

	pub.mu.Lock()
	defer pub.mu.Unlock()
	if !containsPhases(pub.phases, core.PhaseResolve, core.PhaseStream, core.PhaseRecord) {
		t.Fatalf("phases = %v, want resolve→stream→record", pub.phases)
	}
	if len(pub.logs) == 0 {
		t.Fatal("no log lines published")
	}
	if len(pub.bytes) == 0 || pub.bytes[len(pub.bytes)-1] == 0 {
		t.Fatalf("byte ticks = %v, want a non-zero final total", pub.bytes)
	}
	if pub.status != core.StatusSuccess {
		t.Fatalf("terminal status = %q, want success", pub.status)
	}
	if !pub.closed {
		t.Fatal("Close not called on terminal run")
	}
}

// containsPhases checks want appears as an ordered (not necessarily contiguous)
// subsequence of got.
func containsPhases(got []core.Phase, want ...core.Phase) bool {
	i := 0
	for _, p := range got {
		if i < len(want) && p == want[i] {
			i++
		}
	}
	return i == len(want)
}
