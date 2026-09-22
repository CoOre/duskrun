package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/duskrun/duskrun/internal/core"

	// The write path validates the connector and storage types against the
	// plugin registry, so the tests must link the same plugins the daemon does.
	// Without these, "unknown connector type" would mask the leak under test.
	_ "github.com/duskrun/duskrun/internal/connector/sshtunnel"
	_ "github.com/duskrun/duskrun/internal/storage/s3"
)

// readAll drains a response body into a string for substring assertions.
func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestViewerCannotReadInlineCredentials is the end-to-end form of the masking
// unit tests, and the one that matters: the listings are what a `viewer`
// actually calls.
//
// The role is defined as "may see that backups are happening". Both listings
// used to return the plugin config verbatim — the DTO comments even claimed
// they omitted it — while the API accepts an inline private_key or secret_key
// just as readily as a *_ref. Read-only therefore included reading the SSH key
// to the database host and the S3 credentials for the backup bucket.
func TestViewerCannotReadInlineCredentials(t *testing.T) {
	srv, _ := authServer(t)
	operator := loginAs(t, srv, core.RoleOperator)
	viewer := loginAs(t, srv, core.RoleViewer)

	const privateKey = "-----BEGIN OPENSSH PRIVATE KEY-----NOPE"
	const secretKey = "s3cr3t-key-material"

	create(t, srv, operator, "/api/connections", map[string]any{
		"name": "prod-db", "engine": "postgres", "connector_type": "ssh-tunnel",
		"connector_config": map[string]any{
			"ssh_host": "db.corp.io", "ssh_user": "backup",
			"remote_host": "127.0.0.1", "remote_port": 5432,
			"private_key": privateKey,
		},
	})
	create(t, srv, operator, "/api/storages", map[string]any{
		"name": "prod-bucket", "type": "s3",
		"config": map[string]any{
			"bucket": "backups", "region": "eu-central-1",
			"access_key": "AKIA123", "secret_key": secretKey,
		},
	})

	for _, c := range []struct{ path, leaked, kept string }{
		{"/api/connections", privateKey, "db.corp.io"},
		{"/api/storages", secretKey, "backups"},
	} {
		t.Run(c.path, func(t *testing.T) {
			resp := doJSON(t, srv, http.MethodGet, c.path, viewer, nil)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			body := readAll(t, resp)
			if strings.Contains(body, c.leaked) {
				t.Fatalf("a viewer read a credential out of %s:\n%s", c.path, body)
			}
			// The masking must not go so far that the listing stops being
			// usable: the form needs the host and the bucket to render.
			if !strings.Contains(body, c.kept) {
				t.Fatalf("%s lost %q, which is not a credential:\n%s", c.path, c.kept, body)
			}
		})
	}
}

// create POSTs a row and fails the test unless it lands.
func create(t *testing.T, srv *httptest.Server, token, path string, body any) {
	t.Helper()
	resp := doJSON(t, srv, http.MethodPost, path, token, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s: status %d — %s", path, resp.StatusCode, readAll(t, resp))
	}
}

// TestOperatorEditKeepsInlineCredential: masking on read is only safe if the
// write path restores what it hid. An operator changing the port through a
// read-modify-write client must not blank the key it never saw.
func TestOperatorEditKeepsInlineCredential(t *testing.T) {
	srv, st := authServer(t)
	operator := loginAs(t, srv, core.RoleOperator)

	const privateKey = "-----BEGIN OPENSSH PRIVATE KEY-----NOPE"
	create(t, srv, operator, "/api/connections", map[string]any{
		"name": "prod-db", "engine": "postgres", "connector_type": "ssh-tunnel",
		"connector_config": map[string]any{
			"ssh_host": "db.corp.io", "ssh_user": "backup",
			"remote_host": "127.0.0.1", "remote_port": 5432,
			"private_key": privateKey,
		},
	})

	// Read the masked listing, edit one field, send the whole config back.
	var list []connDTO
	decodeInto(t, authGet(t, srv.URL+"/api/connections"), &list)
	if len(list) != 1 {
		t.Fatalf("got %d connections, want 1", len(list))
	}
	var cfg map[string]any
	if err := json.Unmarshal(list[0].ConnectorConfig, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["private_key"] != secretMask {
		t.Fatalf("private_key = %v, want it masked before the round trip", cfg["private_key"])
	}
	cfg["remote_port"] = 5433

	resp := doJSON(t, srv, http.MethodPatch, "/api/connections/"+itoa(list[0].ID), operator, map[string]any{
		"name": "prod-db", "engine": "postgres", "connector_type": "ssh-tunnel",
		"connector_config": cfg,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH: status %d — %s", resp.StatusCode, readAll(t, resp))
	}

	// Read the stored row directly: the API would only ever show the mask.
	stored, err := st.GetConnection(t.Context(), list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(stored.ConnectorConfig, &got); err != nil {
		t.Fatal(err)
	}
	if got["private_key"] != privateKey {
		t.Fatalf("private_key = %v, want the real key preserved through the edit", got["private_key"])
	}
	if got["remote_port"] != float64(5433) {
		t.Fatalf("remote_port = %v, want the edit to land", got["remote_port"])
	}
}
