package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// SecretRefStore is the store surface secret resolution needs: turning a
// "secret://name" reference into the sealed row. *sqlite.Store satisfies it.
type SecretRefStore interface {
	GetSecretByRef(ctx context.Context, ref string) (*Secret, error)
}

// SecretResolver replaces every "<key>_ref" in a plugin config with the
// decrypted secret under "<key>". It is the one place that knows the convention,
// so a connector's private key, a storage's access key, a dumper's fetch key and
// a notifier's bot token all resolve the same way — and none of them has to be
// stored in the config in plaintext.
//
// A nil *SecretResolver is usable: configs without references pass through
// untouched, and a config that does carry one fails with "no secret opener
// configured" rather than panicking or silently substituting an empty value.
type SecretResolver struct {
	store SecretRefStore
	box   SecretOpener
}

// NewSecretResolver builds a resolver over the secret store and the opener that
// decrypts its rows. Either may be nil; see the nil-receiver note above — the
// failure then surfaces on the config that actually needs a secret.
func NewSecretResolver(store SecretRefStore, box SecretOpener) *SecretResolver {
	return &SecretResolver{store: store, box: box}
}

// refSuffix marks a config key as a reference to the secret store.
const refSuffix = "_ref"

// Resolve returns raw with every reference substituted. It walks nested objects
// and arrays, so a config that groups its credentials (mssql's
// `fetch: {private_key_ref}`) resolves as readily as a flat one. The original
// bytes are returned unchanged when there is nothing to substitute, which keeps
// key order stable for configs that carry no secrets at all.
//
// An inline value already present under "<key>" wins over "<key>_ref": that is
// what lets the API hand a plugin an already-resolved config without the
// resolver reaching for the store a second time.
func (r *SecretResolver) Resolve(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	changed, err := r.resolveMap(ctx, cfg, "")
	if err != nil {
		return nil, err
	}
	if !changed {
		return raw, nil
	}
	resolved, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return resolved, nil
}

// resolveMap substitutes references in one object in place and recurses into its
// children. path is the dotted prefix used in error messages ("fetch.").
func (r *SecretResolver) resolveMap(ctx context.Context, cfg map[string]any, path string) (bool, error) {
	changed := false
	for key, val := range cfg {
		switch child := val.(type) {
		case map[string]any:
			sub, err := r.resolveMap(ctx, child, path+key+".")
			if err != nil {
				return false, err
			}
			changed = changed || sub
			continue
		case []any:
			sub, err := r.resolveSlice(ctx, child, path+key+".")
			if err != nil {
				return false, err
			}
			changed = changed || sub
			continue
		}

		base, isRef := strings.CutSuffix(key, refSuffix)
		if !isRef || base == "" {
			continue
		}
		ref, _ := val.(string)
		if ref == "" {
			continue
		}
		if inline, _ := cfg[base].(string); inline != "" {
			continue // an explicit inline value wins over the reference
		}
		plain, err := r.Open(ctx, ref)
		if err != nil {
			return false, fmt.Errorf("%s%s: %w", path, key, err)
		}
		cfg[base] = string(plain)
		changed = true
	}
	return changed, nil
}

func (r *SecretResolver) resolveSlice(ctx context.Context, items []any, path string) (bool, error) {
	changed := false
	for i, item := range items {
		switch child := item.(type) {
		case map[string]any:
			sub, err := r.resolveMap(ctx, child, fmt.Sprintf("%s%d.", path, i))
			if err != nil {
				return false, err
			}
			changed = changed || sub
		case []any:
			sub, err := r.resolveSlice(ctx, child, fmt.Sprintf("%s%d.", path, i))
			if err != nil {
				return false, err
			}
			changed = changed || sub
		}
	}
	return changed, nil
}

// Open decrypts the secret behind a reference. It is the raw form used where the
// secret is not part of a config blob — a connection's password, which reaches
// the dumper as plugin.Credentials rather than as JSON.
func (r *SecretResolver) Open(ctx context.Context, ref string) ([]byte, error) {
	if r == nil || r.box == nil {
		return nil, fmt.Errorf("secret %q required but no secret opener configured", ref)
	}
	if r.store == nil {
		return nil, fmt.Errorf("secret %q required but no secret store configured", ref)
	}
	sec, err := r.store.GetSecretByRef(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("secret %q: %w", ref, err)
	}
	if sec == nil {
		return nil, fmt.Errorf("secret %q: not found", ref)
	}
	plain, err := r.box.Open(*sec)
	if err != nil {
		return nil, fmt.Errorf("secret %q: %w", ref, err)
	}
	return plain, nil
}
