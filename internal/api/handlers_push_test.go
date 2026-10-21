package api

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/CoOre/duskrun/internal/core"
	"github.com/CoOre/duskrun/internal/notifier/webpush"
	"github.com/CoOre/duskrun/internal/plugin"
	"github.com/CoOre/duskrun/internal/store/sqlite"
)

// stubPush is a PushService that accepts endpoints on push.test and records
// what it was asked to send.
type stubPush struct {
	reason  string
	sendErr error

	mu   sync.Mutex
	sent []core.PushSubscription
}

func (p *stubPush) Available() (string, string) {
	if p.reason != "" {
		return "", p.reason
	}
	return "BPUBLICKEY", ""
}

func (p *stubPush) CheckEndpoint(endpoint string) error {
	if !strings.HasPrefix(endpoint, "https://push.test/") {
		return webpushErr("not a known push service")
	}
	return nil
}

func (p *stubPush) SendTo(_ context.Context, sub core.PushSubscription, ev plugin.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ev.Kind != eventTest {
		return webpushErr("unexpected kind " + string(ev.Kind))
	}
	p.sent = append(p.sent, sub)
	return p.sendErr
}

type webpushErr string

func (e webpushErr) Error() string { return string(e) }

func pushServer(t *testing.T, push *stubPush) (*httptest.Server, *sqlite.Store) {
	t.Helper()
	return authServerWith(t, Deps{Push: push})
}

// browserKeys returns a valid p256dh/auth pair, as a browser would send it.
func browserKeys(t *testing.T) map[string]string {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	return map[string]string{
		"p256dh": base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()),
		"auth":   base64.RawURLEncoding.EncodeToString(auth),
	}
}

func subscribe(t *testing.T, srv *httptest.Server, token, endpoint string, want int) int64 {
	t.Helper()
	resp := doJSON(t, srv, http.MethodPost, "/api/push/subscriptions", token,
		map[string]any{"endpoint": endpoint, "keys": browserKeys(t)})
	if want != http.StatusOK {
		if got := status(t, resp); got != want {
			t.Fatalf("subscribe %s: status %d, want %d", endpoint, got, want)
		}
		return 0
	}
	return decodeBody[map[string]int64](t, resp, http.StatusOK)["id"]
}

func TestPushStatus(t *testing.T) {
	srv, _ := pushServer(t, &stubPush{})
	tok := loginAs(t, srv, core.RoleViewer)
	got := decodeBody[pushStatusDTO](t, doJSON(t, srv, http.MethodGet, "/api/push", tok, nil), http.StatusOK)
	if !got.Available || got.PublicKey != "BPUBLICKEY" || got.Reason != "" {
		t.Fatalf("status = %+v", got)
	}

	off, _ := pushServer(t, &stubPush{reason: webpush.ReasonInsecureOrigin})
	tok = loginAs(t, off, core.RoleViewer)
	got = decodeBody[pushStatusDTO](t, doJSON(t, off, http.MethodGet, "/api/push", tok, nil), http.StatusOK)
	if got.Available || got.Reason != webpush.ReasonInsecureOrigin || got.PublicKey != "" {
		t.Fatalf("unavailable status = %+v", got)
	}
	subscribe(t, off, tok, "https://push.test/a", http.StatusConflict)

	none, _ := authServerWith(t, Deps{})
	tok = loginAs(t, none, core.RoleViewer)
	got = decodeBody[pushStatusDTO](t, doJSON(t, none, http.MethodGet, "/api/push", tok, nil), http.StatusOK)
	if got.Available {
		t.Fatal("no push service, yet reported available")
	}
}

func TestPushSubscribeListDelete(t *testing.T) {
	srv, _ := pushServer(t, &stubPush{})
	viewer := loginAs(t, srv, core.RoleViewer)
	admin := loginAs(t, srv, core.RoleAdmin)

	id := subscribe(t, srv, viewer, "https://push.test/v1", http.StatusOK)
	// The same browser saving again is the same row.
	if again := subscribe(t, srv, viewer, "https://push.test/v1", http.StatusOK); again != id {
		t.Fatalf("resubscribe id = %d, want %d", again, id)
	}
	subscribe(t, srv, admin, "https://push.test/a1", http.StatusOK)

	list := decodeBody[[]pushSubscriptionDTO](t, doJSON(t, srv, http.MethodGet, "/api/push/subscriptions", viewer, nil), http.StatusOK)
	if len(list) != 1 || list[0].ID != id || list[0].Service != "push.test" {
		t.Fatalf("viewer's list = %+v; want only their own browser", list)
	}

	// Someone else's subscription is not found, not forbidden.
	if got := status(t, doJSON(t, srv, http.MethodDelete, "/api/push/subscriptions/"+itoa(id), admin, nil)); got != http.StatusNotFound {
		t.Fatalf("delete another user's: status %d, want 404", got)
	}
	if got := status(t, doJSON(t, srv, http.MethodDelete, "/api/push/subscriptions/"+itoa(id), viewer, nil)); got != http.StatusNoContent {
		t.Fatalf("delete own: status %d, want 204", got)
	}
	list = decodeBody[[]pushSubscriptionDTO](t, doJSON(t, srv, http.MethodGet, "/api/push/subscriptions", viewer, nil), http.StatusOK)
	if len(list) != 0 {
		t.Fatalf("after delete: %+v", list)
	}
}

func TestPushUnsubscribeByEndpoint(t *testing.T) {
	srv, st := pushServer(t, &stubPush{})
	viewer := loginAs(t, srv, core.RoleViewer)
	admin := loginAs(t, srv, core.RoleAdmin)
	subscribe(t, srv, admin, "https://push.test/a1", http.StatusOK)

	// Another user cannot unsubscribe a browser that is not theirs.
	if got := status(t, doJSON(t, srv, http.MethodPost, "/api/push/unsubscribe", viewer,
		map[string]string{"endpoint": "https://push.test/a1"})); got != http.StatusNoContent {
		t.Fatalf("status %d, want 204", got)
	}
	if targets, _ := st.PushTargets(context.Background()); len(targets) != 1 {
		t.Fatal("a user removed another user's subscription")
	}
	if got := status(t, doJSON(t, srv, http.MethodPost, "/api/push/unsubscribe", admin,
		map[string]string{"endpoint": "https://push.test/a1"})); got != http.StatusNoContent {
		t.Fatalf("status %d, want 204", got)
	}
	if targets, _ := st.PushTargets(context.Background()); len(targets) != 0 {
		t.Fatal("own unsubscribe left the subscription")
	}
}

func TestPushSubscribeValidation(t *testing.T) {
	srv, _ := pushServer(t, &stubPush{})
	tok := loginAs(t, srv, core.RoleViewer)

	// Any signed-in user can store an endpoint the server will later POST to;
	// one outside the push services must be refused.
	subscribe(t, srv, tok, "https://169.254.169.254/latest", http.StatusBadRequest)

	good := browserKeys(t)
	for name, keys := range map[string]map[string]string{
		"short p256dh":     {"p256dh": base64.RawURLEncoding.EncodeToString(make([]byte, 33)), "auth": good["auth"]},
		"not base64":       {"p256dh": "!!!", "auth": good["auth"]},
		"long auth":        {"p256dh": good["p256dh"], "auth": base64.RawURLEncoding.EncodeToString(make([]byte, 32))},
		"missing auth key": {"p256dh": good["p256dh"]},
	} {
		resp := doJSON(t, srv, http.MethodPost, "/api/push/subscriptions", tok,
			map[string]any{"endpoint": "https://push.test/x", "keys": keys})
		if got := status(t, resp); got != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, got)
		}
	}
}

// TestPushNeedsAUser: the static token has no browser to subscribe and no
// user row to own a subscription.
func TestPushNeedsAUser(t *testing.T) {
	srv, _ := pushServer(t, &stubPush{})
	resp := doJSON(t, srv, http.MethodPost, "/api/push/subscriptions", testToken,
		map[string]any{"endpoint": "https://push.test/x", "keys": browserKeys(t)})
	if got := status(t, resp); got != http.StatusForbidden {
		t.Fatalf("API token subscribe: status %d, want 403", got)
	}
	if got := status(t, doJSON(t, srv, http.MethodGet, "/api/push/subscriptions", testToken, nil)); got != http.StatusForbidden {
		t.Fatalf("API token list: status %d, want 403", got)
	}
	// The instance-level status is fine to read with the token.
	if got := status(t, doJSON(t, srv, http.MethodGet, "/api/push", testToken, nil)); got != http.StatusOK {
		t.Fatalf("API token status: %d, want 200", got)
	}
}

func TestPushTestDevice(t *testing.T) {
	push := &stubPush{}
	srv, _ := pushServer(t, push)
	viewer := loginAs(t, srv, core.RoleViewer)
	admin := loginAs(t, srv, core.RoleAdmin)
	id := subscribe(t, srv, viewer, "https://push.test/v1", http.StatusOK)

	if got := status(t, doJSON(t, srv, http.MethodPost, "/api/push/subscriptions/"+itoa(id)+"/test", admin, nil)); got != http.StatusNotFound {
		t.Fatalf("test another user's device: status %d, want 404", got)
	}
	if got := status(t, doJSON(t, srv, http.MethodPost, "/api/push/subscriptions/"+itoa(id)+"/test", viewer, nil)); got != http.StatusNoContent {
		t.Fatalf("test own device: status %d, want 204", got)
	}
	if len(push.sent) != 1 || push.sent[0].Endpoint != "https://push.test/v1" {
		t.Fatalf("sent = %+v", push.sent)
	}

	push.sendErr = webpush.ErrGone
	if got := status(t, doJSON(t, srv, http.MethodPost, "/api/push/subscriptions/"+itoa(id)+"/test", viewer, nil)); got != http.StatusGone {
		t.Fatalf("gone device: status %d, want 410", got)
	}
	push.sendErr = webpushErr("push: service answered 403")
	resp := doJSON(t, srv, http.MethodPost, "/api/push/subscriptions/"+itoa(id)+"/test", viewer, nil)
	if got := status(t, resp); got != http.StatusBadGateway {
		t.Fatalf("failing device: status %d, want 502", got)
	}
}

// TestPushChannelRefusedWhenUnavailable: a push channel on an instance that
// cannot send must be refused when saved, with the reason, not accepted and
// then fail on every backup.
func TestPushChannelRefusedWhenUnavailable(t *testing.T) {
	registerUnavailablePush()
	srv, _ := authServerWith(t, Deps{Notify: core.NewNotifications(nil, nil, core.NotificationsConfig{}, nil)})
	tok := loginAs(t, srv, core.RoleAdmin)
	resp := doJSON(t, srv, http.MethodPost, "/api/notifiers", tok, map[string]any{
		"name": "browsers", "type": "push", "config": map[string]string{"min_role": "viewer"},
	})
	defer resp.Body.Close()
	var e map[string]string
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if !strings.Contains(e["error"], "push") {
		t.Fatalf("error = %q; want the push reason", e["error"])
	}
}

var pushRegistered sync.Once

// registerUnavailablePush adds the push type to the global registry the
// channel endpoints consult, as serve does, but with no service behind it.
func registerUnavailablePush() {
	pushRegistered.Do(func() {
		if !plugin.Notifiers.Has(webpush.Type) {
			webpush.Register(plugin.Notifiers, nil)
		}
	})
}
