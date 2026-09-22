package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/store/sqlite"
)

// seededServer opens a temp store, seeds a connection+storage+task and returns a
// running API server plus a raw handle for seeding runs.
func seededServer(t *testing.T) (*httptest.Server, *sql.DB, *sqlite.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.db")
	st, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO connection (name, engine, connector_type, created_at) VALUES ('c','postgres','direct', unixepoch())`)
	mustExec(t, db, `INSERT INTO storage (name, type, created_at) VALUES ('s','localfs', unixepoch())`)
	mustExec(t, db, `INSERT INTO task (name, connection_id, storage_id, codec_chain, cron, enabled, created_at)
		VALUES ('nightly', 1, 1, '["zstd"]', '0 2 * * *', 1, unixepoch())`)
	_ = ctx

	srv := httptest.NewServer(NewRouter(Deps{Store: st, Token: testToken}))
	t.Cleanup(srv.Close)
	return srv, db, st
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func authGet(t *testing.T, url string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestListTasks(t *testing.T) {
	srv, _, _ := seededServer(t)
	resp := authGet(t, srv.URL+"/api/tasks")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var tasks []taskDTO
	if err := json.NewDecoder(resp.Body).Decode(&tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	if tasks[0].Name != "nightly" || !tasks[0].Enabled {
		t.Fatalf("task = %+v, want name=nightly enabled", tasks[0])
	}
}

func TestListConnections(t *testing.T) {
	srv, _, _ := seededServer(t)
	resp := authGet(t, srv.URL+"/api/connections")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var conns []connDTO
	if err := json.NewDecoder(resp.Body).Decode(&conns); err != nil {
		t.Fatal(err)
	}
	if len(conns) != 1 {
		t.Fatalf("connections = %d, want 1", len(conns))
	}
	c := conns[0]
	if c.Name != "c" || c.Engine != "postgres" || c.ConnectorType != "direct" {
		t.Fatalf("conn = %+v, want name=c engine=postgres connector_type=direct", c)
	}
}

func TestListConnectionDatabases(t *testing.T) {
	srv, _, st := seededServer(t)
	srv.Config.Handler = NewRouter(Deps{
		Store: st,
		Token: testToken,
		DatabaseList: func(_ context.Context, conn core.Connection) ([]string, error) {
			if conn.ID != 1 || conn.Engine != "postgres" {
				t.Fatalf("conn = %+v, want seeded postgres connection", conn)
			}
			return []string{"analytics", "orders"}, nil
		},
	})

	resp := authGet(t, srv.URL+"/api/connections/1/databases")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Databases []string `json:"databases"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Databases) != 2 || got.Databases[0] != "analytics" || got.Databases[1] != "orders" {
		t.Fatalf("databases = %+v, want analytics/orders", got.Databases)
	}
}

func TestListConnectionDatabasesUnsupported(t *testing.T) {
	srv, _, st := seededServer(t)
	srv.Config.Handler = NewRouter(Deps{
		Store: st,
		Token: testToken,
		DatabaseList: func(context.Context, core.Connection) ([]string, error) {
			return nil, fmt.Errorf("%w: sqlite", errDatabaseListUnsupported)
		},
	})

	resp := authGet(t, srv.URL+"/api/connections/1/databases")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var got map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got["error"], "database listing is not supported") {
		t.Fatalf("error response = %+v", got)
	}
}

func TestListStorages(t *testing.T) {
	srv, _, _ := seededServer(t)
	resp := authGet(t, srv.URL+"/api/storages")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var storages []storageDTO
	if err := json.NewDecoder(resp.Body).Decode(&storages); err != nil {
		t.Fatal(err)
	}
	if len(storages) != 1 {
		t.Fatalf("storages = %d, want 1", len(storages))
	}
	if storages[0].Name != "s" || storages[0].Type != "localfs" {
		t.Fatalf("storage = %+v, want name=s type=localfs", storages[0])
	}
}

func TestListRunsKeysetPage(t *testing.T) {
	srv, db, _ := seededServer(t)
	for i := 0; i < 5; i++ {
		mustExec(t, db, `INSERT INTO run (task_id, status, attempt, created_at)
			VALUES (1, 'success', 1, unixepoch() + ?)`, i)
	}

	assertIDs := func(path string, want ...int64) {
		t.Helper()
		resp := authGet(t, srv.URL+path)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, resp.StatusCode)
		}
		var runs []runDTO
		if err := json.NewDecoder(resp.Body).Decode(&runs); err != nil {
			t.Fatal(err)
		}
		if len(runs) != len(want) {
			t.Fatalf("%s len = %d, want %d (%+v)", path, len(runs), len(want), runs)
		}
		for i, id := range want {
			if runs[i].ID != id {
				t.Fatalf("%s ids[%d] = %d, want %d (%+v)", path, i, runs[i].ID, id, runs)
			}
		}
	}

	assertIDs("/api/runs?limit=2", 5, 4)
	assertIDs("/api/runs?limit=2&before=4", 3, 2)
	assertIDs("/api/runs?limit=2&after=3", 5, 4)
	assertIDs("/api/runs?limit=2&anchor=3", 3, 2)
}

func TestGetRun(t *testing.T) {
	srv, db, _ := seededServer(t)
	// Seed a finished run with a log.
	res, err := db.Exec(`INSERT INTO run (task_id, status, attempt, error, log, created_at)
		VALUES (1, 'success', 1, '', 'dump ok\n123 bytes', unixepoch())`)
	if err != nil {
		t.Fatal(err)
	}
	runID, _ := res.LastInsertId()
	mustExec(t, db, `INSERT INTO artifact (run_id, storage_id, key, size, checksum, created_at)
		VALUES (?, 1, 'nightly/app/20260722_0200_app.dump.zst', 123, 'sha', unixepoch())`, runID)

	resp := authGet(t, srv.URL+"/api/runs/"+strconv.FormatInt(runID, 10))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var run runDTO
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	if run.ID != runID || run.Status != "success" {
		t.Fatalf("run = %+v, want id=%d status=success", run, runID)
	}
	if run.Log == "" {
		t.Fatal("run log empty, want the seeded log")
	}
	if run.Artifact == nil || run.Artifact.Key == "" || run.Artifact.Size != 123 {
		t.Fatalf("run artifact = %+v, want metadata", run.Artifact)
	}

	// Unknown id → 404.
	resp2 := authGet(t, srv.URL+"/api/runs/9999")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("missing run status = %d, want 404", resp2.StatusCode)
	}
}

func TestTaskDTONextRun(t *testing.T) {
	now := time.Date(2026, 8, 12, 6, 51, 0, 0, time.UTC)

	// Enabled task: next_run is the next cron activation, in UTC.
	dto := toTaskDTOAt(core.Task{Cron: "0 2 * * *", Enabled: true}, now)
	if dto.NextRun == nil {
		t.Fatal("next_run = nil, want the next activation")
	}
	want := time.Date(2026, 8, 13, 2, 0, 0, 0, time.UTC)
	if !dto.NextRun.Equal(want) {
		t.Fatalf("next_run = %s, want %s", dto.NextRun, want)
	}

	// Paused task: no schedule to report.
	if got := toTaskDTOAt(core.Task{Cron: "0 2 * * *"}, now); got.NextRun != nil {
		t.Fatalf("paused next_run = %s, want nil", got.NextRun)
	}

	// Unparsable cron: omitted rather than fabricated (the dispatcher skips it too).
	if got := toTaskDTOAt(core.Task{Cron: "not a cron", Enabled: true}, now); got.NextRun != nil {
		t.Fatalf("bad-cron next_run = %s, want nil", got.NextRun)
	}
}
