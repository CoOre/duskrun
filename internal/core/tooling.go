package core

import (
	"fmt"
	"os/exec"
	"strings"
)

// RequiredTool maps a DB engine to the external CLI it streams from. An empty
// string means the engine needs no external binary (e.g. a pure-Go dumper).
func RequiredTool(engine string) string {
	switch engine {
	case "postgres":
		return "pg_dump"
	case "mysql":
		return "mysqldump"
	case "mongodb":
		return "mongodump"
	case "redis":
		return "redis-cli"
	default:
		return ""
	}
}

// versionArgs are tried in order to coax a version string out of a tool; the
// first invocation that exits zero with output wins. `go`, for instance, rejects
// --version but answers `go version`.
var versionArgs = [][]string{{"--version"}, {"version"}, {"-V"}}

// CheckTool verifies an external tool is on PATH and returns its version line.
// Presence is decided by PATH lookup; the version is best-effort.
func CheckTool(name string) (string, error) {
	if _, err := exec.LookPath(name); err != nil {
		return "", fmt.Errorf("tool %q not found in PATH: %w", name, err)
	}
	for _, args := range versionArgs {
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			continue
		}
		if v := firstLine(out); v != "" {
			return v, nil
		}
	}
	return name + " (present; version unknown)", nil
}

// firstLine returns the first non-empty trimmed line of b.
func firstLine(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s
		}
	}
	return ""
}
