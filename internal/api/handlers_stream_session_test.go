package api

import (
	"bufio"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/duskrun/duskrun/internal/auth"
	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/store/sqlite"
)

// The SSE run stream was only ever tested with the static API token, which has
// no session at all and therefore skips the recheck entirely. These tests cover
// the path a real user takes: a session token, revalidated on every heartbeat
// for as long as the backup runs.
//
// Both directions matter and they pull against each other. The stream must
// survive an arbitrary number of heartbeats for a session that is still good —
// a stream that quietly dies after the first recheck looks exactly like a
// backup that stopped producing output. And it must end promptly when the
// session stops being good, because "disabling an account ends access
// immediately" is worth nothing if an open stream keeps feeding run output to a
// revoked credential.

// sessionStreamServer builds a server with both a Hub and real user accounts,
// plus one running run to subscribe to.
func sessionStreamServer(t *testing.T) (*httptest.Server, *sqlite.Store, *core.Hub) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stream.db")
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

	hash, err := auth.Hash(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, r := range []core.Role{core.RoleAdmin, core.RoleOperator, core.RoleViewer} {
		if _, err := st.CreateUser(ctx, core.User{
			Email: string(r) + "@corp.io", Name: string(r), Role: r, PasswordHash: hash,
		}); err != nil {
			t.Fatalf("seed %s: %v", r, err)
		}
	}

	mustExec(t, db, `INSERT INTO connection (name, engine, connector_type, created_at) VALUES ('c','postgres','direct', unixepoch())`)
	mustExec(t, db, `INSERT INTO storage (name, type, created_at) VALUES ('s','localfs', unixepoch())`)
	mustExec(t, db, `INSERT INTO task (name, connection_id, storage_id, codec_chain, cron, enabled, created_at)
		VALUES ('nightly', 1, 1, '["zstd"]', '0 2 * * *', 1, unixepoch())`)
	mustExec(t, db, `INSERT INTO run (task_id, status, worker, attempt, started_at, created_at)
		VALUES (1, 'running', 'w0', 1, unixepoch(), unixepoch())`)

	hub := core.NewHub(nil)
	// Publish once so the Hub considers run 1 live. Without this Subscribe
	// reports "not live", the handler replays the stored log and returns, and
	// every test below would measure a stream that was never open.
	hub.Phase(1, core.PhaseStream)

	srv := httptest.NewServer(NewRouter(Deps{Store: st, Token: testToken, Hub: hub}))
	t.Cleanup(srv.Close)
	return srv, st, hub
}

// shortHeartbeat speeds the recheck up for the duration of one test.
func shortHeartbeat(t *testing.T, d time.Duration) {
	t.Helper()
	prev := sseHeartbeat
	sseHeartbeat = d
	t.Cleanup(func() { sseHeartbeat = prev })
}

// openStream subscribes to the run's event stream with the given bearer token
// and returns a channel of raw lines, closed when the server ends the stream.
func openStream(t *testing.T, srv *httptest.Server, token string) (<-chan string, func()) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/runs/1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}

	lines := make(chan string, 256)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	return lines, func() { resp.Body.Close() }
}

// TestStreamSurvivesHeartbeatsUnderSessionToken: a live session must keep the
// stream open across many rechecks. This is the regression test for the recheck
// looking the session up by the wrong value — any mismatch there fails the very
// first heartbeat and closes the stream a few seconds into every backup.
func TestStreamSurvivesHeartbeatsUnderSessionToken(t *testing.T) {
	shortHeartbeat(t, 30*time.Millisecond)
	srv, _, hub := sessionStreamServer(t)
	token := loginAs(t, srv, core.RoleViewer)

	lines, closeStream := openStream(t, srv, token)
	defer closeStream()

	// Well past a dozen heartbeats.
	deadline := time.After(600 * time.Millisecond)
	keepAlives := 0
	for keepAlives < 5 {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("stream closed after %d heartbeats — a live session was treated as dead", keepAlives)
			}
			if strings.HasPrefix(line, ": keep-alive") {
				keepAlives++
			}
		case <-deadline:
			t.Fatalf("only %d heartbeats arrived; the stream stalled", keepAlives)
		}
	}
	hub.Close(1)
}

// TestStreamEndsWhenSessionIsDeleted: revoking a session must take the open
// stream down with it, not just block the next request.
func TestStreamEndsWhenSessionIsDeleted(t *testing.T) {
	shortHeartbeat(t, 30*time.Millisecond)
	srv, st, _ := sessionStreamServer(t)
	token := loginAs(t, srv, core.RoleViewer)

	lines, closeStream := openStream(t, srv, token)
	defer closeStream()

	// Wait for one heartbeat first, so the close below is demonstrably the
	// recheck acting and not the stream never having worked.
	waitKeepAlive(t, lines)

	ctx := context.Background()
	sessions, err := st.ListSessions(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) == 0 {
		t.Fatal("no session to revoke")
	}
	if err := st.DeleteSession(ctx, sessions[0].ID); err != nil {
		t.Fatal(err)
	}

	waitClosed(t, lines, "a revoked session kept receiving run output")
}

// TestStreamEndsWhenUserIsDisabled: same for the account, which is the control
// an admin actually reaches for.
func TestStreamEndsWhenUserIsDisabled(t *testing.T) {
	shortHeartbeat(t, 30*time.Millisecond)
	srv, st, _ := sessionStreamServer(t)
	token := loginAs(t, srv, core.RoleViewer)

	lines, closeStream := openStream(t, srv, token)
	defer closeStream()
	waitKeepAlive(t, lines)

	ctx := context.Background()
	user, err := st.GetUserByEmail(ctx, string(core.RoleViewer)+"@corp.io")
	if err != nil {
		t.Fatal(err)
	}
	user.Disabled = true
	if err := st.UpdateUser(ctx, *user); err != nil {
		t.Fatal(err)
	}

	waitClosed(t, lines, "a disabled account kept receiving run output")
}

// waitKeepAlive blocks until one heartbeat comment arrives.
func waitKeepAlive(t *testing.T, lines <-chan string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stream closed before the first heartbeat")
			}
			if strings.HasPrefix(line, ": keep-alive") {
				return
			}
		case <-deadline:
			t.Fatal("no heartbeat arrived")
		}
	}
}

// waitClosed blocks until the server ends the stream, failing with msg if it
// keeps sending instead.
func waitClosed(t *testing.T, lines <-chan string, msg string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-lines:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal(msg)
		}
	}
}
