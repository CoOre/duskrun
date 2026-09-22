// Package config loads Duskrun configuration from a file plus environment
// overrides. The master key never lives in the file — only a reference to where
// it comes from (env var or file path), resolved at startup.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// DefaultRetentionCron runs the sweep nightly at 03:30, after the usual backup
// window so a fresh artifact is already in the catalog when keep_last counts.
const DefaultRetentionCron = "30 3 * * *"

// Config is the resolved runtime configuration.
type Config struct {
	// DBPath is the SQLite metadata file. Default "duskrun.db".
	DBPath string
	// ListenAddr for the REST API (M5). Default ":8080".
	ListenAddr string
	// WorkerLimit is the global concurrency cap (TZ §7). Default 4.
	WorkerLimit int
	// MasterKey is the raw master key bytes, resolved from MasterKeySource.
	MasterKey []byte
	// LogFormat is "json" (default) or "text".
	LogFormat string
	// APIToken is the bearer token guarding the REST API (M5). Empty disables the
	// API (all protected routes reject).
	APIToken string
	// RetentionCron schedules the retention sweep (standard 5-field cron).
	// Default "30 3 * * *"; set to "off" to disable the scheduled sweep and
	// leave retention to POST /api/retention/sweep.
	RetentionCron string
	// TrustProxy tells the API that it sits behind a reverse proxy, so login
	// throttling may key on X-Forwarded-For. Off by default: honouring the
	// header when nothing sets it lets a caller choose its own throttle bucket,
	// while ignoring it behind a proxy throttles every user as one address.
	TrustProxy bool
}

// Load builds a Config from environment variables, applying defaults. A file
// loader can be layered later; env is the source of truth for secrets.
//
// Env:
//
//	DUSKRUN_DB_PATH        default "duskrun.db"
//	DUSKRUN_LISTEN         default ":8080"
//	DUSKRUN_WORKERS        default 4
//	DUSKRUN_LOG_FORMAT     default "json"
//	DUSKRUN_RETENTION_CRON default "30 3 * * *" ("off" disables the sweep)
//	DUSKRUN_TRUSTED_PROXY  default false (trust X-Forwarded-For for throttling)
//	DUSKRUN_MASTER_KEY     raw master key (mutually exclusive with _FILE)
//	DUSKRUN_MASTER_KEY_FILE path to a file holding the master key
func Load() (*Config, error) {
	c := &Config{
		DBPath:        env("DUSKRUN_DB_PATH", "duskrun.db"),
		ListenAddr:    env("DUSKRUN_LISTEN", ":8080"),
		WorkerLimit:   envInt("DUSKRUN_WORKERS", 4),
		LogFormat:     env("DUSKRUN_LOG_FORMAT", "json"),
		APIToken:      os.Getenv("DUSKRUN_API_TOKEN"),
		RetentionCron: retentionCron(),
		TrustProxy:    envBool("DUSKRUN_TRUSTED_PROXY", false),
	}

	key, keyFile := os.Getenv("DUSKRUN_MASTER_KEY"), os.Getenv("DUSKRUN_MASTER_KEY_FILE")
	switch {
	case key != "" && keyFile != "":
		return nil, fmt.Errorf("config: set only one of DUSKRUN_MASTER_KEY or DUSKRUN_MASTER_KEY_FILE")
	case key != "":
		c.MasterKey = []byte(key)
	case keyFile != "":
		b, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, fmt.Errorf("config: read master key file: %w", err)
		}
		c.MasterKey = b
	default:
		// Allowed for read-only CLI commands; the serve/run paths validate it.
	}
	return c, nil
}

// placeholderMarker appears in every credential .env.example ships. A value
// containing it was copied, not generated.
//
// .env.example keeps its credential variables empty so docker compose's own
// `${VAR:?...}` guard fires on an unedited copy. This check is the layer under
// that one, for the operator who fills the blank in with the instruction text,
// or who runs the binary without compose at all. Refusing here is the whole
// point: a master key that ships in a public repository encrypts nothing, and
// an instance that starts with it looks exactly like one that is secured.
const placeholderMarker = "change-me"

// RequireMasterKey errors if no master key was configured — call from paths that
// must decrypt secrets (serve, run).
func (c *Config) RequireMasterKey() error {
	if len(c.MasterKey) == 0 {
		return fmt.Errorf("config: master key required (set DUSKRUN_MASTER_KEY or _FILE)")
	}
	if isPlaceholder(string(c.MasterKey)) {
		return fmt.Errorf("config: DUSKRUN_MASTER_KEY is still the example placeholder — " +
			"generate a real one with `openssl rand -base64 48`")
	}
	return nil
}

// RequireUsableCredentials rejects placeholder values in every credential the
// daemon takes from the environment. Call it from serve.
func (c *Config) RequireUsableCredentials() error {
	if err := c.RequireMasterKey(); err != nil {
		return err
	}
	if isPlaceholder(c.APIToken) {
		return fmt.Errorf("config: DUSKRUN_API_TOKEN is still the example placeholder — " +
			"generate a real one with `openssl rand -hex 32`")
	}
	return nil
}

// isPlaceholder reports whether a value is one of the example's own strings.
func isPlaceholder(v string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(v)), placeholderMarker)
}

// retentionCron resolves the sweep schedule. Unlike env(), an explicitly empty
// value is NOT treated as unset: `DUSKRUN_RETENTION_CRON=` in a compose file is
// how an operator turns retention off, and falling back to the nightly default
// there would delete artifacts they meant to keep. Only an absent variable
// takes the default; empty and "off" both disable the sweep.
func retentionCron() string {
	v, ok := os.LookupEnv("DUSKRUN_RETENTION_CRON")
	if !ok {
		return DefaultRetentionCron
	}
	v = strings.TrimSpace(v)
	if v == "" || v == "off" {
		return ""
	}
	return v
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// envBool reads a boolean flag. Anything strconv.ParseBool accepts works
// ("1", "true", "yes" does not); an unparseable value keeps the default rather
// than failing startup over a typo in an optional switch.
func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
