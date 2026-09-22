// Package localfs implements Storage over a local directory. Writes are atomic:
// stream to a temp file in the same dir, fsync, then rename into place, so a
// crash never leaves a half-written artifact under its final key.
package localfs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/duskrun/duskrun/internal/plugin"
)

func init() {
	plugin.Storages.Register("localfs", New)
}

// Config is the localfs plugin config (the storage.config JSON blob).
type Config struct {
	Root string `json:"root"` // base directory, e.g. "/srv/backup"
}

// New builds a localfs Storage from its JSON config.
func New(raw []byte) (plugin.Storage, error) {
	var c Config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("localfs: bad config: %w", err)
		}
	}
	if c.Root == "" {
		return nil, fmt.Errorf("localfs: root is required")
	}
	return &Storage{root: c.Root}, nil
}

// Storage is a local-filesystem artifact store rooted at root.
type Storage struct{ root string }

func (s *Storage) path(key string) string { return filepath.Join(s.root, filepath.FromSlash(key)) }

// Write streams r into root/key atomically and reports size. Checksum is left
// empty: the pipeline owns the canonical SHA-256 (computed via io.TeeReader).
func (s *Storage) Write(ctx context.Context, key string, r io.Reader) (plugin.ObjectMeta, error) {
	dst := s.path(key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return plugin.ObjectMeta{}, fmt.Errorf("localfs: mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".dr-*.tmp")
	if err != nil {
		return plugin.ObjectMeta{}, fmt.Errorf("localfs: temp: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we return before the rename.
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	n, err := io.Copy(tmp, r)
	if err != nil {
		_ = tmp.Close()
		return plugin.ObjectMeta{}, fmt.Errorf("localfs: copy: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return plugin.ObjectMeta{}, fmt.Errorf("localfs: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return plugin.ObjectMeta{}, fmt.Errorf("localfs: close: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return plugin.ObjectMeta{}, fmt.Errorf("localfs: rename: %w", err)
	}
	tmpName = "" // committed; disarm cleanup
	return plugin.ObjectMeta{Key: key, Size: n}, nil
}

// Read opens root/key for download or verify.
func (s *Storage) Read(ctx context.Context, key string) (io.ReadCloser, error) {
	f, err := os.Open(s.path(key))
	if err != nil {
		return nil, fmt.Errorf("localfs: open %s: %w", key, err)
	}
	return f, nil
}

// List returns objects whose key starts with prefix (relative to root).
func (s *Storage) List(ctx context.Context, prefix string) ([]plugin.Object, error) {
	var out []plugin.Object
	base := s.root
	err := filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, plugin.Object{Key: key, Size: info.Size(), Modified: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("localfs: list: %w", err)
	}
	return out, nil
}

// Delete removes root/key (used by the Retention Manager, M4).
func (s *Storage) Delete(ctx context.Context, key string) error {
	if err := os.Remove(s.path(key)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("localfs: delete %s: %w", key, err)
	}
	return nil
}
