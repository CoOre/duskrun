package obs

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const botToken = "123456789:AAH-abcdefghijklmnopqrstuvwxyz012345"

func capture(t *testing.T, format string, log func(*slog.Logger)) string {
	t.Helper()
	var buf bytes.Buffer
	opts := &slog.HandlerOptions{ReplaceAttr: redact}
	var h slog.Handler = slog.NewJSONHandler(&buf, opts)
	if format == "text" {
		h = slog.NewTextHandler(&buf, opts)
	}
	log(slog.New(h))
	return buf.String()
}

func TestRedactsCredentialKeysAnyCase(t *testing.T) {
	for _, key := range []string{"password", "Password", "TOKEN", "api_key", "dsn"} {
		out := capture(t, "json", func(l *slog.Logger) { l.Info("x", key, "hunter2") })
		if strings.Contains(out, "hunter2") {
			t.Errorf("key %q leaked: %s", key, out)
		}
	}
}

func TestScrubsErrorText(t *testing.T) {
	// The shape http.Client.Do returns when the Telegram API is unreachable.
	err := fmt.Errorf("telegram: post: %w",
		errors.New(`Post "https://api.telegram.org/bot`+botToken+`/sendMessage": dial tcp: connection refused`))
	for _, format := range []string{"json", "text"} {
		out := capture(t, format, func(l *slog.Logger) { l.Error("notify failed", "err", err) })
		if strings.Contains(out, botToken) || strings.Contains(out, "abcdefghijklmnop") {
			t.Errorf("%s: bot token leaked: %s", format, out)
		}
		if !strings.Contains(out, "connection refused") {
			t.Errorf("%s: scrubbing lost the useful part: %s", format, out)
		}
	}
}

func TestScrub(t *testing.T) {
	cases := []struct{ in, leak, keep string }{
		{"dial postgres://backup:s3cr3t@db:5432/app", "s3cr3t", "postgres://backup:"},
		{"GET https://hooks.example/x?token=abc123&x=1", "abc123", "x=1"},
		{"GET https://h/x?SIG=deadbeef", "deadbeef", "SIG="},
		{"bot" + botToken + "/sendMessage", botToken, "/sendMessage"},
	}
	for _, c := range cases {
		got := Scrub(c.in)
		if strings.Contains(got, c.leak) || !strings.Contains(got, c.keep) || !strings.Contains(got, redacted) {
			t.Errorf("Scrub(%q) = %q", c.in, got)
		}
	}
}

func TestMasksNestedStructSecrets(t *testing.T) {
	type creds struct {
		User     string
		Password string
		Key      string `masq:"secret"`
	}
	out := capture(t, "json", func(l *slog.Logger) {
		l.Info("x", "creds", creds{User: "backup", Password: "hunter2", Key: "k-123"})
	})
	if strings.Contains(out, "hunter2") || strings.Contains(out, "k-123") {
		t.Errorf("nested secret leaked: %s", out)
	}
	if !strings.Contains(out, "backup") {
		t.Errorf("non-secret field lost: %s", out)
	}
}

func TestLeavesOrdinaryAttrsAlone(t *testing.T) {
	out := capture(t, "json", func(l *slog.Logger) {
		l.Info("worker: run success", "task", "pg-main", "run", 42, "ok", true)
	})
	for _, want := range []string{`"msg":"worker: run success"`, `"task":"pg-main"`, `"run":42`, `"ok":true`, `"level":"INFO"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
}
