package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestOversizedBodyIsRefused: without a bound, json.Decoder reads until the
// client stops sending, so one request grows the process by as much memory as
// the sender cares to spend. The limit is enforced as middleware rather than
// per handler so a new endpoint cannot forget it.
func TestOversizedBodyIsRefused(t *testing.T) {
	srv, _ := authServer(t)
	token := loginAs(t, srv, "admin")

	// A syntactically valid object whose single string value dwarfs the cap.
	var b bytes.Buffer
	b.WriteString(`{"name":"`)
	b.WriteString(strings.Repeat("A", maxBodyBytes+1024))
	b.WriteString(`"}`)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/tasks", &b)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 — %s", resp.StatusCode, readAll(t, resp))
	}
}

// TestUnauthenticatedOversizedBodyIsRefused: the limit sits above the
// authentication middleware, so an anonymous caller cannot spend the server's
// memory on the one public route either.
func TestUnauthenticatedOversizedBodyIsRefused(t *testing.T) {
	srv, _ := authServer(t)

	var b bytes.Buffer
	b.WriteString(`{"email":"`)
	b.WriteString(strings.Repeat("A", maxBodyBytes+1024))
	b.WriteString(`","password":"x"}`)

	resp, err := http.Post(srv.URL+"/api/login", "application/json", &b)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// login runs its own decoder and reports a malformed body; either refusal
	// is fine, being read in full is not.
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("an oversized anonymous body was accepted: %s", readAll(t, resp))
	}
}

// TestNormalBodyStillFits guards the other direction: the cap must sit well
// above a real request, including one carrying an inline PEM private key.
func TestNormalBodyStillFits(t *testing.T) {
	srv, _ := authServer(t)
	token := loginAs(t, srv, "operator")

	body, err := json.Marshal(map[string]any{
		"name": "prod-db", "engine": "postgres", "connector_type": "direct",
		"connector_config": map[string]any{
			"host": "db.corp.io", "port": 5432,
			// A 4096-bit PEM key is a few kilobytes; this is larger than any.
			"private_key": strings.Repeat("K", 8*1024),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/connections", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		t.Fatalf("the cap refused an ordinary request with an inline key")
	}
}
