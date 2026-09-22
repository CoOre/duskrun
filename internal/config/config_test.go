package config

import "testing"

// TestRetentionCron covers the disable paths specifically. Treating an
// explicitly empty value as "unset" would be dangerous here: writing
// `DUSKRUN_RETENTION_CRON=` in a compose file is how an operator turns retention
// off, and silently falling back to the nightly default would start deleting
// artifacts they meant to keep.
func TestRetentionCron(t *testing.T) {
	cases := []struct {
		name  string
		set   bool
		value string
		want  string
	}{
		{"unset takes the default", false, "", DefaultRetentionCron},
		{"explicit empty disables", true, "", ""},
		{"whitespace disables", true, "   ", ""},
		{"off disables", true, "off", ""},
		{"custom schedule is kept", true, "0 5 * * *", "0 5 * * *"},
		{"surrounding space is trimmed", true, "  0 5 * * *  ", "0 5 * * *"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.set {
				t.Setenv("DUSKRUN_RETENTION_CRON", c.value)
			}
			if got := retentionCron(); got != c.want {
				t.Errorf("retentionCron() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestPlaceholderCredentialsAreRefused: .env.example ships these variables
// empty so compose's own guard fires, but nothing stops an operator from
// pasting the instruction text into the blank, or from running the binary
// without compose. A master key that appears in a public repository encrypts
// nothing, and an instance started with one is indistinguishable from a secured
// one until someone reads the source.
func TestPlaceholderCredentialsAreRefused(t *testing.T) {
	real := "Ql9m2vXpR7t4Ks8Ld1Nf6Bz3Hy0Cw5Ee7Aa9Uu2Ii4Oo6Pp8Tt0"

	cases := []struct {
		name      string
		masterKey string
		apiToken  string
		wantErr   bool
	}{
		{"generated values pass", real, "0123456789abcdef", false},
		{"example master key", "change-me-generate-with-openssl-rand-base64-48", "0123456789abcdef", true},
		{"example api token", real, "change-me-generate-with-openssl-rand-hex-32", true},
		// Case and stray whitespace must not be a way around it: an operator
		// editing the file by hand produces both.
		{"different case", "CHANGE-ME-generate-with-openssl", "0123456789abcdef", true},
		{"padded", "  change-me-x  ", "0123456789abcdef", true},
		// No token at all is a supported configuration (user accounts only),
		// so an empty token must not be read as a placeholder.
		{"no api token", real, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &Config{MasterKey: []byte(c.masterKey), APIToken: c.apiToken}
			err := cfg.RequireUsableCredentials()
			if c.wantErr && err == nil {
				t.Fatal("a placeholder credential was accepted")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("a real credential was refused: %v", err)
			}
		})
	}
}

// TestMissingMasterKeyStillRefused: the placeholder check must not have
// displaced the original emptiness check.
func TestMissingMasterKeyStillRefused(t *testing.T) {
	if err := (&Config{}).RequireUsableCredentials(); err == nil {
		t.Fatal("an instance with no master key at all was allowed to serve")
	}
}
