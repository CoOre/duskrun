package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/duskrun/duskrun/internal/secret"
	"github.com/duskrun/duskrun/internal/store/sqlite"
)

// secretServer returns an API server whose Deps include a real Sealer, so the
// POST /secrets path exercises envelope encryption end-to-end.
func secretServer(t *testing.T) (*httptest.Server, *sqlite.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.db")
	st, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, err := secret.NewBox([]byte("test-master-key"), "env:v1")
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	srv := httptest.NewServer(NewRouter(Deps{
		Store: st, Token: testToken, Secrets: box,
		ToolCheck: func(string) error { return nil },
	}))
	t.Cleanup(srv.Close)
	return srv, st
}

// TestCreateSecretSealsAndLists: POST /secrets seals a value and GET /secrets
// returns its metadata without ever exposing the ciphertext or value.
func TestCreateSecretSealsAndLists(t *testing.T) {
	srv, st := secretServer(t)

	resp := authPost(t, srv.URL+"/api/secrets",
		`{"name":"db/pg-primary","type":"db-password","value":"hunter2"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	// The stored secret decrypts back to the original plaintext.
	sec, err := st.GetSecretByRef(context.Background(), "secret://db/pg-primary")
	if err != nil {
		t.Fatalf("GetSecretByRef: %v", err)
	}
	box, _ := secret.NewBox([]byte("test-master-key"), "env:v1")
	pt, err := box.Open(*sec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(pt) != "hunter2" {
		t.Fatalf("plaintext = %q, want hunter2", pt)
	}

	// The list endpoint returns metadata only — no value/ciphertext field.
	lresp := authGet(t, srv.URL+"/api/secrets")
	defer lresp.Body.Close()
	var raw []map[string]any
	if err := json.NewDecoder(lresp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw) != 1 {
		t.Fatalf("len = %d, want 1", len(raw))
	}
	if raw[0]["name"] != "db/pg-primary" || raw[0]["type"] != "db-password" {
		t.Fatalf("unexpected metadata: %+v", raw[0])
	}
	if _, ok := raw[0]["value"]; ok {
		t.Fatal("list leaked value field")
	}
	if _, ok := raw[0]["ciphertext"]; ok {
		t.Fatal("list leaked ciphertext field")
	}
}

// TestCreateSecretValidation: missing fields are rejected with 400.
func TestCreateSecretValidation(t *testing.T) {
	srv, _ := secretServer(t)
	resp := authPost(t, srv.URL+"/api/secrets", `{"name":"x","type":"db-password"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestCreateSecretNoSealer: without a configured Sealer the endpoint is 503.
func TestCreateSecretNoSealer(t *testing.T) {
	srv := emptyServer(t)
	resp := authPost(t, srv.URL+"/api/secrets",
		`{"name":"x","type":"db-password","value":"v"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}
