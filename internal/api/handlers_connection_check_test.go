package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/duskrun/duskrun/internal/core"
	_ "github.com/duskrun/duskrun/internal/dumper/mongodb"
	_ "github.com/duskrun/duskrun/internal/dumper/redis"
	"github.com/duskrun/duskrun/internal/store/sqlite"
)

func openTestStore(t *testing.T) *sqlite.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.db")
	st, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func connectionTestHandler(t *testing.T, deps Deps) http.Handler {
	t.Helper()
	deps.Store = openTestStore(t)
	deps.Token = testToken
	return NewRouter(deps)
}

func postConnectionTest(t *testing.T, h http.Handler, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/connections/test", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Result()
}

func decodeConnectionTest(t *testing.T, resp *http.Response) connectionTestResponse {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got connectionTestResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

// checkStatus returns the status of the check with the given key, or "" if the
// check was not reported (stage not reached).
func checkStatus(got connectionTestResponse, key string) connectionTestStatus {
	for _, c := range got.Checks {
		if c.Key == key {
			return c.Status
		}
	}
	return ""
}

func TestConnectionTestSuccess(t *testing.T) {
	h := connectionTestHandler(t, Deps{
		ConnectionCheck: func(_ context.Context, conn core.Connection) ([]string, error) {
			if conn.Name != "pg" || conn.Engine != "postgres" || conn.ConnectorType != "direct" {
				t.Fatalf("conn = %+v, want posted connection", conn)
			}
			return []string{"analytics", "orders"}, nil
		},
	})

	resp := postConnectionTest(t, h,
		`{"name":"pg","engine":"postgres","connector_type":"direct","connector_config":{"host":"127.0.0.1","port":5432}}`)
	got := decodeConnectionTest(t, resp)

	if got.Status != connectionTestOK || got.Error != nil {
		t.Fatalf("response = %+v, want ok without error", got)
	}
	if len(got.Databases) != 2 || got.Databases[0] != "analytics" || got.Databases[1] != "orders" {
		t.Fatalf("databases = %+v, want analytics/orders", got.Databases)
	}
	// All four stages reported ok.
	for _, key := range []string{"config", "connector", "auth", "catalog"} {
		if checkStatus(got, key) != connectionTestOK {
			t.Fatalf("check %q = %q, want ok (checks=%+v)", key, checkStatus(got, key), got.Checks)
		}
	}
}

func TestConnectionTestInvalidConfigReturnsFailedBody(t *testing.T) {
	called := false
	h := connectionTestHandler(t, Deps{
		ConnectionCheck: func(context.Context, core.Connection) ([]string, error) {
			called = true
			return nil, nil
		},
	})

	resp := postConnectionTest(t, h,
		`{"engine":"postgres","connector_type":"direct","connector_config":{"host":"127.0.0.1","port":5432}}`)
	got := decodeConnectionTest(t, resp)

	if called {
		t.Fatal("ConnectionCheck called for invalid config")
	}
	if got.Status != connectionTestFailed || got.Error == nil || got.Error.Code != "invalid_config" {
		t.Fatalf("response = %+v, want failed invalid_config", got)
	}
	if len(got.Checks) != 1 || got.Checks[0].Key != "config" || got.Checks[0].Status != connectionTestFailed {
		t.Fatalf("checks = %+v, want only config failed", got.Checks)
	}
}

func TestConnectionTestToolMissingFailsAtAuthStage(t *testing.T) {
	h := connectionTestHandler(t, Deps{
		ConnectionCheck: func(context.Context, core.Connection) ([]string, error) {
			return nil, &connCheckError{Stage: stageAuth, Code: "tool_missing", err: errors.New("psql missing at /internal/path")}
		},
	})

	resp := postConnectionTest(t, h,
		`{"name":"pg","engine":"postgres","connector_type":"direct","connector_config":{"host":"127.0.0.1","port":5432}}`)
	got := decodeConnectionTest(t, resp)

	if got.Status != connectionTestFailed || got.Error == nil || got.Error.Code != "tool_missing" {
		t.Fatalf("response = %+v, want failed tool_missing", got)
	}
	// connector passed, auth failed, catalog not reached.
	if checkStatus(got, "connector") != connectionTestOK || checkStatus(got, "auth") != connectionTestFailed {
		t.Fatalf("checks = %+v, want connector ok + auth failed", got.Checks)
	}
	if checkStatus(got, "catalog") != "" {
		t.Fatalf("checks = %+v, want catalog omitted", got.Checks)
	}
	if strings.Contains(mustJSON(t, got), "/internal/path") {
		t.Fatalf("response leaks tool path: %s", mustJSON(t, got))
	}
}

func TestConnectionTestUnsupportedEngineFailsAtCatalogStage(t *testing.T) {
	h := connectionTestHandler(t, Deps{
		ConnectionCheck: func(context.Context, core.Connection) ([]string, error) {
			return nil, &connCheckError{Stage: stageCatalog, Code: "unsupported_engine", err: errors.New("nope")}
		},
	})

	resp := postConnectionTest(t, h,
		`{"name":"pg","engine":"postgres","connector_type":"direct","connector_config":{"host":"127.0.0.1","port":5432}}`)
	got := decodeConnectionTest(t, resp)

	if got.Status != connectionTestFailed || got.Error == nil || got.Error.Code != "unsupported_engine" {
		t.Fatalf("response = %+v, want failed unsupported_engine", got)
	}
	if checkStatus(got, "connector") != connectionTestOK || checkStatus(got, "auth") != connectionTestOK || checkStatus(got, "catalog") != connectionTestFailed {
		t.Fatalf("checks = %+v, want connector/auth ok + catalog failed", got.Checks)
	}
}

func TestConnectionTestSecretNotFoundConnCheckError(t *testing.T) {
	h := connectionTestHandler(t, Deps{
		ConnectionCheck: func(context.Context, core.Connection) ([]string, error) {
			return nil, &connCheckError{Stage: stageAuth, Code: "secret_not_found", err: errors.New("secret lookup")}
		},
	})

	resp := postConnectionTest(t, h,
		`{"name":"pg","engine":"postgres","connector_type":"direct","connector_config":{"host":"127.0.0.1","port":5432},"secret_ref":"secret://db/missing"}`)
	got := decodeConnectionTest(t, resp)

	if got.Status != connectionTestFailed || got.Error == nil || got.Error.Code != "secret_not_found" {
		t.Fatalf("response = %+v, want failed secret_not_found", got)
	}
	if checkStatus(got, "auth") != connectionTestFailed {
		t.Fatalf("checks = %+v, want auth failed", got.Checks)
	}
}

func TestConnectionTestConnectFailedSanitizesBackendError(t *testing.T) {
	h := connectionTestHandler(t, Deps{
		ConnectionCheck: func(context.Context, core.Connection) ([]string, error) {
			return nil, &connCheckError{
				Stage: stageAuth,
				Code:  "connect_failed",
				err:   errors.New("psql: could not connect to 10.0.0.7 as backup via /var/run/postgresql"),
			}
		},
	})

	resp := postConnectionTest(t, h,
		`{"name":"pg","engine":"postgres","connector_type":"direct","connector_config":{"host":"10.0.0.7","port":5432},"username":"backup"}`)
	got := decodeConnectionTest(t, resp)
	body := mustJSON(t, got)

	if got.Status != connectionTestFailed || got.Error == nil || got.Error.Code != "connect_failed" {
		t.Fatalf("response = %+v, want failed connect_failed", got)
	}
	if strings.Contains(body, "10.0.0.7") || strings.Contains(body, "backup") || strings.Contains(body, "/var/run") {
		t.Fatalf("response leaks backend error detail: %s", body)
	}
}

func TestConnectionTestFallsBackToConnectorStageForOpaqueError(t *testing.T) {
	h := connectionTestHandler(t, Deps{
		ConnectionCheck: func(context.Context, core.Connection) ([]string, error) {
			return nil, errors.New("something unclassified")
		},
	})

	resp := postConnectionTest(t, h,
		`{"name":"pg","engine":"postgres","connector_type":"direct","connector_config":{"host":"127.0.0.1","port":5432}}`)
	got := decodeConnectionTest(t, resp)

	if got.Error == nil || got.Error.Code != "connect_failed" {
		t.Fatalf("response = %+v, want connect_failed fallback", got)
	}
	if checkStatus(got, "connector") != connectionTestFailed {
		t.Fatalf("checks = %+v, want connector failed", got.Checks)
	}
}

// --- checkConnection instrumentation (real staged probe, no live DB) ---

func TestCheckConnectionToolMissingIsAuthStage(t *testing.T) {
	// direct connector opens without dialing, so a failing toolCheck lands the
	// failure at the auth stage — proving the connector stage genuinely passed.
	st := openTestStore(t)
	conn := core.Connection{
		Name: "pg", Engine: "postgres", ConnectorType: "direct",
		ConnectorConfig: json.RawMessage(`{"host":"127.0.0.1","port":5432}`),
	}
	toolCheck := func(string) error { return errors.New("psql not on PATH") }

	_, err := checkConnection(context.Background(), core.NewSecretResolver(st, nil), toolCheck, conn)
	var ce *connCheckError
	if !errors.As(err, &ce) || ce.Stage != stageAuth || ce.Code != "tool_missing" {
		t.Fatalf("err = %v, want auth/tool_missing connCheckError", err)
	}
}

func TestCheckConnectionMissingUsernameIsAuthStage(t *testing.T) {
	st := openTestStore(t)
	conn := core.Connection{
		Name: "pg", Engine: "postgres", ConnectorType: "direct",
		ConnectorConfig: json.RawMessage(`{"host":"127.0.0.1","port":5432}`),
	}
	toolCheck := func(string) error { return nil }

	_, err := checkConnection(context.Background(), core.NewSecretResolver(st, nil), toolCheck, conn)
	var ce *connCheckError
	if !errors.As(err, &ce) || ce.Stage != stageAuth || ce.Code != "username_required" {
		t.Fatalf("err = %v, want auth/username_required connCheckError", err)
	}
}

func TestCheckConnectionBadConnectorConfigIsConnectorStage(t *testing.T) {
	st := openTestStore(t)
	conn := core.Connection{
		Name: "pg", Engine: "postgres", ConnectorType: "direct",
		ConnectorConfig: json.RawMessage(`{"host":"","port":0}`),
	}
	toolCheck := func(string) error { return nil }

	_, err := checkConnection(context.Background(), core.NewSecretResolver(st, nil), toolCheck, conn)
	var ce *connCheckError
	if !errors.As(err, &ce) || ce.Stage != stageConnector || ce.Code != "invalid_config" {
		t.Fatalf("err = %v, want connector/invalid_config connCheckError", err)
	}
}

// TestCheckConnectionStopsAtConnectorForEngineWithoutCatalog is the guard for
// the check that could never go green: redis has no database list and no DB
// username, so running the auth and catalog stages against it failed every
// healthy connection.
func TestCheckConnectionStopsAtConnectorForEngineWithoutCatalog(t *testing.T) {
	st := openTestStore(t)
	toolCheck := func(string) error {
		t.Fatal("tool check ran for an engine with no catalog probe")
		return nil
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	for _, engine := range []string{"redis", "mongodb"} {
		conn := core.Connection{
			Name: engine, Engine: engine, ConnectorType: "direct",
			ConnectorConfig: json.RawMessage(fmt.Sprintf(`{"host":"127.0.0.1","port":%d}`, port)),
		}
		dbs, err := checkConnection(context.Background(), core.NewSecretResolver(st, nil), toolCheck, conn)
		if err != nil {
			t.Fatalf("%s: checkConnection = %v, want the probe to stop after the connector stage", engine, err)
		}
		if dbs != nil {
			t.Fatalf("%s: databases = %v, want none", engine, dbs)
		}
	}
}

// TestCheckConnectionDialsEndpointForEngineWithoutCatalog guards against a
// green check on an unreachable host: direct Open never dials, so without the
// explicit probe a closed port passed "Проверить соединение".
func TestCheckConnectionDialsEndpointForEngineWithoutCatalog(t *testing.T) {
	st := openTestStore(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // nothing listens on port now

	for _, engine := range []string{"redis", "mongodb"} {
		conn := core.Connection{
			Name: engine, Engine: engine, ConnectorType: "direct",
			ConnectorConfig: json.RawMessage(fmt.Sprintf(`{"host":"127.0.0.1","port":%d}`, port)),
		}
		_, err := checkConnection(context.Background(), core.NewSecretResolver(st, nil), nil, conn)
		var ce *connCheckError
		if !errors.As(err, &ce) || ce.Stage != stageConnector || ce.Code != "connect_failed" {
			t.Fatalf("%s: err = %v, want connector/connect_failed connCheckError", engine, err)
		}
	}
}

// TestConnectionTestReportsSkippedStages proves the untried stages are reported
// as skipped, not painted green: the UI must not claim an authentication that
// never happened.
func TestConnectionTestReportsSkippedStages(t *testing.T) {
	h := connectionTestHandler(t, Deps{
		ConnectionCheck: func(context.Context, core.Connection) ([]string, error) { return nil, nil },
	})

	resp := postConnectionTest(t, h,
		`{"name":"cache","engine":"redis","connector_type":"direct","connector_config":{"host":"127.0.0.1","port":6379}}`)
	got := decodeConnectionTest(t, resp)

	if got.Status != connectionTestOK || got.Error != nil {
		t.Fatalf("response = %+v, want ok without error", got)
	}
	for _, key := range []string{"config", "connector"} {
		if checkStatus(got, key) != connectionTestOK {
			t.Fatalf("check %q = %q, want ok (checks=%+v)", key, checkStatus(got, key), got.Checks)
		}
	}
	for _, key := range []string{"auth", "catalog"} {
		if checkStatus(got, key) != connectionTestSkipped {
			t.Fatalf("check %q = %q, want skipped (checks=%+v)", key, checkStatus(got, key), got.Checks)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
