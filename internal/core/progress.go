package core

import (
	"sync"
	"time"
)

// Phase is a coarse stage of a run's pipeline, surfaced live to the UI. The
// pipeline itself is a single fused stream (dump→codecs→storage), so these are
// milestones around it, not sequential sub-steps.
type Phase string

const (
	PhaseQueued  Phase = "queued"  // enqueued, before the worker starts
	PhaseResolve Phase = "resolve" // loading connection/storage/plugins/credentials
	PhaseStream  Phase = "stream"  // dump → codecs → storage streaming
	PhaseRecord  Phase = "record"  // inserting the artifact + finishing the run
)

// Event kinds carried by ProgressEvent.Kind.
const (
	EventLog    = "log"    // a log line (Message, with Phase context)
	EventPhase  = "phase"  // a phase transition (Phase)
	EventBytes  = "bytes"  // cumulative bytes streamed + throughput (+ Total if known)
	EventTotal  = "total"  // the estimated total for the progress bar became known
	EventStatus = "status" // terminal status reached (Status)
)

// ProgressEvent is one live update about a running (or replayed) run. It is the
// wire shape streamed over SSE; JSON tags mirror the frontend RunProgressEvent.
type ProgressEvent struct {
	RunID         int64     `json:"run_id"`
	Seq           int64     `json:"seq"`
	Kind          string    `json:"kind"`
	Phase         Phase     `json:"phase,omitempty"`
	Message       string    `json:"message,omitempty"`
	Bytes         int64     `json:"bytes,omitempty"`
	Total         int64     `json:"total,omitempty"` // estimated total (prev run's artifact size); 0 = unknown
	ThroughputBps float64   `json:"throughput_bps,omitempty"`
	Status        RunStatus `json:"status,omitempty"`
	At            time.Time `json:"at"`
}

// Publisher receives live progress from a running Executor. *Hub implements it;
// the Executor holds one only when live streaming is wired (serve), so every
// call site tolerates a nil Publisher.
type Publisher interface {
	Phase(runID int64, p Phase)
	Log(runID int64, phase Phase, msg string)
	Bytes(runID int64, sent int64)
	Total(runID int64, total int64)
	Status(runID int64, status RunStatus)
	Close(runID int64)
}

const (
	// tailMax bounds the per-run snapshot ring buffer handed to late subscribers.
	tailMax = 256
	// subBuf sizes each subscriber channel; overflow drops live events (the
	// snapshot still carries recent history, so a briefly-slow client recovers).
	subBuf = 256
)

// Hub is the in-memory registry of live runs. The Executor publishes into it and
// the SSE handler subscribes. It holds nothing durable: a run's live state lives
// here only between start and Finish; the full log is persisted separately.
type Hub struct {
	mu   sync.Mutex
	runs map[int64]*liveRun
	now  func() time.Time
}

type liveRun struct {
	phase      Phase
	bytes      int64
	total      int64
	seq        int64
	tail       []ProgressEvent
	subs       map[int]chan ProgressEvent
	nextSub    int
	lastBytes  int64
	lastByteAt time.Time
}

// NewHub builds an empty Hub. now defaults to time.Now (used for throughput and
// event timestamps).
func NewHub(now func() time.Time) *Hub {
	if now == nil {
		now = time.Now
	}
	return &Hub{runs: map[int64]*liveRun{}, now: now}
}

// ensure returns the liveRun for runID, creating it on first use. Caller holds mu.
func (h *Hub) ensure(runID int64) *liveRun {
	lr := h.runs[runID]
	if lr == nil {
		lr = &liveRun{phase: PhaseQueued, subs: map[int]chan ProgressEvent{}}
		h.runs[runID] = lr
	}
	return lr
}

// publishLocked stamps a sequence number, appends to the ring buffer, and
// fans out to subscribers without blocking. Caller holds mu.
func (h *Hub) publishLocked(lr *liveRun, ev ProgressEvent) {
	lr.seq++
	ev.Seq = lr.seq
	lr.tail = append(lr.tail, ev)
	if len(lr.tail) > tailMax {
		lr.tail = lr.tail[len(lr.tail)-tailMax:]
	}
	for _, ch := range lr.subs {
		select {
		case ch <- ev:
		default: // slow subscriber — drop this live event
		}
	}
}

// Phase records a phase transition and broadcasts it.
func (h *Hub) Phase(runID int64, p Phase) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	lr := h.ensure(runID)
	lr.phase = p
	h.publishLocked(lr, ProgressEvent{RunID: runID, Kind: EventPhase, Phase: p, At: h.now()})
}

// Log broadcasts a log line tagged with the current phase.
func (h *Hub) Log(runID int64, phase Phase, msg string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	lr := h.ensure(runID)
	h.publishLocked(lr, ProgressEvent{RunID: runID, Kind: EventLog, Phase: phase, Message: msg, At: h.now()})
}

// Total records the estimated total (previous run's artifact size) and broadcasts
// it so subscribers can switch to a determinate progress bar.
func (h *Hub) Total(runID int64, total int64) {
	if h == nil || total <= 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	lr := h.ensure(runID)
	lr.total = total
	h.publishLocked(lr, ProgressEvent{RunID: runID, Kind: EventTotal, Phase: lr.phase, Total: total, At: h.now()})
}

// Bytes broadcasts the cumulative bytes streamed so far plus an instantaneous
// throughput derived from the delta since the previous Bytes call, and the
// estimated total (if known) so the UI can render a percentage.
func (h *Hub) Bytes(runID int64, sent int64) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	lr := h.ensure(runID)
	now := h.now()
	var bps float64
	if !lr.lastByteAt.IsZero() {
		if dt := now.Sub(lr.lastByteAt).Seconds(); dt > 0 {
			bps = float64(sent-lr.lastBytes) / dt
		}
	}
	lr.bytes = sent
	lr.lastBytes = sent
	lr.lastByteAt = now
	h.publishLocked(lr, ProgressEvent{RunID: runID, Kind: EventBytes, Phase: lr.phase, Bytes: sent, Total: lr.total, ThroughputBps: bps, At: now})
}

// Status broadcasts the terminal status. Follow with Close to end the streams.
func (h *Hub) Status(runID int64, status RunStatus) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	lr := h.ensure(runID)
	h.publishLocked(lr, ProgressEvent{RunID: runID, Kind: EventStatus, Phase: lr.phase, Status: status, At: h.now()})
}

// Close ends every subscriber stream for runID and forgets the run. Subscribers
// ranging over their channel see it close and finish; new subscribers get ok=false.
func (h *Hub) Close(runID int64) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	lr := h.runs[runID]
	if lr == nil {
		return
	}
	for _, ch := range lr.subs {
		close(ch)
	}
	delete(h.runs, runID)
}

// Subscribe attaches a listener to a live run. It returns the current snapshot
// (recent events for immediate render), a channel of subsequent events, a cancel
// to detach, and ok=false if the run is not (or no longer) live — in which case
// the caller should replay the persisted run instead.
func (h *Hub) Subscribe(runID int64) (snapshot []ProgressEvent, ch <-chan ProgressEvent, cancel func(), ok bool) {
	if h == nil {
		return nil, nil, nil, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	lr := h.runs[runID]
	if lr == nil {
		return nil, nil, nil, false
	}
	c := make(chan ProgressEvent, subBuf)
	id := lr.nextSub
	lr.nextSub++
	lr.subs[id] = c
	snap := append([]ProgressEvent(nil), lr.tail...)
	cancel = func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		// The run may already be Closed (subs cleared); guard the delete.
		if cur := h.runs[runID]; cur == lr {
			delete(lr.subs, id)
		}
	}
	return snap, c, cancel, true
}
