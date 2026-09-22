package api

import (
	"encoding/json"
	"strings"
)

// Masking of inline credentials in plugin configs.
//
// Every configurable thing in duskrun — a connection, a storage, a notification
// channel — carries a free-form JSON config that the plugin interprets. The
// supported way to put a credential in one is a "<key>_ref" pointing at the
// secret store, and core.SecretResolver substitutes the decrypted value at use
// time. But the API also accepts an inline value under "<key>", because some
// deployments would rather not manage a secret row for a throwaway MinIO key.
//
// Those inline values must not come back out of a GET. The listings are
// readable by `viewer`, whose whole definition is "may see that backups are
// happening" — handing that role an S3 secret key or an SSH private key out of
// a connection listing would make the role meaningless. Masking is what lets
// the config stay visible (endpoint, bucket, ssh_host are exactly what an
// operator needs to recognise a row) while the credentials in it do not.
//
// Masking creates a second problem that unmaskSecrets solves: a client editing
// a row resends the whole config it read, which is how config keys the form
// does not model survive an edit. Without restoration, every such edit would
// persist "••••" over a working credential.

// secretishKeys name the config fields whose values are credentials.
//
// passphrase is here because an encrypted SSH private key's passphrase is one,
// and it is the only such field whose name does not end in a word from this
// list — it would otherwise be the single credential that leaked.
var secretishKeys = []string{"token", "password", "passphrase", "secret", "key", "auth"}

// secretMask stands in for a credential value in a listing. It is also what a
// client sends back when it round-trips a config it read, so the write path
// recognises it and restores the stored value.
const secretMask = "••••"

// maskSecrets redacts credential-looking values anywhere in a config, leaving
// everything an operator needs to recognise the row (url, chat_id, endpoint,
// ssh_host) and the *_ref pointers themselves, which name a secret rather than
// containing one.
//
// It recurses into nested objects and arrays because core.SecretResolver does:
// a credential the resolver would substitute at any depth is a credential this
// must hide at that depth, or the two disagree about what counts as a secret
// and the gap is a leak.
func maskSecrets(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var cfg any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		// Unparseable config: return nothing rather than risk leaking it.
		return json.RawMessage(`{}`)
	}
	if !maskValue(cfg) {
		return raw
	}
	out, err := json.Marshal(cfg)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return out
}

// maskValue replaces credential values in place, reporting whether it changed
// anything so an untouched config can be returned byte-for-byte.
func maskValue(v any) bool {
	switch node := v.(type) {
	case map[string]any:
		masked := false
		for key, val := range node {
			if child, ok := val.(string); ok {
				if isSecretish(key) && child != "" {
					node[key] = secretMask
					masked = true
				}
				continue
			}
			if maskValue(val) {
				masked = true
			}
		}
		return masked
	case []any:
		masked := false
		for _, item := range node {
			if maskValue(item) {
				masked = true
			}
		}
		return masked
	}
	return false
}

// unmaskSecrets restores the values maskSecrets hid, matching its recursion so
// a nested credential survives an edit the same way a top-level one does.
func unmaskSecrets(req, current json.RawMessage) json.RawMessage {
	if len(req) == 0 || len(current) == 0 {
		return req
	}
	var in, stored any
	if json.Unmarshal(req, &in) != nil || json.Unmarshal(current, &stored) != nil {
		return req
	}
	if !unmaskValue(in, stored) {
		return req
	}
	out, err := json.Marshal(in)
	if err != nil {
		return req
	}
	return out
}

// unmaskValue walks the incoming config alongside the stored one, replacing
// masks with what was stored at the same path. A mask with nothing behind it is
// dropped rather than kept: persisting it would hand the plugin a password of
// four bullets, which fails at delivery time instead of at save time.
func unmaskValue(in, stored any) bool {
	switch node := in.(type) {
	case map[string]any:
		prevMap, _ := stored.(map[string]any)
		restored := false
		for key, val := range node {
			if s, ok := val.(string); ok {
				if s != secretMask {
					continue
				}
				if prev, ok := prevMap[key]; ok {
					node[key] = prev
				} else {
					delete(node, key)
				}
				restored = true
				continue
			}
			if unmaskValue(val, prevMap[key]) {
				restored = true
			}
		}
		return restored
	case []any:
		prevSlice, _ := stored.([]any)
		restored := false
		for i, item := range node {
			// Positional: an array whose length changed cannot be matched up,
			// so anything past the stored end simply finds nothing to restore.
			var prev any
			if i < len(prevSlice) {
				prev = prevSlice[i]
			}
			if unmaskValue(item, prev) {
				restored = true
			}
		}
		return restored
	}
	return false
}

// isSecretish matches a credential field but not its "_ref" pointer.
func isSecretish(key string) bool {
	if strings.HasSuffix(key, "_ref") {
		return false
	}
	for _, s := range secretishKeys {
		if key == s || strings.HasSuffix(key, "_"+s) {
			return true
		}
	}
	return false
}
