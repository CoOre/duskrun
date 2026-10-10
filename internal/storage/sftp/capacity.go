package sftp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"

	"github.com/pkg/sftp"

	"github.com/CoOre/duskrun/internal/plugin"
)

var _ plugin.CapacityReporter = (*Storage)(nil)

// Capacity reports the remote volume holding cfg.Path via the OpenSSH
// statvfs@openssh.com extension. A server without it (non-OpenSSH, some
// appliances) fails here, and the caller simply shows no capacity.
//
// The SFTP client has no context-aware calls, so statvfs runs in its own
// goroutine; on ctx expiry the caller gets ctx.Err() and closing the storage
// tears the session down, which unblocks the abandoned request.
func (s *Storage) Capacity(ctx context.Context) (plugin.Capacity, error) {
	client, err := s.connect(ctx)
	if err != nil {
		return plugin.Capacity{}, err
	}
	type result struct {
		c   plugin.Capacity
		err error
	}
	done := make(chan result, 1)
	go func() {
		c, err := statvfs(client, s.cfg.Path)
		done <- result{c, err}
	}()
	select {
	case r := <-done:
		return r.c, r.err
	case <-ctx.Done():
		return plugin.Capacity{}, fmt.Errorf("sftp: capacity: %w", ctx.Err())
	}
}

// statvfs measures the volume holding dir. dir may not exist yet — Write
// creates it on first upload — so the nearest existing ancestor is measured:
// that is the volume the first backup will land on.
func statvfs(client *sftp.Client, dir string) (plugin.Capacity, error) {
	for {
		st, err := client.StatVFS(dir)
		if err == nil {
			// Free is f_bavail, not the library's FreeSpace (f_bfree): the
			// root-reserved share is not space a backup user can write to.
			return plugin.Capacity{
				Total: int64(st.Frsize * st.Blocks),
				Free:  int64(st.Frsize * st.Bavail),
			}, nil
		}
		parent := path.Dir(dir)
		if !errors.Is(err, os.ErrNotExist) || parent == dir {
			return plugin.Capacity{}, fmt.Errorf("sftp: statvfs %s: %w", dir, err)
		}
		dir = parent
	}
}
