package localfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"syscall"

	"github.com/CoOre/duskrun/internal/plugin"
)

var _ plugin.CapacityReporter = (*Storage)(nil)

// Capacity reports the size of the filesystem root lives on. statfs ignores
// ctx and can block indefinitely on a dead network mount, so it runs in its own
// goroutine: the caller gets ctx.Err() on time, and the goroutine is abandoned
// to finish (or not) on its own.
func (s *Storage) Capacity(ctx context.Context) (plugin.Capacity, error) {
	type result struct {
		c   plugin.Capacity
		err error
	}
	done := make(chan result, 1)
	go func() {
		c, err := statfs(s.root)
		done <- result{c, err}
	}()
	select {
	case r := <-done:
		return r.c, r.err
	case <-ctx.Done():
		return plugin.Capacity{}, fmt.Errorf("localfs: capacity: %w", ctx.Err())
	}
}

// statfs measures the volume holding root. root may not exist yet — Write
// creates it lazily — so the nearest existing ancestor is measured instead:
// that is the volume the first backup will land on.
func statfs(root string) (plugin.Capacity, error) {
	dir, err := filepath.Abs(root)
	if err != nil {
		return plugin.Capacity{}, fmt.Errorf("localfs: capacity: %w", err)
	}
	for {
		var st syscall.Statfs_t
		err := syscall.Statfs(dir, &st)
		if err == nil {
			// Bsize is int64 on linux and uint32 on darwin; uint64 is a real
			// conversion on both, so neither build trips unconvert.
			bsize := uint64(st.Bsize)
			return plugin.Capacity{
				Total: int64(st.Blocks * bsize),
				Free:  int64(st.Bavail * bsize),
			}, nil
		}
		parent := filepath.Dir(dir)
		if !errors.Is(err, fs.ErrNotExist) || parent == dir {
			return plugin.Capacity{}, fmt.Errorf("localfs: statfs %s: %w", dir, err)
		}
		dir = parent
	}
}
