package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/duskrun/duskrun/internal/plugin"
)

// ExecutorStore is the metadata-store surface the Executor needs to assemble and
// record a run. *sqlite.Store satisfies it; tests supply fakes or a real store.
type ExecutorStore interface {
	GetConnection(ctx context.Context, id int64) (*Connection, error)
	GetStorage(ctx context.Context, id int64) (*Storage, error)
	GetSecretByRef(ctx context.Context, ref string) (*Secret, error)
	InsertArtifact(ctx context.Context, a Artifact) (int64, error)
	Finish(ctx context.Context, runID int64, status RunStatus, log, errMsg string) error
}

// SecretOpener decrypts a stored Secret's value. *secret.Box implements it; it
// is an interface here because package secret imports core, so core cannot
// import secret without a cycle.
type SecretOpener interface {
	Open(s Secret) ([]byte, error)
}

// Executor is the single place that turns a Task into a completed run: resolve
// plugins + credentials, stream the pipeline, then record the artifact and
// terminal run status. Both the `duskrun run` CLI and the worker pool use it, so
// the assembly logic lives in exactly one place.
type Executor struct {
	store ExecutorStore
	res   *SecretResolver
	now   func() time.Time
	pub   Publisher
}

// NewExecutor builds an Executor. box may be nil only if no task uses a secret
// ref; now defaults to time.Now.
func NewExecutor(store ExecutorStore, box SecretOpener, now func() time.Time) *Executor {
	if now == nil {
		now = time.Now
	}
	return &Executor{store: store, res: NewSecretResolver(store, box), now: now}
}

// SetPublisher wires a live-progress sink (the daemon's Hub). Optional: without
// it, runs execute exactly as before and publish nothing. Set once at wiring
// time, before any Run.
func (e *Executor) SetPublisher(p Publisher) { e.pub = p }

// Outcome reports what a successful run produced.
type Outcome struct {
	Key         string
	Size        int64
	Checksum    string
	ArtifactID  int64
	RestoreHint string
}

// Run executes task's pipeline and records the result against runID. On success
// it inserts the artifact and marks the run success. On failure it marks the run
// failed and returns the error (so the caller — worker — can decide on retries).
// The run's terminal state is always recorded here; callers must not Finish again.
func (e *Executor) Run(ctx context.Context, task *Task, runID int64) (Outcome, error) {
	log := runLog{pub: e.pub, runID: runID}
	log.setPhase(PhaseResolve)
	log.add("run %d: task %q started", runID, task.Name)
	out, err := e.run(ctx, task, runID, &log)
	if err != nil {
		log.add("failed: %s", err)
		// Record the failure on a detached context: ctx may already be cancelled
		// (per-run timeout or shutdown), but the terminal status MUST still be
		// written or the run would be stuck 'running' until the next ReapStale.
		_ = e.store.Finish(context.WithoutCancel(ctx), runID, StatusFailed, log.String(), err.Error())
		e.publishTerminal(runID, StatusFailed)
		return Outcome{}, err
	}
	e.publishTerminal(runID, StatusSuccess)
	return out, nil
}

// publishTerminal broadcasts the run's final status and closes its live streams.
func (e *Executor) publishTerminal(runID int64, status RunStatus) {
	if e.pub == nil {
		return
	}
	e.pub.Status(runID, status)
	e.pub.Close(runID)
}

func (e *Executor) run(ctx context.Context, task *Task, runID int64, log *runLog) (Outcome, error) {
	conn, err := e.store.GetConnection(ctx, task.ConnectionID)
	if err != nil {
		return Outcome{}, fmt.Errorf("connection %d: %w", task.ConnectionID, err)
	}
	log.add("connection: %s (%s/%s)", conn.Name, conn.Engine, conn.ConnectorType)
	stor, err := e.store.GetStorage(ctx, task.StorageID)
	if err != nil {
		return Outcome{}, fmt.Errorf("storage %d: %w", task.StorageID, err)
	}
	log.add("storage: %s (%s)", stor.Name, stor.Type)

	// Every plugin config goes through the same resolver: a `<key>_ref` is
	// substituted with the decrypted secret before the plugin sees it, so no
	// private key or access key has to be stored next to the host it belongs to.
	connectorConfig, err := e.res.Resolve(ctx, conn.ConnectorConfig)
	if err != nil {
		return Outcome{}, fmt.Errorf("connector config: %w", err)
	}
	connector, err := plugin.Connectors.Create(conn.ConnectorType, connectorConfig)
	if err != nil {
		return Outcome{}, err
	}
	dumperOpts, err := e.res.Resolve(ctx, task.DumperOpts)
	if err != nil {
		return Outcome{}, fmt.Errorf("dumper options: %w", err)
	}
	dumper, err := plugin.Dumpers.Create(conn.Engine, dumperOpts)
	if err != nil {
		return Outcome{}, err
	}
	storageConfig, err := e.res.Resolve(ctx, stor.Config)
	if err != nil {
		return Outcome{}, fmt.Errorf("storage config: %w", err)
	}
	storage, err := plugin.Storages.Create(stor.Type, storageConfig)
	if err != nil {
		return Outcome{}, err
	}
	defer CloseStorage(storage)
	codecs := make([]plugin.Codec, 0, len(task.CodecChain))
	for _, name := range task.CodecChain {
		c, err := plugin.Codecs.Create(name, nil)
		if err != nil {
			return Outcome{}, err
		}
		codecs = append(codecs, c)
	}
	if len(codecs) == 0 {
		log.add("codecs: none")
	} else {
		log.add("codecs: %s", strings.Join(task.CodecChain, " -> "))
	}

	creds, err := e.resolveCreds(ctx, conn.SecretRef)
	if err != nil {
		return Outcome{}, err
	}
	// The connection carries the login user; the secret carries only the password.
	// A username set in the task's dumper_opts still overrides this (buildArgs).
	creds.Username = conn.Username
	if conn.SecretRef != "" {
		log.add("credentials: resolved %s", conn.SecretRef)
	}

	// The dumper options the pipeline runs on are the RESOLVED ones: every
	// registered dumper ignores its factory config and reads its knobs from the
	// DumpOptions handed to Dump, so a `<key>_ref` unmarshalled from the raw
	// task.DumperOpts would reach the dumper as the literal reference string.
	var opts plugin.DumpOptions
	if len(dumperOpts) > 0 {
		if err := json.Unmarshal(dumperOpts, &opts); err != nil {
			return Outcome{}, fmt.Errorf("dumper opts: %w", err)
		}
	}
	// The restore hint is built from the unresolved options instead: it is text
	// shown in the UI, and a decrypted secret has no business in it.
	var hintOpts plugin.DumpOptions
	if len(task.DumperOpts) > 0 {
		if err := json.Unmarshal(task.DumperOpts, &hintOpts); err != nil {
			return Outcome{}, fmt.Errorf("dumper opts: %w", err)
		}
	}
	db, _ := opts["database"].(string)
	format, _ := opts["format"].(string)
	if all, _ := opts["all_databases"].(bool); all {
		// Whole-server dump: there is no single database, so label the key "all".
		db = "all"
		if conn.Engine == "postgres" {
			// pg_dumpall emits plain SQL → the artifact carries the .sql extension.
			format = "plain"
		}
	}
	if db == "" {
		// Engines that dump the whole instance by nature (redis always, mongodb
		// with no database selected) leave no name for the key segment, and an
		// empty one would produce "task//20260813_0200_.rdb".
		db = "all"
	}
	key := BuildArtifactKey(task.Name, db, conn.Engine, format, codecs, e.now())
	log.add("artifact key: %s", key)
	// Seed the live progress bar with the previous successful run's artifact size:
	// a same-codec, compressed-vs-compressed estimate of this run's total. Optional
	// (store may not implement it, or there may be no prior run) → indeterminate bar.
	if e.pub != nil {
		if sz, ok := e.store.(interface {
			LastSuccessfulArtifactSize(context.Context, int64) (int64, error)
		}); ok {
			if total, terr := sz.LastSuccessfulArtifactSize(ctx, task.ID); terr == nil && total > 0 {
				e.pub.Total(runID, total)
			}
		}
	}
	log.setPhase(PhaseStream)
	log.add("pipeline: dump -> codecs -> storage")

	res, err := RunPipeline(ctx, PipelineInput{
		Connector: connector, Dumper: dumper, Codecs: codecs, Storage: storage,
		Creds: creds, DumpOpts: opts, Key: key,
		OnBytes: e.bytesHook(runID),
	})
	if err != nil {
		return Outcome{}, err
	}
	log.setPhase(PhaseRecord)
	log.add("pipeline complete: %d bytes sha256:%s", res.Size, res.Checksum)

	// Record success on a detached context too: the pipeline already succeeded,
	// so a late cancellation must not lose the artifact/terminal-status writes.
	rec := context.WithoutCancel(ctx)
	artID, err := e.store.InsertArtifact(rec, Artifact{
		RunID: runID, StorageID: stor.ID, Key: res.Key, Size: res.Size, Checksum: res.Checksum,
	})
	if err != nil {
		return Outcome{}, fmt.Errorf("insert artifact: %w", err)
	}
	log.add("artifact recorded: id=%d", artID)
	log.add("success")
	if err := e.store.Finish(rec, runID, StatusSuccess, log.String(), ""); err != nil {
		return Outcome{}, fmt.Errorf("finish run: %w", err)
	}
	return Outcome{
		Key: res.Key, Size: res.Size, Checksum: res.Checksum,
		ArtifactID: artID, RestoreHint: dumper.RestoreHint(hintOpts),
	}, nil
}

// bytesHook returns the pipeline's OnBytes callback, forwarding cumulative bytes
// to the publisher. Returns nil when no publisher is wired (no live viewers).
func (e *Executor) bytesHook(runID int64) func(int64) {
	if e.pub == nil {
		return nil
	}
	return func(total int64) { e.pub.Bytes(runID, total) }
}

// runLog accumulates the human log persisted at Finish, and (when a publisher is
// wired) mirrors each line and phase transition to the live Hub.
type runLog struct {
	lines []string
	pub   Publisher
	runID int64
	phase Phase
}

// setPhase records the current phase for subsequent log lines and broadcasts it.
func (l *runLog) setPhase(p Phase) {
	l.phase = p
	if l.pub != nil {
		l.pub.Phase(l.runID, p)
	}
}

func (l *runLog) add(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	l.lines = append(l.lines, time.Now().UTC().Format(time.RFC3339)+" "+msg)
	if l.pub != nil {
		l.pub.Log(l.runID, l.phase, msg)
	}
}

func (l *runLog) String() string {
	return strings.Join(l.lines, "\n")
}

// resolveCreds decrypts the connection's secret (if any) into Credentials.
func (e *Executor) resolveCreds(ctx context.Context, ref string) (plugin.Credentials, error) {
	if ref == "" {
		return plugin.Credentials{}, nil // no secret → rely on peer/trust auth
	}
	pw, err := e.res.Open(ctx, ref)
	if err != nil {
		return plugin.Credentials{}, err
	}
	return plugin.Credentials{Password: string(pw)}, nil
}
