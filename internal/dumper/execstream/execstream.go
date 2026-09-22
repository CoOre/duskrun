// Package execstream is the shared plumbing for streaming-dump drivers
// (postgres, mysql, ...). It runs a subprocess and exposes its stdout as an
// io.ReadCloser that folds a non-zero process exit — with a bounded stderr tail
// — into a READ ERROR at EOF, never a silent EOF. This is the contract the
// worker relies on to fail a run (plugin.Dumper.Dump docs).
//
// Concurrency: cmd.Stderr is a bounded io.Writer so os/exec drains stderr in its
// own goroutine while we read stdout — avoiding the classic full-pipe deadlock.
package execstream

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// DefaultStderrLimit bounds how much trailing stderr is retained for diagnostics.
const DefaultStderrLimit = 16 << 10

// Stream starts cmd and returns a reader over its stdout. name labels the tool
// in error messages (e.g. "pg_dump"). A failure to start is returned as-is so
// the caller can attach a tool-specific hint (e.g. "is pg_dump installed?").
func Stream(cmd *exec.Cmd, name string) (io.ReadCloser, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("execstream: stdout pipe: %w", err)
	}
	stderr := &tailWriter{limit: DefaultStderrLimit}
	cmd.Stderr = stderr // os/exec drains this concurrently — no deadlock

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &stream{cmd: cmd, name: name, stdout: stdout, stderr: stderr}, nil
}

// stream adapts a running command's stdout into an io.ReadCloser that folds the
// process exit status into the read result.
type stream struct {
	cmd    *exec.Cmd
	name   string
	stdout io.ReadCloser
	stderr *tailWriter
	once   sync.Once
	waited error
}

func (s *stream) Read(p []byte) (int, error) {
	n, err := s.stdout.Read(p)
	if err == io.EOF {
		// Drained stdout: reap the process and translate a non-zero exit.
		s.once.Do(func() { s.waited = s.cmd.Wait() })
		if s.waited != nil {
			return n, fmt.Errorf("%s failed: %w: %s", s.name, s.waited, s.stderr.String())
		}
		return n, io.EOF
	}
	return n, err
}

// Close kills the process if still running and reaps it exactly once.
func (s *stream) Close() error {
	_ = s.stdout.Close()
	s.once.Do(func() {
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		s.waited = s.cmd.Wait()
	})
	return nil
}

// tailWriter keeps only the last `limit` bytes written — enough to report why a
// tool failed without buffering unbounded stderr.
type tailWriter struct {
	limit int
	buf   []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
	return len(p), nil
}

func (t *tailWriter) String() string { return strings.TrimSpace(string(t.buf)) }
