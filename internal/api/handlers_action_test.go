package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/duskrun/duskrun/internal/store/sqlite"
)

func TestRunNowEnqueues(t *testing.T) {
	srv, _, st := seededServer(t) // task id 1 exists
	resp := authPost(t, srv.URL+"/api/tasks/1/run", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var body map[string]int64
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["run_id"] == 0 {
		t.Fatal("run_id missing")
	}
	// A queued run now exists for the task.
	runs, err := st.ListRuns(context.Background(), 1, "queued")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("queued runs = %d, want 1", len(runs))
	}

	// Missing task → 404.
	resp2 := authPost(t, srv.URL+"/api/tasks/9999/run", "")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("missing task run status = %d, want 404", resp2.StatusCode)
	}
}

func TestArtifactDownload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.db")
	st, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	payload := []byte("artifact-bytes-0123456789")
	key := "task/db/20260722_0200_db.dump.zst"
	dst := filepath.Join(root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, payload, 0o640); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, `INSERT INTO connection (name, engine, connector_type, created_at) VALUES ('c','postgres','direct', unixepoch())`)
	mustExec(t, db, `INSERT INTO storage (name, type, config, created_at) VALUES ('s','localfs', ?, unixepoch())`, `{"root":"`+root+`"}`)
	mustExec(t, db, `INSERT INTO task (name, connection_id, storage_id, cron, created_at) VALUES ('t',1,1,'0 2 * * *', unixepoch())`)
	mustExec(t, db, `INSERT INTO run (task_id, status, attempt, created_at) VALUES (1,'success',1, unixepoch())`)
	res, err := db.Exec(`INSERT INTO artifact (run_id, storage_id, key, size, checksum, created_at) VALUES (1,1,?,?,'abc', unixepoch())`,
		key, len(payload))
	if err != nil {
		t.Fatal(err)
	}
	artID, _ := res.LastInsertId()

	srv := httptest.NewServer(NewRouter(Deps{Store: st, Token: testToken}))
	defer srv.Close()

	resp := authGet(t, srv.URL+"/api/artifacts/"+strconv.FormatInt(artID, 10)+"/download")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("content-type = %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd == "" {
		t.Error("missing Content-Disposition")
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != string(payload) {
		t.Fatalf("downloaded %q, want %q", got, payload)
	}

	resp2 := authGet(t, srv.URL+"/api/runs/1/artifact/download")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("run artifact status = %d, want 200", resp2.StatusCode)
	}
	got, _ = io.ReadAll(resp2.Body)
	if string(got) != string(payload) {
		t.Fatalf("run artifact downloaded %q, want %q", got, payload)
	}
}
