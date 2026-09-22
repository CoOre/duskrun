package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// builtFS stands in for a real Vite output: an index.html referencing one
// fingerprinted script, plus that script.
func builtFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html": {Data: []byte(
			`<!doctype html><html><head><title>Duskrun</title>` +
				`<script type="module" src="/assets/index-ABC123.js"></script></head>` +
				`<body><div id="root"></div></body></html>`)},
		"assets/index-ABC123.js": {Data: []byte("console.log('spa')")},
	}
}

// unbuiltFS stands in for a binary built without the frontend step: dist/ holds
// only the marker that keeps go:embed compiling.
func unbuiltFS() fstest.MapFS {
	return fstest.MapFS{".gitkeep": {Data: []byte{}}}
}

func get(t *testing.T, h http.Handler, path string) (int, string, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(body)
}

// TestServesIndexAtRoot: GET / returns the embedded index.html as HTML.
func TestServesIndexAtRoot(t *testing.T) {
	status, ct, body := get(t, handlerFor(builtFS()), "/")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(ct, "text/html") {
		t.Fatalf("content-type = %q, want text/html", ct)
	}
	if !strings.Contains(body, "index-ABC123.js") {
		t.Fatalf("body is not the built index: %q", body)
	}
}

// TestSPAFallback: an unknown client-route path returns index.html (200), so
// deep links work with client-side routing.
func TestSPAFallback(t *testing.T) {
	status, ct, body := get(t, handlerFor(builtFS()), "/tasks")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(ct, "text/html") {
		t.Fatalf("content-type = %q, want text/html", ct)
	}
	if !strings.Contains(body, "index-ABC123.js") {
		t.Fatalf("fallback body is not the SPA index: %q", body)
	}
}

// TestAssetIsServedWithImmutableCaching: a present asset is served as itself,
// not routed through the SPA fallback.
func TestAssetIsServedWithImmutableCaching(t *testing.T) {
	srv := httptest.NewServer(handlerFor(builtFS()))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/assets/index-ABC123.js")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("cache-control = %q, want immutable", cc)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "console.log") {
		t.Fatalf("asset body = %q", string(body))
	}
}

// TestMissingAssetIs404NotHTML is the regression test for the failure that
// shipped: a committed index.html pointing at gitignored asset filenames.
//
// The SPA fallback used to answer a missing asset with index.html at status
// 200 and Content-Type text/html. The browser refuses that as a module on MIME
// type and renders nothing, while the server records a success — a blank page
// with no failure anywhere the operator would look. A 404 names the problem.
func TestMissingAssetIs404NotHTML(t *testing.T) {
	status, ct, _ := get(t, handlerFor(builtFS()), "/assets/index-GONE999.js")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — a missing asset must not fall back to HTML", status)
	}
	if strings.Contains(ct, "text/html") && status == http.StatusOK {
		t.Fatalf("content-type = %q at 200: the browser would refuse this silently", ct)
	}
}

// TestUnbuiltBinaryExplainsItself: a binary built without `make ui` must say so
// rather than serve a broken page or a 500.
func TestUnbuiltBinaryExplainsItself(t *testing.T) {
	h := handlerFor(unbuiltFS())

	status, ct, body := get(t, h, "/")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(ct, "text/html") {
		t.Fatalf("content-type = %q, want text/html", ct)
	}
	if !strings.Contains(body, "make release") {
		t.Fatalf("the placeholder does not tell the operator how to fix it: %q", body)
	}

	// Deep links get the same explanation, not a 404 that reads as a bad URL.
	if status, _, _ := get(t, h, "/tasks"); status != http.StatusOK {
		t.Fatalf("deep link status = %d, want 200", status)
	}
	// Assets 404 rather than serving the placeholder as JavaScript.
	if status, _, _ := get(t, h, "/assets/index-ABC123.js"); status != http.StatusNotFound {
		t.Fatalf("asset status = %d, want 404", status)
	}
}

// TestEmbeddedPlaceholderNeverReferencesAssets: the committed placeholder must
// not name a hashed bundle. That is precisely the mistake being fixed, and it
// would reintroduce the blank page the moment someone regenerated the file.
func TestEmbeddedPlaceholderNeverReferencesAssets(t *testing.T) {
	if strings.Contains(string(Placeholder), "/assets/") {
		t.Fatal("placeholder.html references /assets/ — those filenames are generated and gitignored")
	}
}
