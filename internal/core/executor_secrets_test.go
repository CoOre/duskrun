package core_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/plugin"
)

// capture records the last config each fake plugin was built with, so a test can
// assert what the executor actually handed the plugin rather than what it stored.
var capture struct {
	mu       sync.Mutex
	storage  []byte
	dumper   []byte
	dumpOpts plugin.DumpOptions // what Dump was actually called with
	hintOpts plugin.DumpOptions // what RestoreHint was actually called with
	closed   int                // times the built storage was closed
}

func init() {
	plugin.Storages.Register("capturefs", func(cfg []byte) (plugin.Storage, error) {
		capture.mu.Lock()
		capture.storage = append([]byte(nil), cfg...)
		capture.mu.Unlock()
		return closableStorage{}, nil
	})
	plugin.Dumpers.Register("captureengine", func(cfg []byte) (plugin.Dumper, error) {
		capture.mu.Lock()
		capture.dumper = append([]byte(nil), cfg...)
		capture.mu.Unlock()
		return captureDumper{fakeDumper{data: []byte("payload")}}, nil
	})
}

// captureDumper records the options the executor hands the RUNNING dumper.
// Every real dumper ignores its factory config and reads its knobs here, so this
// — not the factory config — is where a resolved secret has to arrive.
type captureDumper struct{ fakeDumper }

func (d captureDumper) Dump(ctx context.Context, ep plugin.Endpoint, cr plugin.Credentials, opt plugin.DumpOptions) (io.ReadCloser, error) {
	capture.mu.Lock()
	capture.dumpOpts = opt
	capture.mu.Unlock()
	return d.fakeDumper.Dump(ctx, ep, cr, opt)
}

func (captureDumper) RestoreHint(opt plugin.DumpOptions) string {
	capture.mu.Lock()
	capture.hintOpts = opt
	capture.mu.Unlock()
	return "fake-restore <file>"
}

// closableStorage is a discardStorage that also counts Close, standing in for a
// session-holding storage (sftp) the executor has to release.
type closableStorage struct{ discardStorage }

func (closableStorage) Close() error {
	capture.mu.Lock()
	capture.closed++
	capture.mu.Unlock()
	return nil
}

// discardStorage accepts writes and reports a plausible ObjectMeta; the test is
// about the config it was built with, not about the bytes.
type discardStorage struct{}

func (discardStorage) Write(_ context.Context, key string, r io.Reader) (plugin.ObjectMeta, error) {
	n, err := io.Copy(io.Discard, r)
	return plugin.ObjectMeta{Key: key, Size: n}, err
}
func (discardStorage) Read(context.Context, string) (io.ReadCloser, error) { return nil, nil }
func (discardStorage) List(context.Context, string) ([]plugin.Object, error) {
	return nil, nil
}
func (discardStorage) Delete(context.Context, string) error { return nil }

// fixedOpener stands in for *secret.Box: every sealed row opens to the same
// plaintext, which is enough to prove the value travelled from the store.
type fixedOpener struct{ value string }

func (o fixedOpener) Open(core.Secret) ([]byte, error) { return []byte(o.value), nil }

// seedSecretTask builds a connection/storage/task graph whose storage and dumper
// configs both carry a `<key>_ref`, plus the secret row they point at.
func seedSecretTask(t *testing.T, path string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO secret (name, type, ciphertext, key_id, created_at)
		 VALUES ('sftp/key', 'ssh-key', x'00', 'env:v1', unixepoch())`,
	); err != nil {
		t.Fatal(err)
	}
	cr, err := db.ExecContext(ctx,
		`INSERT INTO connection (name, engine, connector_type, connector_config, created_at)
		 VALUES ('cs', 'captureengine', 'direct', '{"host":"127.0.0.1","port":5432}', unixepoch())`,
	)
	if err != nil {
		t.Fatal(err)
	}
	connID, _ := cr.LastInsertId()
	sr, err := db.ExecContext(ctx,
		`INSERT INTO storage (name, type, config, created_at)
		 VALUES ('remote', 'capturefs', '{"path":"/backups","private_key_ref":"secret://sftp/key"}', unixepoch())`,
	)
	if err != nil {
		t.Fatal(err)
	}
	storID, _ := sr.LastInsertId()
	tr, err := db.ExecContext(ctx,
		`INSERT INTO task (name, connection_id, storage_id, codec_chain, dumper_opts, cron, enabled, timeout_sec, created_at)
		 VALUES ('st', ?, ?, '[]', '{"database":"app","fetch":{"private_key_ref":"secret://sftp/key"}}', '0 2 * * *', 1, 1800, unixepoch())`,
		connID, storID,
	)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := tr.LastInsertId()
	return id
}

// TestExecutorResolvesPluginConfigSecrets is the guard for the gap that made
// sftp impossible: a storage (and a staged dumper's fetch block) keeps its key
// in the secret store, and the plugin must receive the decrypted value — not the
// reference, and not an empty string.
func TestExecutorResolvesPluginConfigSecrets(t *testing.T) {
	st, path := openStore(t)
	taskID := seedSecretTask(t, path)
	capture.mu.Lock()
	capture.closed = 0
	capture.mu.Unlock()

	ctx := context.Background()
	task, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	runID, err := st.StartManualRun(ctx, taskID)
	if err != nil {
		t.Fatalf("StartManualRun: %v", err)
	}

	exec := core.NewExecutor(st, fixedOpener{value: "PRIVATE KEY"}, time.Now)
	if _, err := exec.Run(ctx, task, runID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	capture.mu.Lock()
	storageCfg, dumperCfg := capture.storage, capture.dumper
	capture.mu.Unlock()

	var stor map[string]any
	if err := json.Unmarshal(storageCfg, &stor); err != nil {
		t.Fatalf("storage config %s: %v", storageCfg, err)
	}
	if stor["private_key"] != "PRIVATE KEY" {
		t.Fatalf("storage private_key = %v, want the resolved secret", stor["private_key"])
	}

	var dump struct {
		Fetch struct {
			PrivateKey string `json:"private_key"`
		} `json:"fetch"`
	}
	if err := json.Unmarshal(dumperCfg, &dump); err != nil {
		t.Fatalf("dumper opts %s: %v", dumperCfg, err)
	}
	if dump.Fetch.PrivateKey != "PRIVATE KEY" {
		t.Fatalf("dumper fetch.private_key = %q, want the resolved secret", dump.Fetch.PrivateKey)
	}

	// The factory config above is not enough: every registered dumper ignores it
	// and reads its options from what Dump receives, so the resolution has to be
	// visible there too.
	capture.mu.Lock()
	dumpOpts, hintOpts, closed := capture.dumpOpts, capture.hintOpts, capture.closed
	capture.mu.Unlock()

	if got := fetchKey(t, dumpOpts); got != "PRIVATE KEY" {
		t.Fatalf("Dump opts fetch.private_key = %q, want the resolved secret", got)
	}
	// The restore hint is UI text: it must keep the reference, not the key.
	if got := fetchKey(t, hintOpts); got != "" {
		t.Fatalf("RestoreHint opts fetch.private_key = %q, want the unresolved config", got)
	}
	if closed != 1 {
		t.Fatalf("storage closed %d times, want exactly 1 — a session-holding storage leaks otherwise", closed)
	}
}

// fetchKey digs the nested fetch.private_key out of DumpOptions, returning "" when
// the block carries only the unresolved reference.
func fetchKey(t *testing.T, opt plugin.DumpOptions) string {
	t.Helper()
	fetch, ok := opt["fetch"].(map[string]any)
	if !ok {
		t.Fatalf("dump opts %v carry no fetch block", opt)
	}
	key, _ := fetch["private_key"].(string)
	return key
}

// TestExecutorFailsRunOnMissingSecret proves the failure is loud: a dangling ref
// fails the run instead of building the plugin with an empty credential, which
// would surface as an authentication error against the wrong cause.
func TestExecutorFailsRunOnMissingSecret(t *testing.T) {
	st, path := openStore(t)
	taskID := seedSecretTask(t, path)

	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM secret WHERE name = 'sftp/key'`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	task, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	runID, err := st.StartManualRun(ctx, taskID)
	if err != nil {
		t.Fatalf("StartManualRun: %v", err)
	}

	exec := core.NewExecutor(st, fixedOpener{value: "PRIVATE KEY"}, time.Now)
	if _, err := exec.Run(ctx, task, runID); err == nil {
		t.Fatal("Run = nil error, want the missing secret to fail the run")
	}

	run, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != core.StatusFailed {
		t.Fatalf("run status = %s, want failed", run.Status)
	}
}
