// Package tar implements a tar-archive Codec that wraps the single dump stream
// as the sole member of a POSIX tar archive. Chained with gzip ([tar, gzip]) it
// yields the familiar ".tar.gz"; alone it yields ".tar".
//
// Unlike gzip/zstd, tar is NOT a streaming transform: a tar header must carry
// the member's byte size BEFORE its body, yet the dump arrives as a stream of
// unknown size. So this codec necessarily BUFFERS the whole dump to a temp FILE
// (on disk, never memory — safe for large DBs) and emits header+body+padding on
// Close. This is a deliberate exception to Duskrun's no-temp-file streaming
// design (pipeline.go), accepted because a tar container fundamentally cannot be
// produced from an unknown-size stream.
package tar

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/duskrun/duskrun/internal/plugin"
)

func init() {
	plugin.Codecs.Register("tar", New)
}

const defaultMember = "dump"

// Config sets the archived member's filename (cosmetic: the name seen after
// `tar xf`). Defaults to "dump" when empty.
type Config struct {
	Name string `json:"name"`
}

// New builds a tar Codec from its JSON config.
func New(raw []byte) (plugin.Codec, error) {
	c := Config{Name: defaultMember}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("tar: bad config: %w", err)
		}
	}
	if c.Name == "" {
		c.Name = defaultMember
	}
	return Codec{cfg: c}, nil
}

// Codec is the tar archiver.
type Codec struct{ cfg Config }

func (Codec) Name() string { return "tar" }
func (Codec) Ext() string  { return ".tar" }

// NewWriter buffers everything written to a temp file, then on Close emits a
// one-member tar archive (header sized from the buffered bytes) into dst.
func (c Codec) NewWriter(dst io.Writer) (io.WriteCloser, error) {
	tmp, err := os.CreateTemp("", "duskrun-tar-*")
	if err != nil {
		return nil, fmt.Errorf("tar: temp file: %w", err)
	}
	// Unlink immediately: the open fd keeps the bytes reachable, but the name is
	// gone at once, so a crash mid-backup leaves no stray file behind.
	_ = os.Remove(tmp.Name())
	return &tarWriter{dst: dst, tmp: tmp, name: c.cfg.Name}, nil
}

// NewReader unwraps the single tar member (restore/verify).
func (Codec) NewReader(src io.Reader) (io.ReadCloser, error) {
	tr := tar.NewReader(src)
	if _, err := tr.Next(); err != nil {
		return nil, fmt.Errorf("tar: read header: %w", err)
	}
	return io.NopCloser(tr), nil
}

// tarWriter accumulates the dump in tmp and finalises the archive on Close.
type tarWriter struct {
	dst  io.Writer
	tmp  *os.File
	name string
	size int64
}

func (w *tarWriter) Write(p []byte) (int, error) {
	n, err := w.tmp.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *tarWriter) Close() error {
	defer w.tmp.Close() // fd close also frees the already-unlinked temp file
	if _, err := w.tmp.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("tar: seek buffer: %w", err)
	}
	tw := tar.NewWriter(w.dst)
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     w.name,
		Mode:     0o600,
		Size:     w.size,
		ModTime:  time.Unix(0, 0), // deterministic; the artifact key already carries the timestamp
	}); err != nil {
		return fmt.Errorf("tar: write header: %w", err)
	}
	if _, err := io.Copy(tw, w.tmp); err != nil {
		return fmt.Errorf("tar: write body: %w", err)
	}
	return tw.Close() // terminating zero blocks
}
