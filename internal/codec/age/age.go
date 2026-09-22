// Package age implements the age Codec: encryption that leaves an artifact
// restorable WITHOUT Duskrun using the stock `age` tool (age-encryption.org/v1).
//
// Two modes:
//   - x25519 (default): encrypt to one or more age public recipients; decrypt
//     with the matching identity (secret key).
//   - passphrase: scrypt-based symmetric encryption from a passphrase.
//
// Close is mandatory on the writer — age finalises its last STREAM chunk on
// Close, and skipping it corrupts the artifact (roadmap §1).
package age

import (
	"encoding/json"
	"fmt"
	"io"

	"filippo.io/age"

	"github.com/duskrun/duskrun/internal/plugin"
)

func init() {
	plugin.Codecs.Register("age", New)
}

// Config is the age codec config (a segment of the task's codec configuration).
// Credentials (identity/passphrase) are resolved upstream and passed inline; a
// future identity_ref would be dereferenced from the secret store before New.
type Config struct {
	Mode       string   `json:"mode"`       // "x25519" (default) | "passphrase"
	Recipients []string `json:"recipients"` // age1... public keys (x25519 encrypt side)
	Identity   string   `json:"identity"`   // AGE-SECRET-KEY-1... (x25519 decrypt side)
	Passphrase string   `json:"passphrase"` // passphrase mode (both sides)
}

const (
	modeX25519     = "x25519"
	modePassphrase = "passphrase"
)

// New builds an age Codec from its JSON config. A nil/empty config yields an
// x25519 codec with no recipients — usable only once recipients are supplied.
func New(raw []byte) (plugin.Codec, error) {
	var c Config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("age: bad config: %w", err)
		}
	}
	if c.Mode == "" {
		c.Mode = modeX25519
	}
	if c.Mode != modeX25519 && c.Mode != modePassphrase {
		return nil, fmt.Errorf("age: unknown mode %q", c.Mode)
	}
	return Codec{cfg: c}, nil
}

// Codec is the age stream transformer.
type Codec struct{ cfg Config }

func (Codec) Name() string { return "age" }
func (Codec) Ext() string  { return ".age" }

// NewWriter wraps dst so bytes written are age-encrypted into it. Close MUST be
// called to finalise the STREAM.
func (c Codec) NewWriter(dst io.Writer) (io.WriteCloser, error) {
	recips, err := c.recipients()
	if err != nil {
		return nil, err
	}
	w, err := age.Encrypt(dst, recips...)
	if err != nil {
		return nil, fmt.Errorf("age: encrypt: %w", err)
	}
	return w, nil
}

// NewReader decrypts src (restore/verify). age.Decrypt yields an io.Reader; we
// adapt it to io.ReadCloser.
func (c Codec) NewReader(src io.Reader) (io.ReadCloser, error) {
	ids, err := c.identities()
	if err != nil {
		return nil, err
	}
	r, err := age.Decrypt(src, ids...)
	if err != nil {
		return nil, fmt.Errorf("age: decrypt: %w", err)
	}
	return io.NopCloser(r), nil
}

// recipients builds the age recipients for the write side.
func (c Codec) recipients() ([]age.Recipient, error) {
	if c.cfg.Mode == modePassphrase {
		r, err := age.NewScryptRecipient(c.cfg.Passphrase)
		if err != nil {
			return nil, fmt.Errorf("age: passphrase recipient: %w", err)
		}
		return []age.Recipient{r}, nil
	}
	if len(c.cfg.Recipients) == 0 {
		return nil, fmt.Errorf("age: x25519 mode requires at least one recipient")
	}
	out := make([]age.Recipient, 0, len(c.cfg.Recipients))
	for _, s := range c.cfg.Recipients {
		r, err := age.ParseX25519Recipient(s)
		if err != nil {
			return nil, fmt.Errorf("age: recipient %q: %w", s, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// identities builds the age identities for the read side.
func (c Codec) identities() ([]age.Identity, error) {
	if c.cfg.Mode == modePassphrase {
		id, err := age.NewScryptIdentity(c.cfg.Passphrase)
		if err != nil {
			return nil, fmt.Errorf("age: passphrase identity: %w", err)
		}
		return []age.Identity{id}, nil
	}
	if c.cfg.Identity == "" {
		return nil, fmt.Errorf("age: x25519 mode requires an identity to decrypt")
	}
	id, err := age.ParseX25519Identity(c.cfg.Identity)
	if err != nil {
		return nil, fmt.Errorf("age: identity: %w", err)
	}
	return []age.Identity{id}, nil
}
