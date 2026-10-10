// Package obs wires structured logging (slog) with secret masking in the
// ReplaceAttr slot. Masking is layered, because no single rule catches
// everything:
//
//   - attribute keys that name a credential (password, token, …) are masked
//     whole, whatever the value;
//   - strings and errors are scrubbed in place: credential-shaped fragments
//     (a Telegram bot token, the password in a URL's userinfo, ?token=…) are
//     replaced and the rest of the message survives, so an error stays useful;
//   - anything else (structs, maps, slices) goes through m-mizutani/masq, which
//     redacts fields tagged `masq:"secret"` or named like a credential.
package obs

import (
	"log/slog"
	"os"
	"regexp"
	"strings"

	"github.com/m-mizutani/masq"
)

// redactKeys are attribute keys whose values are always masked in logs.
var redactKeys = map[string]bool{
	"password": true, "secret": true, "token": true,
	"private_key": true, "passphrase": true, "master_key": true,
	"api_key": true, "access_key": true, "secret_key": true, "dsn": true,
}

const redacted = "«REDACTED»"

// scrubRules rewrite credential-shaped fragments inside free text. Each keeps
// what identifies the place (scheme, user, parameter name) and drops the value.
var scrubRules = []struct {
	re   *regexp.Regexp
	repl string
}{
	// Telegram bot token, e.g. in https://api.telegram.org/bot<id>:<secret>/…
	{regexp.MustCompile(`\d{6,}:[A-Za-z0-9_-]{30,}`), redacted},
	// Password in URL userinfo: postgres://user:pass@host → user:«REDACTED»@host.
	{regexp.MustCompile(`(://[^/@\s:]*:)[^/@\s]+@`), "${1}" + redacted + "@"},
	// Credential-named query parameters.
	{regexp.MustCompile(`(?i)([?&](?:token|access_token|api_key|apikey|key|secret|password|sig|signature)=)[^&\s"']+`), "${1}" + redacted},
}

// Scrub masks credential-shaped fragments in s and leaves the rest intact. Use
// it for any error text that is persisted or shown, not just logged.
func Scrub(s string) string {
	for _, r := range scrubRules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}

// structMask redacts secrets nested inside composite values.
var structMask = masq.New(
	masq.WithRedactMessage(redacted),
	masq.WithTag("secret"),
	masq.WithFieldName("Password"),
	masq.WithFieldName("Secret"),
	masq.WithFieldName("Token"),
	masq.WithFieldName("PrivateKey"),
	masq.WithFieldName("Passphrase"),
	masq.WithFieldName("MasterKey"),
	masq.WithFieldName("APIKey"),
	masq.WithFieldName("AccessKey"),
	masq.WithFieldName("SecretKey"),
	masq.WithFieldName("DSN"),
)

// NewLogger builds a slog.Logger. format is "json" or "text".
func NewLogger(format string) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level:       slog.LevelInfo,
		ReplaceAttr: redact,
	}
	var h slog.Handler
	if strings.EqualFold(format, "text") {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

func redact(groups []string, a slog.Attr) slog.Attr {
	if redactKeys[strings.ToLower(a.Key)] {
		return slog.String(a.Key, redacted)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, Scrub(a.Value.String()))
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			// Handlers print an error as its text anyway; scrubbing the text
			// is reliable where cloning the error's internals is not.
			return slog.String(a.Key, Scrub(err.Error()))
		}
		return structMask(groups, a)
	default:
		return a
	}
}
