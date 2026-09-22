package execstream

import (
	"context"
	"io"
	"os/exec"
	"strings"
	"testing"
)

// TestExecStreamSuccess reads a clean stdout stream through to EOF.
func TestExecStreamSuccess(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "printf duskrun")
	rc, err := Stream(cmd, "sh")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "duskrun" {
		t.Fatalf("stdout = %q, want %q", got, "duskrun")
	}
}

// TestExecStreamExitCode surfaces a non-zero exit as a read error carrying the
// exit status and the stderr tail — not a silent EOF.
func TestExecStreamExitCode(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "echo boom 1>&2; exit 3")
	rc, err := Stream(cmd, "sh")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer rc.Close()
	_, err = io.ReadAll(rc)
	if err == nil {
		t.Fatal("ReadAll returned nil error, want failure on non-zero exit")
	}
	msg := err.Error()
	if !strings.Contains(msg, "exit") {
		t.Errorf("error %q missing exit status", msg)
	}
	if !strings.Contains(msg, "boom") {
		t.Errorf("error %q missing stderr tail 'boom'", msg)
	}
}
