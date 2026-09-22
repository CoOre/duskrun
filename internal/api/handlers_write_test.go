package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/store/sqlite"

	// Register the plugins the write-path validation checks against.
	_ "github.com/duskrun/duskrun/internal/codec/zstd"
	_ "github.com/duskrun/duskrun/internal/connector/dockerproxy"
	_ "github.com/duskrun/duskrun/internal/connector/local"
	_ "github.com/duskrun/duskrun/internal/dumper/postgres"
	_ "github.com/duskrun/duskrun/internal/notifier/logn"
	_ "github.com/duskrun/duskrun/internal/storage/localfs"
)

// emptyServer returns an API server over a fresh empty store.
func emptyServer(t *testing.T) *httptest.Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.db")
	st, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// Permissive tool check so these tests don't depend on pg_dump/mysqldump
	// being installed on the host.
	srv := httptest.NewServer(NewRouter(Deps{
		Store: st, Token: testToken,
		ToolCheck: func(string) error { return nil },
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestCreateTaskToolMissing: with the engine's external tool unavailable, task
// creation is rejected with 400.
func TestCreateTaskToolMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.db")
	st, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := httptest.NewServer(NewRouter(Deps{
		Store: st, Token: testToken,
		ToolCheck: func(engine string) error { return errToolMissing },
	}))
	t.Cleanup(srv.Close)

	resp := authPost(t, srv.URL+"/api/connections",
		`{"name":"pg","engine":"postgres","connector_type":"direct","connector_config":{"host":"h","port":5432}}`)
	connID := decodeID(t, resp)
	resp.Body.Close()
	resp = authPost(t, srv.URL+"/api/storages", `{"name":"local","type":"localfs","config":{"root":"/tmp/x"}}`)
	storID := decodeID(t, resp)
	resp.Body.Close()

	bad := authPost(t, srv.URL+"/api/tasks",
		`{"name":"t","connection_id":`+itoa(connID)+`,"storage_id":`+itoa(storID)+`,"cron":"0 2 * * *"}`)
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (tool missing)", bad.StatusCode)
	}
}

func authPost(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func authPatch(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPatch, url, bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeID(t *testing.T, resp *http.Response) int64 {
	t.Helper()
	var m map[string]int64
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m["id"]
}

func TestCreateAndListTaskRoundTrip(t *testing.T) {
	srv := emptyServer(t)

	resp := authPost(t, srv.URL+"/api/connections",
		`{"name":"pg","engine":"postgres","connector_type":"direct","connector_config":{"host":"127.0.0.1","port":5432}}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create connection status = %d, want 201", resp.StatusCode)
	}
	connID := decodeID(t, resp)
	resp.Body.Close()

	resp = authPost(t, srv.URL+"/api/storages", `{"name":"local","type":"localfs","config":{"root":"/tmp/x"}}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create storage status = %d, want 201", resp.StatusCode)
	}
	storID := decodeID(t, resp)
	resp.Body.Close()

	taskBody := `{"name":"nightly","connection_id":` + itoa(connID) +
		`,"storage_id":` + itoa(storID) +
		`,"cron":"0 2 * * *","codec_chain":["zstd"],"notifiers":["log"],"dumper_opts":{"database":"app"}}`
	resp = authPost(t, srv.URL+"/api/tasks", taskBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create task status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	// Round-trip: the task now appears in the listing.
	list := authGet(t, srv.URL+"/api/tasks")
	defer list.Body.Close()
	var tasks []taskDTO
	if err := json.NewDecoder(list.Body).Decode(&tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Name != "nightly" {
		t.Fatalf("tasks = %+v, want one 'nightly'", tasks)
	}
}

func TestCreateDockerProxyConnectionRoundTrip(t *testing.T) {
	srv := emptyServer(t)

	for _, body := range []string{
		`{"name":"pg-docker","engine":"postgres","connector_type":"docker-proxy","connector_config":{"network":"app_default","target_host":"postgres","target_port":5432}}`,
		`{"name":"pg-docker-ssh","engine":"postgres","connector_type":"docker-ssh-proxy","connector_config":{"ssh_host":"docker.example.com","ssh_user":"deploy","private_key_ref":"secret://ssh/docker","network":"app_default","target_host":"postgres","target_port":5432}}`,
	} {
		resp := authPost(t, srv.URL+"/api/connections", body)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create connection status = %d, want 201", resp.StatusCode)
		}
		resp.Body.Close()
	}

	connsResp := authGet(t, srv.URL+"/api/connections")
	defer connsResp.Body.Close()
	var conns []connDTO
	if err := json.NewDecoder(connsResp.Body).Decode(&conns); err != nil {
		t.Fatal(err)
	}
	if len(conns) != 2 || conns[0].ConnectorType != "docker-proxy" || conns[1].ConnectorType != "docker-ssh-proxy" {
		t.Fatalf("connections = %+v, want docker proxy connections", conns)
	}
}

func TestUpdateConnectionStorageTask(t *testing.T) {
	srv := emptyServer(t)

	resp := authPost(t, srv.URL+"/api/connections",
		`{"name":"pg","engine":"postgres","connector_type":"direct","connector_config":{"host":"127.0.0.1","port":5432},"secret_ref":"secret://old"}`)
	connID := decodeID(t, resp)
	resp.Body.Close()
	resp = authPost(t, srv.URL+"/api/storages", `{"name":"local","type":"localfs","config":{"root":"/tmp/a"}}`)
	storID := decodeID(t, resp)
	resp.Body.Close()
	resp = authPost(t, srv.URL+"/api/tasks",
		`{"name":"nightly","connection_id":`+itoa(connID)+`,"storage_id":`+itoa(storID)+`,"cron":"0 2 * * *","codec_chain":["zstd"],"dumper_opts":{"database":"app"}}`)
	taskID := decodeID(t, resp)
	resp.Body.Close()

	patch := authPatch(t, srv.URL+"/api/connections/"+itoa(connID),
		`{"name":"pg2","engine":"postgres","connector_type":"direct","connector_config":{"host":"db","port":15432},"secret_ref":"secret://new"}`)
	if patch.StatusCode != http.StatusOK {
		t.Fatalf("patch connection status = %d, want 200", patch.StatusCode)
	}
	patch.Body.Close()

	patch = authPatch(t, srv.URL+"/api/storages/"+itoa(storID), `{"name":"local2","type":"localfs","config":{"root":"/tmp/b"}}`)
	if patch.StatusCode != http.StatusOK {
		t.Fatalf("patch storage status = %d, want 200", patch.StatusCode)
	}
	patch.Body.Close()

	patch = authPatch(t, srv.URL+"/api/tasks/"+itoa(taskID),
		`{"name":"nightly2","connection_id":`+itoa(connID)+`,"storage_id":`+itoa(storID)+`,"cron":"0 3 * * *","codec_chain":["zstd"],"dumper_opts":{"database":"app"},"enabled":false}`)
	if patch.StatusCode != http.StatusOK {
		t.Fatalf("patch task status = %d, want 200", patch.StatusCode)
	}
	patch.Body.Close()

	connsResp := authGet(t, srv.URL+"/api/connections")
	defer connsResp.Body.Close()
	var conns []connDTO
	if err := json.NewDecoder(connsResp.Body).Decode(&conns); err != nil {
		t.Fatal(err)
	}
	if len(conns) != 1 || conns[0].Name != "pg2" || conns[0].SecretRef != "secret://new" {
		t.Fatalf("connections = %+v, want updated connection", conns)
	}
	if string(conns[0].ConnectorConfig) == "" {
		t.Fatalf("connector config not returned for edit: %+v", conns[0])
	}

	stResp := authGet(t, srv.URL+"/api/storages")
	defer stResp.Body.Close()
	var storages []storageDTO
	if err := json.NewDecoder(stResp.Body).Decode(&storages); err != nil {
		t.Fatal(err)
	}
	if len(storages) != 1 || storages[0].Name != "local2" || string(storages[0].Config) == "" {
		t.Fatalf("storages = %+v, want updated storage with config", storages)
	}

	taskResp := authGet(t, srv.URL+"/api/tasks")
	defer taskResp.Body.Close()
	var tasks []taskDTO
	if err := json.NewDecoder(taskResp.Body).Decode(&tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Name != "nightly2" || tasks[0].Cron != "0 3 * * *" || tasks[0].Enabled {
		t.Fatalf("tasks = %+v, want updated disabled task", tasks)
	}
}

func TestCreateTaskValidates(t *testing.T) {
	srv := emptyServer(t)
	// A valid connection + storage to reference.
	resp := authPost(t, srv.URL+"/api/connections",
		`{"name":"pg","engine":"postgres","connector_type":"direct","connector_config":{"host":"h","port":5432}}`)
	connID := decodeID(t, resp)
	resp.Body.Close()
	resp = authPost(t, srv.URL+"/api/storages", `{"name":"local","type":"localfs","config":{"root":"/tmp/x"}}`)
	storID := decodeID(t, resp)
	resp.Body.Close()

	// Bad cron → 400.
	bad := authPost(t, srv.URL+"/api/tasks",
		`{"name":"t1","connection_id":`+itoa(connID)+`,"storage_id":`+itoa(storID)+`,"cron":"not a cron"}`)
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad cron status = %d, want 400", bad.StatusCode)
	}
	bad.Body.Close()

	// Nonexistent storage → 400.
	bad2 := authPost(t, srv.URL+"/api/tasks",
		`{"name":"t2","connection_id":`+itoa(connID)+`,"storage_id":9999,"cron":"0 2 * * *"}`)
	if bad2.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad storage status = %d, want 400", bad2.StatusCode)
	}
	bad2.Body.Close()

	// Unknown codec → 400.
	bad3 := authPost(t, srv.URL+"/api/tasks",
		`{"name":"t3","connection_id":`+itoa(connID)+`,"storage_id":`+itoa(storID)+`,"cron":"0 2 * * *","codec_chain":["nope"]}`)
	if bad3.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown codec status = %d, want 400", bad3.StatusCode)
	}
	bad3.Body.Close()
}

// TestTaskRetentionRoundTrips covers the GFS policy end to end: it survives
// create, comes back in the listing, and survives an edit that carries it.
func TestTaskRetentionRoundTrips(t *testing.T) {
	srv := emptyServer(t)
	connID, storID := taskRefs(t, srv)

	body := `{"name":"nightly","connection_id":` + itoa(connID) + `,"storage_id":` + itoa(storID) +
		`,"cron":"0 2 * * *","dumper_opts":{"database":"app"},"retention":{"keep_last":14,"gfs":{"daily":7,"weekly":4,"monthly":12}}}`
	resp := authPost(t, srv.URL+"/api/tasks", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create task status = %d, want 201", resp.StatusCode)
	}
	taskID := decodeID(t, resp)
	resp.Body.Close()

	got := taskRetention(t, srv)
	if got.KeepLast != 14 || got.GFS == nil {
		t.Fatalf("retention = %+v, want keep_last 14 + GFS", got)
	}
	if got.GFS.Daily != 7 || got.GFS.Weekly != 4 || got.GFS.Monthly != 12 {
		t.Fatalf("gfs = %+v, want 7/4/12", got.GFS)
	}

	patch := authPatch(t, srv.URL+"/api/tasks/"+itoa(taskID),
		`{"name":"nightly","connection_id":`+itoa(connID)+`,"storage_id":`+itoa(storID)+
			`,"cron":"0 2 * * *","dumper_opts":{"database":"app"},"retention":{"keep_last":14,"gfs":{"daily":7,"weekly":4,"monthly":12}}}`)
	if patch.StatusCode != http.StatusOK {
		t.Fatalf("patch task status = %d, want 200", patch.StatusCode)
	}
	patch.Body.Close()

	if got := taskRetention(t, srv); got.GFS == nil || got.GFS.Monthly != 12 {
		t.Fatalf("retention after edit = %+v, want GFS preserved", got)
	}
}

// TestTaskRetentionDropsEmptyGFS: an all-zero block keeps nothing, so it must
// not persist as a policy that merely looks configured.
func TestTaskRetentionDropsEmptyGFS(t *testing.T) {
	srv := emptyServer(t)
	connID, storID := taskRefs(t, srv)

	resp := authPost(t, srv.URL+"/api/tasks",
		`{"name":"nightly","connection_id":`+itoa(connID)+`,"storage_id":`+itoa(storID)+
			`,"cron":"0 2 * * *","dumper_opts":{"database":"app"},"retention":{"keep_last":5,"gfs":{"daily":0,"weekly":0,"monthly":0}}}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create task status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	got := taskRetention(t, srv)
	if got.GFS != nil {
		t.Fatalf("gfs = %+v, want nil for an all-zero block", got.GFS)
	}
	if got.KeepLast != 5 {
		t.Fatalf("keep_last = %d, want 5", got.KeepLast)
	}
}

// TestTaskRejectsNegativeRetention: Forget reads any count <= 0 as "rule off",
// so a negative would silently disable pruning instead of failing loudly.
func TestTaskRejectsNegativeRetention(t *testing.T) {
	srv := emptyServer(t)
	connID, storID := taskRefs(t, srv)

	for _, ret := range []string{
		`{"keep_last":-1}`,
		`{"gfs":{"daily":-1,"weekly":4,"monthly":12}}`,
		`{"gfs":{"daily":7,"weekly":-4,"monthly":12}}`,
		`{"gfs":{"daily":7,"weekly":4,"monthly":-12}}`,
	} {
		resp := authPost(t, srv.URL+"/api/tasks",
			`{"name":"t","connection_id":`+itoa(connID)+`,"storage_id":`+itoa(storID)+
				`,"cron":"0 2 * * *","dumper_opts":{"database":"app"},"retention":`+ret+`}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("retention %s status = %d, want 400", ret, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// taskRefs creates the connection and storage a task must reference.
func taskRefs(t *testing.T, srv *httptest.Server) (connID, storID int64) {
	t.Helper()
	resp := authPost(t, srv.URL+"/api/connections",
		`{"name":"pg","engine":"postgres","connector_type":"direct","connector_config":{"host":"127.0.0.1","port":5432}}`)
	connID = decodeID(t, resp)
	resp.Body.Close()
	resp = authPost(t, srv.URL+"/api/storages", `{"name":"local","type":"localfs","config":{"root":"/tmp/x"}}`)
	storID = decodeID(t, resp)
	resp.Body.Close()
	return connID, storID
}

// taskRetention reads back the retention policy of the server's only task.
func taskRetention(t *testing.T, srv *httptest.Server) core.Retention {
	t.Helper()
	list := authGet(t, srv.URL+"/api/tasks")
	defer list.Body.Close()
	var tasks []taskDTO
	if err := json.NewDecoder(list.Body).Decode(&tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want exactly one", len(tasks))
	}
	return tasks[0].Retention
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

var errToolMissing = errors.New("required tool not installed")
