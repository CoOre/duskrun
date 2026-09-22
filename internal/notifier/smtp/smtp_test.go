// Tests run against a real SMTP conversation on a local listener, the way the
// webhook tests run against a real HTTP server: what matters is what the server
// received, not what the client believes it sent.
//
// They live in package smtp (not smtp_test) because the STARTTLS and implicit
// paths need the unexported tlsConfig hook — a fake server can only present a
// self-signed certificate.
package smtp

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duskrun/duskrun/internal/plugin"
)

// --- fake SMTP server -------------------------------------------------------

type fakeServer struct {
	ln       net.Listener
	tlsCfg   *tls.Config
	startTLS bool   // advertise STARTTLS
	authMech string // advertised AUTH mechanisms; empty means no AUTH at all
	authOK   bool
	dropQuit bool // hang up after accepting the message, never answering QUIT

	mu      sync.Mutex
	from    string
	rcpts   []string
	data    string
	authRaw string
	secure  bool // the message arrived after a TLS upgrade
}

func startFake(t *testing.T, f *fakeServer) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f.ln = ln
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.handle(conn)
		}
	}()
	return f
}

func (f *fakeServer) addr() string { return f.ln.Addr().String() }

func (f *fakeServer) port() string {
	_, p, _ := net.SplitHostPort(f.addr())
	return p
}

func (f *fakeServer) snapshot() fakeServer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fakeServer{from: f.from, rcpts: append([]string(nil), f.rcpts...), data: f.data, authRaw: f.authRaw, secure: f.secure}
}

func (f *fakeServer) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	secure := false
	send := func(format string, a ...any) {
		fmt.Fprintf(w, format+"\r\n", a...)
		w.Flush()
	}

	send("220 fake ESMTP")
	for {
		raw, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(raw, "\r\n")
		up := strings.ToUpper(cmd)

		switch {
		case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
			send("250-fake")
			if f.startTLS && !secure {
				send("250-STARTTLS")
			}
			if f.authMech != "" {
				send("250-AUTH %s", f.authMech)
			}
			send("250 HELP")

		case up == "STARTTLS":
			send("220 ready")
			tconn := tls.Server(conn, f.tlsCfg)
			if err := tconn.Handshake(); err != nil {
				return
			}
			conn, r, w, secure = tconn, bufio.NewReader(tconn), bufio.NewWriter(tconn), true

		case strings.HasPrefix(up, "AUTH"):
			fields := strings.Fields(cmd)
			if len(fields) >= 3 {
				f.set(func() { f.authRaw = fields[2] })
			}
			if f.authOK {
				send("235 2.7.0 accepted")
			} else {
				send("535 5.7.8 bad credentials")
			}

		case strings.HasPrefix(up, "MAIL FROM"):
			f.set(func() { f.from = angled(cmd) })
			send("250 ok")

		case strings.HasPrefix(up, "RCPT TO"):
			f.set(func() { f.rcpts = append(f.rcpts, angled(cmd)) })
			send("250 ok")

		case up == "DATA":
			send("354 go ahead")
			var body strings.Builder
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if line == ".\r\n" || line == ".\n" {
					break
				}
				body.WriteString(line)
			}
			f.set(func() { f.data = body.String(); f.secure = secure })
			send("250 ok")
			if f.dropQuit {
				return
			}

		case up == "QUIT":
			send("221 bye")
			return

		case up == "RSET", up == "NOOP":
			send("250 ok")

		default:
			send("500 unrecognized")
		}
	}
}

func (f *fakeServer) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func angled(cmd string) string {
	_, rest, ok := strings.Cut(cmd, "<")
	if !ok {
		return ""
	}
	addr, _, _ := strings.Cut(rest, ">")
	return addr
}

// selfSigned returns a server config and the matching client trust store, so
// the TLS paths are exercised with real verification rather than with
// InsecureSkipVerify.
func selfSigned(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fake-smtp"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}},
		&tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}
}

// build constructs the notifier through New (so config validation is on the
// path under test) and points its TLS at the fake server's certificate.
func build(t *testing.T, cfg map[string]any, clientTLS *tls.Config) *Notifier {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	n, err := New(raw)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sn := n.(*Notifier)
	sn.tlsConfig = clientTLS
	return sn
}

func sampleEvent() plugin.Event {
	return plugin.Event{
		Kind: plugin.EventFailure, Task: "orders-db nightly", RunID: 5230,
		Message: "mysqldump: ERROR 1045 (28000): Access denied",
		At:      time.Date(2026, 8, 12, 1, 30, 41, 0, time.UTC),
	}
}

func headersOf(msg string) string {
	head, _, _ := strings.Cut(msg, "\r\n\r\n")
	return head
}

// --- tests ------------------------------------------------------------------

// TestSMTPNotifyDelivers checks the envelope and the message the server ends up
// holding: sender, recipient, subject and the facts of the event.
func TestSMTPNotifyDelivers(t *testing.T) {
	srv := startFake(t, &fakeServer{})
	n := build(t, map[string]any{
		"host": "127.0.0.1", "port": srv.port(), "tls": "none",
		"from": "duskrun@corp.io", "to": []string{"ops@corp.io"},
	}, nil)

	if err := n.Notify(context.Background(), sampleEvent()); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	got := srv.snapshot()
	if got.from != "duskrun@corp.io" {
		t.Errorf("MAIL FROM = %q, want duskrun@corp.io", got.from)
	}
	if len(got.rcpts) != 1 || got.rcpts[0] != "ops@corp.io" {
		t.Errorf("RCPT TO = %v, want [ops@corp.io]", got.rcpts)
	}
	for _, want := range []string{
		"Subject: [duskrun] failure: orders-db nightly",
		"Content-Type: text/plain; charset=UTF-8",
		"Message-ID: <duskrun.",
		"Событие:   failure",
		"Запуск:    #5230",
		"Время:     2026-08-12 01:30:41 UTC",
		"Access denied",
	} {
		if !strings.Contains(got.data, want) {
			t.Errorf("message is missing %q; got:\n%s", want, got.data)
		}
	}
}

// TestSMTPRejectsIncompleteConfig keeps every check that needs no network in
// New, so the "Test" button explains the problem instead of timing out.
func TestSMTPRejectsIncompleteConfig(t *testing.T) {
	cases := map[string]string{
		"no host":            `{"from":"a@b.io","to":["c@d.io"]}`,
		"no from":            `{"host":"mx.io","to":["c@d.io"]}`,
		"no to":              `{"host":"mx.io","from":"a@b.io"}`,
		"empty to list":      `{"host":"mx.io","from":"a@b.io","to":[]}`,
		"bad from address":   `{"host":"mx.io","from":"not an address","to":["c@d.io"]}`,
		"bad to address":     `{"host":"mx.io","from":"a@b.io","to":["c@d.io","oops"]}`,
		"unknown tls mode":   `{"host":"mx.io","from":"a@b.io","to":["c@d.io"],"tls":"ssl"}`,
		"user without pass":  `{"host":"mx.io","from":"a@b.io","to":["c@d.io"],"username":"u"}`,
		"auth in the clear":  `{"host":"mx.io","from":"a@b.io","to":["c@d.io"],"tls":"none","username":"u","password":"p"}`,
		"port not a number":  `{"host":"mx.io","from":"a@b.io","to":["c@d.io"],"port":"smtp"}`,
		"port out of range":  `{"host":"mx.io","from":"a@b.io","to":["c@d.io"],"port":70000}`,
		"config is not json": `{`,
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New([]byte(cfg)); err == nil {
				t.Fatalf("New(%s) returned nil error, want a rejection", cfg)
			}
		})
	}

	// The same credentials against a local relay are fine: net/smtp releases a
	// password over plaintext only to localhost, and so do we.
	if _, err := New([]byte(`{"host":"127.0.0.1","from":"a@b.io","to":["c@d.io"],"tls":"none","username":"u","password":"p"}`)); err != nil {
		t.Fatalf("New with localhost auth: %v", err)
	}
}

// TestSMTPHeaderInjection: a task name carrying CRLF must not become extra
// headers — otherwise naming a task "x\r\nBcc: …" turns notifications into a
// mailing list controlled by whoever can create tasks.
func TestSMTPHeaderInjection(t *testing.T) {
	srv := startFake(t, &fakeServer{})
	n := build(t, map[string]any{
		"host": "127.0.0.1", "port": srv.port(), "tls": "none",
		"from": "duskrun@corp.io", "to": []string{"ops@corp.io"},
	}, nil)

	ev := sampleEvent()
	ev.Task = "orders\r\nBcc: attacker@evil.example"
	if err := n.Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	head := headersOf(srv.snapshot().data)
	lines := strings.Split(head, "\r\n")
	if len(lines) != 8 {
		t.Fatalf("header block has %d lines, want the 8 headers we write:\n%s", len(lines), head)
	}
	for _, l := range lines {
		if strings.HasPrefix(strings.ToLower(l), "bcc:") {
			t.Fatalf("injected header survived:\n%s", head)
		}
	}
	// The CRLF ends up quoted inside the RFC 2047 encoded-word rather than
	// breaking the header — that is the outcome we want, so assert it rather
	// than merely counting lines.
	if !strings.Contains(head, "=0D=0A") {
		t.Errorf("expected the CRLF to be quoted in the subject, got:\n%s", head)
	}
}

// TestSMTPEncodesNonASCIISubject: Cyrillic task names are normal here, and a
// bare UTF-8 subject renders as mojibake in a fair number of clients.
func TestSMTPEncodesNonASCIISubject(t *testing.T) {
	srv := startFake(t, &fakeServer{})
	n := build(t, map[string]any{
		"host": "127.0.0.1", "port": srv.port(), "tls": "none",
		"from": "duskrun@corp.io", "to": []string{"ops@corp.io"},
	}, nil)

	ev := sampleEvent()
	ev.Task = "заказы-ночной"
	if err := n.Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	head := headersOf(srv.snapshot().data)
	if !strings.Contains(head, "Subject: =?UTF-8?q?") {
		t.Fatalf("subject is not RFC 2047 encoded:\n%s", head)
	}
	if strings.Contains(head, "заказы") {
		t.Errorf("raw UTF-8 leaked into the subject header:\n%s", head)
	}
}

// TestSMTPRequiresSTARTTLSWhenConfigured: a server that does not offer the
// upgrade must abort the send. Falling back would deliver the mail while
// silently dropping the guarantee the operator asked for.
func TestSMTPRequiresSTARTTLSWhenConfigured(t *testing.T) {
	srv := startFake(t, &fakeServer{startTLS: false})
	n := build(t, map[string]any{
		"host": "127.0.0.1", "port": srv.port(), "tls": "starttls",
		"from": "duskrun@corp.io", "to": []string{"ops@corp.io"},
	}, nil)

	err := n.Notify(context.Background(), sampleEvent())
	if err == nil {
		t.Fatal("Notify succeeded against a server without STARTTLS, want an error")
	}
	if !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("error = %v, want it to name STARTTLS", err)
	}
	if got := srv.snapshot(); got.data != "" {
		t.Fatalf("message was sent in the clear:\n%s", got.data)
	}
}

// TestSMTPStartTLSDelivers is the positive half of the pair above: the message
// arrives, and it arrives after the upgrade.
func TestSMTPStartTLSDelivers(t *testing.T) {
	serverTLS, clientTLS := selfSigned(t)
	srv := startFake(t, &fakeServer{startTLS: true, tlsCfg: serverTLS})
	n := build(t, map[string]any{
		"host": "127.0.0.1", "port": srv.port(), "tls": "starttls",
		"from": "duskrun@corp.io", "to": []string{"ops@corp.io"},
	}, clientTLS)

	if err := n.Notify(context.Background(), sampleEvent()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	got := srv.snapshot()
	if !got.secure {
		t.Error("message arrived before the TLS upgrade")
	}
	if !strings.Contains(got.data, "orders-db nightly") {
		t.Errorf("message did not arrive:\n%s", got.data)
	}
}

// TestSMTPAuthPlain checks the server receives exactly the configured
// credentials — the resolved password_ref, in practice.
func TestSMTPAuthPlain(t *testing.T) {
	serverTLS, clientTLS := selfSigned(t)
	srv := startFake(t, &fakeServer{startTLS: true, tlsCfg: serverTLS, authMech: "PLAIN", authOK: true})
	n := build(t, map[string]any{
		"host": "127.0.0.1", "port": srv.port(), "tls": "starttls",
		"from": "duskrun@corp.io", "to": []string{"ops@corp.io"},
		"username": "duskrun", "password": "s3cret",
	}, clientTLS)

	if err := n.Notify(context.Background(), sampleEvent()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(srv.snapshot().authRaw)
	if err != nil {
		t.Fatalf("decode AUTH payload: %v", err)
	}
	if want := "\x00duskrun\x00s3cret"; string(decoded) != want {
		t.Errorf("AUTH payload = %q, want %q", decoded, want)
	}
}

// TestSMTPRejectsBadCredentials: a refused login has to surface as a readable
// delivery-log entry, not as a generic failure.
func TestSMTPRejectsBadCredentials(t *testing.T) {
	serverTLS, clientTLS := selfSigned(t)
	srv := startFake(t, &fakeServer{startTLS: true, tlsCfg: serverTLS, authMech: "PLAIN", authOK: false})
	n := build(t, map[string]any{
		"host": "127.0.0.1", "port": srv.port(), "tls": "starttls",
		"from": "duskrun@corp.io", "to": []string{"ops@corp.io"},
		"username": "duskrun", "password": "wrong",
	}, clientTLS)

	err := n.Notify(context.Background(), sampleEvent())
	if err == nil {
		t.Fatal("Notify succeeded with rejected credentials")
	}
	if !strings.Contains(err.Error(), "duskrun") {
		t.Errorf("error = %v, want it to name the account", err)
	}
}

// TestSMTPRejectsServerWithoutPlainAuth documents the known net/smtp boundary:
// AUTH LOGIN is not implemented, so say what the server offered instead of
// failing opaquely.
func TestSMTPRejectsServerWithoutPlainAuth(t *testing.T) {
	serverTLS, clientTLS := selfSigned(t)
	srv := startFake(t, &fakeServer{startTLS: true, tlsCfg: serverTLS, authMech: "LOGIN NTLM"})
	n := build(t, map[string]any{
		"host": "127.0.0.1", "port": srv.port(), "tls": "starttls",
		"from": "duskrun@corp.io", "to": []string{"ops@corp.io"},
		"username": "duskrun", "password": "s3cret",
	}, clientTLS)

	err := n.Notify(context.Background(), sampleEvent())
	if err == nil {
		t.Fatal("Notify succeeded against a server without AUTH PLAIN")
	}
	if !strings.Contains(err.Error(), "LOGIN") {
		t.Errorf("error = %v, want it to list the advertised mechanisms", err)
	}
}

// TestSMTPMultipleRecipients: every configured address gets its own RCPT TO,
// and all of them appear in the To header.
func TestSMTPMultipleRecipients(t *testing.T) {
	srv := startFake(t, &fakeServer{})
	n := build(t, map[string]any{
		"host": "127.0.0.1", "port": srv.port(), "tls": "none",
		"from": "duskrun@corp.io", "to": []string{"ops@corp.io", "oncall@corp.io"},
	}, nil)

	if err := n.Notify(context.Background(), sampleEvent()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	got := srv.snapshot()
	if len(got.rcpts) != 2 || got.rcpts[0] != "ops@corp.io" || got.rcpts[1] != "oncall@corp.io" {
		t.Fatalf("RCPT TO = %v, want both recipients", got.rcpts)
	}
	if !strings.Contains(got.data, "To: ops@corp.io, oncall@corp.io") {
		t.Errorf("To header does not list both recipients:\n%s", headersOf(got.data))
	}
}

// TestSMTPDefaultPortPerTLSMode: the port follows the mode, and an explicit
// value still wins.
func TestSMTPDefaultPortPerTLSMode(t *testing.T) {
	cases := []struct{ cfg, want string }{
		{`{"host":"mx.io","from":"a@b.io","to":["c@d.io"]}`, "mx.io:587"},
		{`{"host":"mx.io","from":"a@b.io","to":["c@d.io"],"tls":"starttls"}`, "mx.io:587"},
		{`{"host":"mx.io","from":"a@b.io","to":["c@d.io"],"tls":"implicit"}`, "mx.io:465"},
		{`{"host":"mx.io","from":"a@b.io","to":["c@d.io"],"tls":"none"}`, "mx.io:25"},
		{`{"host":"mx.io","from":"a@b.io","to":["c@d.io"],"port":2525}`, "mx.io:2525"},
		{`{"host":"mx.io","from":"a@b.io","to":["c@d.io"],"port":"2525"}`, "mx.io:2525"},
	}
	for _, c := range cases {
		n, err := New([]byte(c.cfg))
		if err != nil {
			t.Fatalf("New(%s): %v", c.cfg, err)
		}
		if got := n.(*Notifier).addr; got != c.want {
			t.Errorf("New(%s) addr = %q, want %q", c.cfg, got, c.want)
		}
	}
}

// TestSMTPMultilineMessageStaysOneField: dump stderr spans several lines, and
// an unindented continuation would read as another field of the report.
func TestSMTPMultilineMessageStaysOneField(t *testing.T) {
	srv := startFake(t, &fakeServer{})
	n := build(t, map[string]any{
		"host": "127.0.0.1", "port": srv.port(), "tls": "none",
		"from": "duskrun@corp.io", "to": []string{"ops@corp.io"},
	}, nil)

	ev := sampleEvent()
	ev.Message = "line one\nline two"
	if err := n.Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if !strings.Contains(srv.snapshot().data, "Сообщение: line one\r\n           line two") {
		t.Errorf("continuation line is not indented:\n%s", srv.snapshot().data)
	}
}

// TestSMTPTestEventOmitsRunID: a channel test and a watchdog finding have no
// run behind them, and "#0" in the report would be noise.
func TestSMTPTestEventOmitsRunID(t *testing.T) {
	srv := startFake(t, &fakeServer{})
	n := build(t, map[string]any{
		"host": "127.0.0.1", "port": srv.port(), "tls": "none",
		"from": "duskrun@corp.io", "to": []string{"ops@corp.io"},
	}, nil)

	ev := sampleEvent()
	ev.RunID = 0
	if err := n.Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if strings.Contains(srv.snapshot().data, "Запуск:") {
		t.Errorf("run line present without a run:\n%s", srv.snapshot().data)
	}
}

// TestSMTPQuitFailureIsNotADeliveryFailure: the server accepted the message at
// end-of-DATA, so a connection that dies before answering QUIT has still
// delivered it. Reporting that error would show a red row in the delivery log
// for a mail sitting in the operator's inbox.
func TestSMTPQuitFailureIsNotADeliveryFailure(t *testing.T) {
	srv := startFake(t, &fakeServer{dropQuit: true})
	n := build(t, map[string]any{
		"host": "127.0.0.1", "port": srv.port(), "tls": "none",
		"from": "duskrun@corp.io", "to": []string{"ops@corp.io"},
	}, nil)

	if err := n.Notify(context.Background(), sampleEvent()); err != nil {
		t.Fatalf("Notify = %v, want nil: the message was accepted before QUIT", err)
	}
	if got := srv.snapshot(); !strings.Contains(got.data, "Access denied") {
		t.Fatalf("the server never got the message; data = %q", got.data)
	}
}

// TestSMTPDialRespectsDeadline: delivery runs on a detached context, so the
// send timeout has to bind the connect too — otherwise a host that swallows the
// SYN pins the worker for the OS timeout, minutes, not seconds.
func TestSMTPDialRespectsDeadline(t *testing.T) {
	n := build(t, map[string]any{
		// 203.0.113.0/24 is TEST-NET-3: reserved for documentation, routed
		// nowhere, so the connect can only end in a timeout.
		"host": "203.0.113.1", "port": "587", "tls": "none",
		"from": "duskrun@corp.io", "to": []string{"ops@corp.io"},
	}, nil)
	// A deadline in the past: the dialer must refuse before the OS gets a say.
	n.now = func() time.Time { return time.Now().Add(-time.Minute) }

	start := time.Now()
	err := n.Notify(context.Background(), sampleEvent())
	if err == nil {
		t.Fatal("Notify = nil, want a dial failure")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("dial took %s, want the deadline to cut it short", elapsed)
	}
}

// TestSMTPRegisteredAsPlugin: the channel is only reachable if the registry
// knows it — this is what `duskrun plugins` lists and what the API validates
// a channel type against.
func TestSMTPRegisteredAsPlugin(t *testing.T) {
	_, err := plugin.Notifiers.Create("smtp", []byte(`{"host":"mx.io","from":"a@b.io","to":["c@d.io"]}`))
	if err != nil {
		t.Fatalf("Notifiers.Create(smtp): %v", err)
	}
}
