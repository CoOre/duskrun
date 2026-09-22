package core_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/plugin"

	_ "github.com/duskrun/duskrun/internal/codec/zstd"      // register "zstd"
	_ "github.com/duskrun/duskrun/internal/connector/local" // register "direct"
	_ "github.com/duskrun/duskrun/internal/storage/localfs" // register "localfs"
)

// --- fake dumpers used across core_test (executor + worker) ---

func init() {
	plugin.Dumpers.Register("fakeexec", func(_ []byte) (plugin.Dumper, error) {
		return fakeDumper{data: []byte("fake dump payload — duskrun executor test\n")}, nil
	})
}

// fakeDumper streams fixed bytes, or errors, or blocks until ctx is done.
type fakeDumper struct {
	data  []byte
	fail  error         // if set, Dump returns a reader that errors after data
	block time.Duration // if >0, Dump blocks (respecting ctx) before returning
}

func (fakeDumper) Mode() plugin.DumpMode { return plugin.ModeStream }
func (fakeDumper) DumpStaged(context.Context, plugin.Endpoint, plugin.Credentials, plugin.DumpOptions) (plugin.RemotePath, plugin.Fetcher, error) {
	return plugin.RemotePath{}, nil, plugin.ErrModeUnsupported
}
func (fakeDumper) RestoreHint(plugin.DumpOptions) string { return "fake-restore <file>" }
func (f fakeDumper) Dump(ctx context.Context, _ plugin.Endpoint, _ plugin.Credentials, _ plugin.DumpOptions) (io.ReadCloser, error) {
	if f.block > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(f.block):
		}
	}
	if f.fail != nil {
		return io.NopCloser(&failReader{data: f.data, err: f.fail}), nil
	}
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

type failReader struct {
	data []byte
	off  int
	err  error
}

func (r *failReader) Read(p []byte) (int, error) {
	if r.off < len(r.data) {
		n := copy(p, r.data[r.off:])
		r.off += n
		return n, nil
	}
	return 0, r.err
}

// seedFakeTask inserts a connection(engine=engine, direct)+localfs(root=dir)+task
// graph via a raw handle and returns the task id. codec chain is ["zstd"].
func seedFakeTask(t *testing.T, path, dir, engine string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	cr, err := db.ExecContext(ctx,
		`INSERT INTO connection (name, engine, connector_type, connector_config, created_at)
		 VALUES ('fc', ?, 'direct', '{"host":"127.0.0.1","port":5432}', unixepoch())`, engine,
	)
	if err != nil {
		t.Fatal(err)
	}
	connID, _ := cr.LastInsertId()
	sr, err := db.ExecContext(ctx,
		`INSERT INTO storage (name, type, config, created_at)
		 VALUES ('fs', 'localfs', ?, unixepoch())`, `{"root":"`+dir+`"}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	storID, _ := sr.LastInsertId()
	tr, err := db.ExecContext(ctx,
		`INSERT INTO task (name, connection_id, storage_id, codec_chain, dumper_opts, cron, enabled, timeout_sec, created_at)
		 VALUES ('ft', ?, ?, '["zstd"]', '{"database":"testdb"}', '0 2 * * *', 1, 1800, unixepoch())`,
		connID, storID,
	)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := tr.LastInsertId()
	return id
}

// TestExecutorSuccess runs a fake-engine task through the shared Executor and
// asserts the run is recorded success with a non-empty checksum and an artifact.
func TestExecutorSuccess(t *testing.T) {
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

	exec := core.NewExecutor(st, nil, func() time.Time {
		return time.Date(2026, 7, 22, 2, 0, 0, 0, time.UTC)
	})
	out, err := exec.Run(context.Background(), task, runID)
	if err != nil {
		t.Fatalf("Executor.Run: %v", err)
	}
	if out.Checksum == "" {
		t.Fatal("checksum empty, want non-empty SHA-256")
	}
	if out.ArtifactID == 0 {
		t.Fatal("artifact id 0, want a recorded artifact")
	}

	// Verify persisted state: run success + one artifact row.
	db, _ := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	defer db.Close()
	var status string
	var runLog string
	if err := db.QueryRow(`SELECT status, log FROM run WHERE id = ?`, runID).Scan(&status, &runLog); err != nil {
		t.Fatal(err)
	}
	if status != string(core.StatusSuccess) {
		t.Fatalf("run status = %q, want success", status)
	}
	if !strings.Contains(runLog, "pipeline complete") || !strings.Contains(runLog, "artifact recorded") {
		t.Fatalf("run log = %q, want pipeline/artifact entries", runLog)
	}
	var artCount int
	var checksum string
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(checksum),'') FROM artifact WHERE run_id = ?`, runID).
		Scan(&artCount, &checksum); err != nil {
		t.Fatal(err)
	}
	if artCount != 1 {
		t.Fatalf("artifacts = %d, want 1", artCount)
	}
	if checksum == "" {
		t.Fatal("stored artifact checksum empty")
	}
}

// TestExecutorContextCancel proves ctx is threaded into the pipeline/dumper:
// cancelling mid-run aborts with a context error (the same propagation that lets
// exec.CommandContext kill a real pg_dump/mysqldump).
func TestExecutorContextCancel(t *testing.T) {
	st, path := openStore(t)
	dir := t.TempDir()
	taskID := seedFakeTaskFull(t, path, dir, "fakeslow", 0, 1800) // blocks 5s in Dump
	task, err := st.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	runID, err := st.StartManualRun(context.Background(), taskID)
	if err != nil {
		t.Fatalf("StartManualRun: %v", err)
	}

	exec := core.NewExecutor(st, nil, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, runErr := exec.Run(ctx, task, runID)
	if runErr == nil {
		t.Fatal("Executor.Run returned nil, want a context-cancellation error")
	}
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("error = %v, want wrapped context.Canceled", runErr)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Run took %s, want to abort promptly on cancel", elapsed)
	}

	// Terminal status recorded despite the cancelled ctx (detached recording).
	db := rawDB(t, path)
	var status string
	if err := db.QueryRow(`SELECT status FROM run WHERE id = ?`, runID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != string(core.StatusFailed) {
		t.Fatalf("run status = %q, want failed", status)
	}
}
