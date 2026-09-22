// Package obs wires structured logging (slog). Secret masking hooks in here:
// the ReplaceAttr slot is where m-mizutani/masq will redact secret-typed values
// once secret handling lands (M1). Until then we redact by well-known attr key
// so credentials never leak even before masq is added.
package obs

import (
	"log/slog"
	"os"
	"strings"
)

// redactKeys are attribute keys whose values are always masked in logs.
var redactKeys = map[string]bool{
	"password": true, "secret": true, "token": true,
	"private_key": true, "passphrase": true, "master_key": true,
}

const redacted = "«REDACTED»"

// NewLogger builds a slog.Logger. format is "json" or "text".
//
// TODO(M1): replace ReplaceAttr with masq.New(masq.WithTag("secure"), ...) so
// masking is type/tag-driven instead of key-name-driven.
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

func redact(_ []string, a slog.Attr) slog.Attr {
	if redactKeys[strings.ToLower(a.Key)] {
		return slog.String(a.Key, redacted)
	}
	return a
}
