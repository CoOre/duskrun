package sftp

import (
	"context"
	"fmt"

	"github.com/CoOre/duskrun/internal/plugin"
)

var _ plugin.Prober = (*Storage)(nil)

// Probe dials and authenticates, which is what a run would fail on first.
// connect already round-trips the session, so nothing more is asked of the
// server.
func (s *Storage) Probe(ctx context.Context) error {
	done := make(chan error, 1)
	go func() {
		_, err := s.connect(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("sftp: probe: %w", ctx.Err())
	}
}
