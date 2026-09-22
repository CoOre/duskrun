package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/store/sqlite"
)

// serverWithStore is emptyServer but also hands back the store, so tests that
// need to seed rows the API can't create directly (e.g. a sealed secret) can.
func serverWithStore(t *testing.T) (*httptest.Server, *sqlite.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.db")
	st, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := httptest.NewServer(NewRouter(Deps{
		Store: st, Token: testToken,
		ToolCheck: func(string) error { return nil },
	}))
	t.Cleanup(srv.Close)
	return srv, st
}

func authDelete(t *testing.T, url string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestDeleteConnectionInUseAndFree: deleting a connection a task references is
// 409; deleting a free connection is 204; deleting a missing id is 404.
func TestDeleteConnectionInUseAndFree(t *testing.T) {
	srv := emptyServer(t)

	resp := authPost(t, srv.URL+"/api/connections",
		`{"name":"pg","engine":"postgres","connector_type":"direct","connector_config":{"host":"h","port":5432}}`)
	connID := decodeID(t, resp)
	resp.Body.Close()
	resp = authPost(t, srv.URL+"/api/storages", `{"name":"local","type":"localfs","config":{"root":"/tmp/x"}}`)
	storID := decodeID(t, resp)
	resp.Body.Close()
	resp = authPost(t, srv.URL+"/api/tasks",
		`{"name":"t","connection_id":`+itoa(connID)+`,"storage_id":`+itoa(storID)+`,"cron":"0 2 * * *","dumper_opts":{"database":"app"}}`)
	resp.Body.Close()

	// In use → 409.
	inUse := authDelete(t, srv.URL+"/api/connections/"+itoa(connID))
	inUse.Body.Close()
	if inUse.StatusCode != http.StatusConflict {
		t.Fatalf("delete used connection status = %d, want 409", inUse.StatusCode)
	}

	// A second, unreferenced connection deletes cleanly (204).
	resp = authPost(t, srv.URL+"/api/connections",
		`{"name":"spare","engine":"postgres","connector_type":"direct","connector_config":{"host":"h","port":5432}}`)
	spareID := decodeID(t, resp)
	resp.Body.Close()
	free := authDelete(t, srv.URL+"/api/connections/"+itoa(spareID))
	free.Body.Close()
	if free.StatusCode != http.StatusNoContent {
		t.Fatalf("delete free connection status = %d, want 204", free.StatusCode)
	}

	// Missing id → 404.
	miss := authDelete(t, srv.URL+"/api/connections/"+itoa(spareID))
	miss.Body.Close()
	if miss.StatusCode != http.StatusNotFound {
		t.Fatalf("delete missing connection status = %d, want 404", miss.StatusCode)
	}
}

// TestDeleteTask: a task with no active run deletes with 204 and disappears.
func TestDeleteTask(t *testing.T) {
	srv := emptyServer(t)

	resp := authPost(t, srv.URL+"/api/connections",
		`{"name":"pg","engine":"postgres","connector_type":"direct","connector_config":{"host":"h","port":5432}}`)
	connID := decodeID(t, resp)
	resp.Body.Close()
	resp = authPost(t, srv.URL+"/api/storages", `{"name":"local","type":"localfs","config":{"root":"/tmp/x"}}`)
	storID := decodeID(t, resp)
	resp.Body.Close()
	resp = authPost(t, srv.URL+"/api/tasks",
		`{"name":"t","connection_id":`+itoa(connID)+`,"storage_id":`+itoa(storID)+`,"cron":"0 2 * * *","dumper_opts":{"database":"app"}}`)
	taskID := decodeID(t, resp)
	resp.Body.Close()

	del := authDelete(t, srv.URL+"/api/tasks/"+itoa(taskID))
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("delete task status = %d, want 204", del.StatusCode)
	}
	list := authGet(t, srv.URL+"/api/tasks")
	defer list.Body.Close()
	var tasks []taskDTO
	if err := json.NewDecoder(list.Body).Decode(&tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatalf("tasks after delete = %d, want 0", len(tasks))
	}
}

// TestDeleteSecretInUse: a secret referenced by a connection is 409 with the
// referrer named; once the connection is gone, it deletes with 204.
func TestDeleteSecretInUse(t *testing.T) {
	srv, st := serverWithStore(t)
	ctx := context.Background()

	secID, err := st.PutSecret(ctx, core.Secret{Name: "db/pg", Type: "db-password", Ciphertext: []byte("x"), KeyID: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	resp := authPost(t, srv.URL+"/api/connections",
		`{"name":"pg","engine":"postgres","connector_type":"direct","connector_config":{"host":"h","port":5432},"secret_ref":"secret://db/pg"}`)
	connID := decodeID(t, resp)
	resp.Body.Close()

	inUse := authDelete(t, srv.URL+"/api/secrets/"+itoa(secID))
	if inUse.StatusCode != http.StatusConflict {
		inUse.Body.Close()
		t.Fatalf("delete used secret status = %d, want 409", inUse.StatusCode)
	}
	var body map[string]string
	_ = json.NewDecoder(inUse.Body).Decode(&body)
	inUse.Body.Close()
	if body["error"] == "" {
		t.Fatalf("409 body should carry an error message, got %v", body)
	}

	// Drop the referrer, then the secret deletes.
	authDelete(t, srv.URL+"/api/connections/"+itoa(connID)).Body.Close()
	free := authDelete(t, srv.URL+"/api/secrets/"+itoa(secID))
	free.Body.Close()
	if free.StatusCode != http.StatusNoContent {
		t.Fatalf("delete free secret status = %d, want 204", free.StatusCode)
	}
}
