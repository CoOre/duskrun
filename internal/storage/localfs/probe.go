package localfs

import (
	"context"
	"fmt"

	"github.com/CoOre/duskrun/internal/plugin"
)

var _ plugin.Prober = (*Storage)(nil)

// Probe checks the volume under root answers. A dead network mount is the
// failure that matters here, and statfs is where it shows: it errors, or hangs
// until ctx gives up.
func (s *Storage) Probe(ctx context.Context) error {
	done := make(chan error, 1)
	go func() {
		_, err := statfs(s.root)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("localfs: probe: %w", ctx.Err())
	}
}
