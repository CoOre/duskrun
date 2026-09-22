package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMaskSecretsCoversWhatTheResolverSubstitutes: masking and
// core.SecretResolver must agree on what counts as a credential. Anything the
// resolver would substitute but the mask leaves alone is a field that reaches a
// viewer in plaintext.
func TestMaskSecretsCoversWhatTheResolverSubstitutes(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		hidden []string // substrings that must not survive
		kept   []string // substrings that must survive
	}{
		{
			name:   "ssh connector",
			in:     `{"ssh_host":"db.corp.io","ssh_user":"backup","private_key":"-----BEGIN OPENSSH PRIVATE KEY-----","private_key_ref":"secret://ssh/db"}`,
			hidden: []string{"BEGIN OPENSSH"},
			// The host, the user and the *_ref pointer are how an operator
			// recognises the row; a reference names a secret, it is not one.
			kept: []string{"db.corp.io", "backup", "secret://ssh/db"},
		},
		{
			name:   "s3 storage",
			in:     `{"bucket":"backups","endpoint":"https://minio.corp.io","access_key":"AKIA123","secret_key":"s3cr3t"}`,
			hidden: []string{"AKIA123", "s3cr3t"},
			kept:   []string{"backups", "minio.corp.io"},
		},
		{
			name: "sftp storage keeps the host key mode",
			in:   `{"host":"files.corp.io","user":"backup","host_key_mode":"strict","passphrase":"hunter2"}`,
			// host_key_mode is a policy setting, not a credential: masking it
			// would show the operator "••••" where a mode belongs.
			hidden: []string{"hunter2"},
			kept:   []string{"files.corp.io", "strict"},
		},
		{
			name:   "nested and arrayed credentials",
			in:     `{"outer":{"password":"p1"},"list":[{"token":"t1"},{"url":"https://ok"}]}`,
			hidden: []string{"p1", "t1"},
			kept:   []string{"https://ok"},
		},
		{
			name:   "empty credential is left alone",
			in:     `{"password":"","host":"h"}`,
			hidden: nil,
			kept:   []string{`"password":""`, "h"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(maskSecrets(json.RawMessage(c.in)))
			for _, h := range c.hidden {
				if strings.Contains(got, h) {
					t.Fatalf("%q survived masking in %s", h, got)
				}
			}
			for _, k := range c.kept {
				if !strings.Contains(got, k) {
					t.Fatalf("%q was lost by masking in %s", k, got)
				}
			}
		})
	}
}

// TestMaskSecretsUnparseableYieldsNothing: a config the masker cannot parse is
// a config it cannot redact, so it must return nothing rather than the raw row.
func TestMaskSecretsUnparseableYieldsNothing(t *testing.T) {
	got := string(maskSecrets(json.RawMessage(`{"password":"leaked"`)))
	if strings.Contains(got, "leaked") {
		t.Fatalf("unparseable config leaked through: %s", got)
	}
}

// TestUnmaskRestoresRoundTrippedConfig: masking is only safe if a client that
// resends what it read does not persist the mask over the real credential.
func TestUnmaskRestoresRoundTrippedConfig(t *testing.T) {
	stored := json.RawMessage(`{"host":"db.corp.io","port":5432,"password":"real","outer":{"token":"deep"}}`)

	// Exactly what a read-modify-write client does: read, edit one field,
	// send the whole thing back.
	masked := maskSecrets(stored)
	var edited map[string]any
	if err := json.Unmarshal(masked, &edited); err != nil {
		t.Fatal(err)
	}
	edited["port"] = 5433
	body, err := json.Marshal(edited)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(unmaskSecrets(body, stored), &got); err != nil {
		t.Fatal(err)
	}
	if got["password"] != "real" {
		t.Fatalf("password = %v, want the stored value restored", got["password"])
	}
	if outer, ok := got["outer"].(map[string]any); !ok || outer["token"] != "deep" {
		t.Fatalf("nested token = %v, want the stored value restored", got["outer"])
	}
	if got["port"] != float64(5433) {
		t.Fatalf("port = %v, want the edit to land", got["port"])
	}
}

// TestUnmaskDropsAMaskWithNothingBehindIt: a mask for a key the stored row does
// not have cannot be restored. Keeping it would hand the plugin a password of
// four bullets, which fails at delivery time instead of at save time.
func TestUnmaskDropsAMaskWithNothingBehindIt(t *testing.T) {
	out := unmaskSecrets(
		json.RawMessage(`{"host":"h","password":"`+secretMask+`"}`),
		json.RawMessage(`{"host":"h"}`),
	)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if _, present := got["password"]; present {
		t.Fatalf("password = %v, want the key dropped", got["password"])
	}
}
