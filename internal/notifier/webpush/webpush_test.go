package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CoOre/duskrun/internal/core"
	"github.com/CoOre/duskrun/internal/plugin"
	"github.com/CoOre/duskrun/internal/secret"
)

// memStore is an in-memory Store.
type memStore struct {
	mu      sync.Mutex
	keys    *core.VAPIDKeys
	subs    []core.PushSubscription
	results map[int64]string // last MarkPushResult cause per subscription
}

func newMemStore() *memStore { return &memStore{results: map[int64]string{}} }

func (m *memStore) InitVAPIDKeys(_ context.Context, k core.VAPIDKeys) (*core.VAPIDKeys, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keys == nil {
		m.keys = &k
	}
	cp := *m.keys
	return &cp, nil
}

func (m *memStore) PushTargets(context.Context) ([]core.PushSubscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]core.PushSubscription(nil), m.subs...), nil
}

func (m *memStore) DeletePushSubscriptionByEndpoint(_ context.Context, _ int64, endpoint string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.subs[:0]
	for _, s := range m.subs {
		if s.Endpoint != endpoint {
			kept = append(kept, s)
		}
	}
	m.subs = kept
	return nil
}

func (m *memStore) MarkPushResult(_ context.Context, id int64, cause string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.results[id] = cause
	return nil
}

func (m *memStore) result(id int64) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.results[id]
	return c, ok
}

func testBox(t *testing.T, key string) *secret.Box {
	t.Helper()
	b, err := secret.NewBox([]byte(key), "test")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// browser is the subscriber side: the key pair and auth secret a browser
// generates, used here to decrypt what the server sent.
type browser struct {
	priv *ecdh.PrivateKey
	auth []byte
}

func newBrowser(t *testing.T) *browser {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	return &browser{priv: priv, auth: auth}
}

func (b *browser) sub(id int64, endpoint string, role core.Role) core.PushSubscription {
	return core.PushSubscription{
		ID: id, UserID: id, Endpoint: endpoint, Role: role,
		P256dh: base64.RawURLEncoding.EncodeToString(b.priv.PublicKey().Bytes()),
		Auth:   base64.RawURLEncoding.EncodeToString(b.auth),
	}
}

// decrypt reverses RFC 8291 §3.4 / RFC 8188 for a single record.
func (b *browser) decrypt(t *testing.T, body []byte) []byte {
	t.Helper()
	if len(body) < 21 {
		t.Fatalf("body too short: %d bytes", len(body))
	}
	salt, idLen := body[:16], int(body[20])
	_ = binary.BigEndian.Uint32(body[16:20]) // record size
	serverPub, ciphertext := body[21:21+idLen], body[21+idLen:]

	pub, err := ecdh.P256().NewPublicKey(serverPub)
	if err != nil {
		t.Fatalf("server key: %v", err)
	}
	shared, err := b.priv.ECDH(pub)
	if err != nil {
		t.Fatal(err)
	}
	info := append(append([]byte("WebPush: info\x00"), b.priv.PublicKey().Bytes()...), serverPub...)
	ikm, _ := hkdf.Key(sha256.New, shared, b.auth, string(info), 32)
	cek, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)

	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	// Strip padding: zeros after the 0x02 last-record delimiter.
	plain = bytes.TrimRight(plain, "\x00")
	if len(plain) == 0 || plain[len(plain)-1] != 0x02 {
		t.Fatal("missing record delimiter")
	}
	return plain[:len(plain)-1]
}

// pushServer stands in for a browser vendor's push service.
type pushServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []recorded
	// status answers a request to path; 201 when absent.
	status map[string]int
}

type recorded struct {
	path   string
	header http.Header
	body   []byte
}

func newPushServer(t *testing.T) *pushServer {
	t.Helper()
	ps := &pushServer{status: map[string]int{}}
	ps.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ps.mu.Lock()
		ps.reqs = append(ps.reqs, recorded{path: r.URL.Path, header: r.Header.Clone(), body: body})
		code, ok := ps.status[r.URL.Path]
		ps.mu.Unlock()
		if !ok {
			code = http.StatusCreated
		}
		if code == http.StatusFound {
			http.Redirect(w, r, "/elsewhere", code)
			return
		}
		w.WriteHeader(code)
		if code >= 400 {
			_, _ = io.WriteString(w, `{"reason":"BadJwtToken"}`)
		}
	}))
	t.Cleanup(ps.Close)
	return ps
}

func (ps *pushServer) requests() []recorded {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return append([]recorded(nil), ps.reqs...)
}

func newService(t *testing.T, st Store, ps *pushServer) *Service {
	t.Helper()
	cfg := Config{Store: st, Box: testBox(t, "master"), PublicURL: "https://backup.corp", ExtraHosts: []string{"127.0.0.1"}}
	if ps != nil {
		cfg.Client = ps.Client()
	}
	svc, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, reason := svc.Available(); reason != "" {
		t.Fatalf("service unavailable: %s", reason)
	}
	return svc
}

var failureEvent = plugin.Event{Kind: plugin.EventFailure, Task: "pg-main", RunID: 42, Message: "pg_dump: connection refused", At: time.Unix(1_800_000_000, 0).UTC()}

func TestAvailability(t *testing.T) {
	for _, tc := range []struct {
		url, reason string
	}{
		{"", ReasonNoPublicURL},
		{"backup.corp", ReasonNoPublicURL},
		{"http://backup.corp", ReasonInsecureOrigin},
		{"http://10.0.0.5:8080", ReasonInsecureOrigin},
		{"https://backup.corp", ""},
		{"http://localhost:8080", ""},
		{"http://127.0.0.1:8080", ""},
	} {
		svc, err := New(context.Background(), Config{Store: newMemStore(), Box: testBox(t, "k"), PublicURL: tc.url})
		if err != nil {
			t.Fatalf("%q: %v", tc.url, err)
		}
		pub, reason := svc.Available()
		if reason != tc.reason || (reason == "") != (pub != "") {
			t.Errorf("%q: key %q, reason %q; want reason %q", tc.url, pub, reason, tc.reason)
		}
	}
	svc, _ := New(context.Background(), Config{Store: newMemStore(), PublicURL: "https://backup.corp"})
	if _, reason := svc.Available(); reason != ReasonNoMasterKey {
		t.Fatalf("no box: reason %q, want %q", reason, ReasonNoMasterKey)
	}
}

// TestKeysSurviveRestart: every browser subscribed against the stored public
// key, so a restart must reuse it, the private half must be sealed, and a
// changed master key must switch push off rather than mint a new pair.
func TestKeysSurviveRestart(t *testing.T) {
	st := newMemStore()
	first := newService(t, st, nil)
	second := newService(t, st, nil)
	if first.publicKey != second.publicKey || first.privateKey != second.privateKey {
		t.Fatal("restart produced a different key pair")
	}
	if bytes.Contains(st.keys.PrivateKey, []byte(first.privateKey)) {
		t.Fatal("private key stored in the clear")
	}

	other, err := New(context.Background(), Config{Store: st, Box: testBox(t, "another master"), PublicURL: "https://backup.corp"})
	if err != nil {
		t.Fatal(err)
	}
	if _, reason := other.Available(); reason != ReasonKeyUnreadable {
		t.Fatalf("foreign master key: reason %q, want %q", reason, ReasonKeyUnreadable)
	}
	if st.keys.PublicKey != first.publicKey {
		t.Fatal("an unreadable key was replaced")
	}
}

func TestSendEncryptsAndSigns(t *testing.T) {
	ps := newPushServer(t)
	st := newMemStore()
	svc := newService(t, st, ps)
	b := newBrowser(t)

	if err := svc.SendTo(context.Background(), b.sub(1, ps.URL+"/push/1", core.RoleViewer), failureEvent); err != nil {
		t.Fatal(err)
	}
	reqs := ps.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	r := reqs[0]

	var got Payload
	if err := json.Unmarshal(b.decrypt(t, r.body), &got); err != nil {
		t.Fatal(err)
	}
	want := Payload{
		Kind: "failure", Task: "pg-main", Title: "✗ pg-main: бэкап не выполнен", RunID: 42,
		Message: failureEvent.Message, At: failureEvent.At, URL: "/history?at=42", Urgent: true,
	}
	if got != want {
		t.Fatalf("payload = %+v\nwant      %+v", got, want)
	}

	for k, v := range map[string]string{
		"Content-Encoding": "aes128gcm", "TTL": "86400", "Urgency": "high", "Topic": topic("failure", "pg-main\x00"),
	} {
		if r.header.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, r.header.Get(k), v)
		}
	}
	claims := verifyVAPID(t, r.header.Get("Authorization"), svc.publicKey)
	if claims["aud"] != ps.URL || claims["sub"] != "https://backup.corp" {
		t.Errorf("claims = %v; want aud %s and sub https://backup.corp", claims, ps.URL)
	}
	if cause, ok := st.result(1); !ok || cause != "" {
		t.Errorf("result = %q, %v; want a recorded success", cause, ok)
	}
}

// verifyVAPID checks an RFC 8292 Authorization header against the server's
// public key and returns the JWT claims.
func verifyVAPID(t *testing.T, header, publicKey string) map[string]any {
	t.Helper()
	tok, k, ok := strings.Cut(strings.TrimPrefix(header, "vapid t="), ", k=")
	if !ok || k != publicKey {
		t.Fatalf("Authorization = %q; want vapid t=…, k=<server key>", header)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	raw, _ := base64.RawURLEncoding.DecodeString(publicKey)
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), raw)
	if err != nil {
		t.Fatalf("server key: %v", err)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if len(sig) != 64 || !ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("VAPID signature does not verify against the server key")
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

func TestSuccessIsLowPriority(t *testing.T) {
	ps := newPushServer(t)
	svc := newService(t, newMemStore(), ps)
	ev := failureEvent
	ev.Kind = plugin.EventSuccess
	if err := svc.SendTo(context.Background(), newBrowser(t).sub(1, ps.URL+"/p", core.RoleViewer), ev); err != nil {
		t.Fatal(err)
	}
	h := ps.requests()[0].header
	if h.Get("TTL") != "3600" || h.Get("Urgency") != "normal" {
		t.Fatalf("TTL %s, Urgency %s; want 3600, normal", h.Get("TTL"), h.Get("Urgency"))
	}
	if h.Get("Topic") == topic("failure", "pg-main") {
		t.Fatal("a success shares its topic with a failure and could replace it")
	}
}

func TestGoneSubscriptionIsDropped(t *testing.T) {
	ps := newPushServer(t)
	ps.status["/gone"] = http.StatusGone
	st := newMemStore()
	svc := newService(t, st, ps)
	st.subs = []core.PushSubscription{newBrowser(t).sub(1, ps.URL+"/gone", core.RoleViewer)}

	if err := svc.Broadcast(context.Background(), core.RoleViewer, failureEvent); !errors.Is(err, ErrNoSubscribers) {
		t.Fatalf("err = %v, want ErrNoSubscribers once the only browser is gone", err)
	}
	if len(st.subs) != 0 {
		t.Fatal("expired subscription kept")
	}
}

func TestBroadcastPartialFailure(t *testing.T) {
	ps := newPushServer(t)
	ps.status["/bad"] = http.StatusForbidden
	st := newMemStore()
	svc := newService(t, st, ps)
	st.subs = []core.PushSubscription{
		newBrowser(t).sub(1, ps.URL+"/ok", core.RoleViewer),
		newBrowser(t).sub(2, ps.URL+"/bad", core.RoleViewer),
	}

	if err := svc.Broadcast(context.Background(), core.RoleViewer, failureEvent); err != nil {
		t.Fatalf("one browser delivered, err = %v; want nil", err)
	}
	if cause, _ := st.result(2); !strings.Contains(cause, "403") || !strings.Contains(cause, "BadJwtToken") {
		t.Fatalf("failed browser's cause = %q; want the status and the service's reason", cause)
	}
	if cause, ok := st.result(1); !ok || cause != "" {
		t.Fatalf("delivered browser's result = %q, %v", cause, ok)
	}
}

func TestBroadcastAllFail(t *testing.T) {
	ps := newPushServer(t)
	ps.status["/a"], ps.status["/b"] = http.StatusInternalServerError, http.StatusInternalServerError
	st := newMemStore()
	svc := newService(t, st, ps)
	st.subs = []core.PushSubscription{
		newBrowser(t).sub(1, ps.URL+"/a", core.RoleViewer),
		newBrowser(t).sub(2, ps.URL+"/b", core.RoleViewer),
	}
	err := svc.Broadcast(context.Background(), core.RoleViewer, failureEvent)
	if err == nil || !strings.Contains(err.Error(), "0/2 delivered") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v; want 0/2 delivered with the status", err)
	}
}

func TestBroadcastHonoursMinRole(t *testing.T) {
	ps := newPushServer(t)
	st := newMemStore()
	svc := newService(t, st, ps)
	st.subs = []core.PushSubscription{
		newBrowser(t).sub(1, ps.URL+"/viewer", core.RoleViewer),
		newBrowser(t).sub(2, ps.URL+"/operator", core.RoleOperator),
		newBrowser(t).sub(3, ps.URL+"/admin", core.RoleAdmin),
	}
	if err := svc.Broadcast(context.Background(), core.RoleOperator, failureEvent); err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, r := range ps.requests() {
		paths[r.path] = true
	}
	if len(paths) != 2 || !paths["/operator"] || !paths["/admin"] {
		t.Fatalf("delivered to %v; want operator and admin only", paths)
	}

	st.subs = st.subs[:1]
	if err := svc.Broadcast(context.Background(), core.RoleAdmin, failureEvent); !errors.Is(err, ErrNoSubscribers) {
		t.Fatalf("no admin browsers: err = %v, want ErrNoSubscribers", err)
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	ps := newPushServer(t)
	ps.status["/moved"] = http.StatusFound
	svc := newService(t, newMemStore(), ps)
	err := svc.SendTo(context.Background(), newBrowser(t).sub(1, ps.URL+"/moved", core.RoleViewer), failureEvent)
	if err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("err = %v; want the redirect reported as a failure", err)
	}
	for _, r := range ps.requests() {
		if r.path == "/elsewhere" {
			t.Fatal("redirect followed")
		}
	}
}

// TestEndpointNotInError: an endpoint is a capability to push to that browser,
// and send errors end up in the delivery log.
func TestEndpointNotInError(t *testing.T) {
	ps := newPushServer(t)
	svc := newService(t, newMemStore(), ps)
	endpoint := ps.URL + "/secret-token-abc"
	ps.Close()
	err := svc.SendTo(context.Background(), newBrowser(t).sub(1, endpoint, core.RoleViewer), failureEvent)
	if err == nil || strings.Contains(err.Error(), "secret-token-abc") {
		t.Fatalf("err = %v; want a failure that does not quote the endpoint", err)
	}
}

func TestCheckEndpoint(t *testing.T) {
	svc := &Service{hosts: append(append([]string(nil), DefaultHosts...), "push.corp.local", ".autopush.corp")}
	for _, tc := range []struct {
		endpoint string
		ok       bool
	}{
		{"https://fcm.googleapis.com/fcm/send/abc", true},
		{"https://updates.push.services.mozilla.com/wpush/v2/abc", true},
		{"https://web.push.apple.com/QAbc", true},
		{"https://wns2-par02p.notify.windows.com/w/?token=abc", true},
		{"https://push.corp.local/x", true},
		{"https://eu.autopush.corp/x", true},
		{"http://fcm.googleapis.com/fcm/send/abc", false},
		{"https://fcm.googleapis.com.evil.io/x", false},
		{"https://evilpush.apple.com/x", false},
		{"https://user:pw@fcm.googleapis.com/x", false},
		{"https://169.254.169.254/latest/meta-data", false},
		{"https://localhost/x", false},
		{"not a url", false},
	} {
		if err := svc.CheckEndpoint(tc.endpoint); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.endpoint, err, tc.ok)
		}
	}
}

func TestPayloadStaysUnderLimit(t *testing.T) {
	ev := failureEvent
	ev.Task = strings.Repeat("т", 500)
	ev.Message = strings.Repeat("<ошибка>&", 1000)
	body, err := json.Marshal(payloadFor(ev))
	if err != nil {
		t.Fatal(err)
	}
	// 4096-byte record minus the 16-byte tag, the 86-byte header and the
	// delimiter.
	if len(body) > 4096-16-86-1 {
		t.Fatalf("payload is %d bytes, over the single-record limit", len(body))
	}
	p := payloadFor(ev)
	if !strings.HasSuffix(p.Message, "…") || !strings.HasSuffix(p.Task, "…") {
		t.Fatal("long fields not marked as cut")
	}
	if got := truncate("абв", 4); got != "…" && got != "а…" {
		t.Fatalf("truncate split a rune: %q", got)
	}
}

func TestPayloadLinks(t *testing.T) {
	for _, tc := range []struct {
		ev   plugin.Event
		want string
	}{
		{plugin.Event{Kind: plugin.EventFailure, RunID: 7}, "/history?at=7"},
		{plugin.Event{Kind: plugin.EventRetentionError}, "/retention"},
		{plugin.Event{Kind: plugin.EventWatchdog}, "/"},
		{plugin.Event{Kind: plugin.EventSuccess, Task: "(проверка канала)"}, "/notifications"},
	} {
		if got := payloadFor(tc.ev).URL; got != tc.want {
			t.Errorf("%s: url %q, want %q", tc.ev.Kind, got, tc.want)
		}
	}
}

// TestChannelTestIsNotASuccess: the channel test is a success event with no
// run, and a phone must not announce it as a completed backup.
func TestChannelTestIsNotASuccess(t *testing.T) {
	p := payloadFor(plugin.Event{Kind: plugin.EventSuccess, Task: "(проверка канала)", Message: "test"})
	if p.Kind != KindTest || p.Task != "" {
		t.Fatalf("payload = %+v; want kind %q and no task", p, KindTest)
	}
	if p := payloadFor(plugin.Event{Kind: plugin.EventSuccess, Task: "pg", RunID: 3}); p.Kind != "success" {
		t.Fatalf("a run's success became %q", p.Kind)
	}
}

func TestRegister(t *testing.T) {
	reg := plugin.NewRegistry[plugin.Notifier]("notifier")
	Register(reg, newService(t, newMemStore(), nil))
	if _, err := reg.Create(Type, []byte(`{"min_role":"root"}`)); err == nil {
		t.Fatal("unknown role accepted")
	}
	n, err := reg.Create(Type, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n.(*Notifier).minRole != core.RoleViewer {
		t.Fatal("omitted min_role is not viewer")
	}

	off := plugin.NewRegistry[plugin.Notifier]("notifier")
	svc, _ := New(context.Background(), Config{Store: newMemStore(), Box: testBox(t, "k")})
	Register(off, svc)
	if _, err := off.Create(Type, nil); err == nil || !strings.Contains(err.Error(), "DUSKRUN_PUBLIC_URL") {
		t.Fatalf("unavailable push: err = %v; want the reason", err)
	}
}

// TestSecurityGoesToAdminsOnly: a channel open to every viewer still keeps who
// signed in from where to administrators.
func TestSecurityGoesToAdminsOnly(t *testing.T) {
	ps := newPushServer(t)
	st := newMemStore()
	svc := newService(t, st, ps)
	st.subs = []core.PushSubscription{
		newBrowser(t).sub(1, ps.URL+"/viewer", core.RoleViewer),
		newBrowser(t).sub(3, ps.URL+"/admin", core.RoleAdmin),
	}
	n := &Notifier{svc: svc, minRole: core.RoleViewer}
	ev := plugin.Event{Kind: plugin.EventSecurity, Subject: "a@x.io", Message: "вход с нового устройства", At: failureEvent.At}
	if err := n.Notify(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	reqs := ps.requests()
	if len(reqs) != 1 || reqs[0].path != "/admin" {
		t.Fatalf("delivered to %d browsers; want the admin's only", len(reqs))
	}
}

func TestPayloadForSystemEvents(t *testing.T) {
	for _, tc := range []struct {
		ev            plugin.Event
		url           string
		urgent        bool
		titleContains string
	}{
		{plugin.Event{Kind: plugin.EventStorageLow, Subject: "nas"}, "/storages", true, "nas"},
		{plugin.Event{Kind: plugin.EventSecurity, Subject: "a@x.io"}, "/settings", true, "a@x.io"},
		{plugin.Event{Kind: plugin.EventRetentionDone, Task: "pg"}, "/retention", false, "pg"},
		{plugin.Event{Kind: plugin.EventRecovered, Task: "pg", RunID: 9}, "/history?at=9", false, "снова в порядке"},
	} {
		p := payloadFor(tc.ev)
		if p.URL != tc.url || p.Urgent != tc.urgent || !strings.Contains(p.Title, tc.titleContains) {
			t.Errorf("%s: payload = %+v", tc.ev.Kind, p)
		}
	}
}
