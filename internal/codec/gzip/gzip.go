// Package gzip implements the gzip Codec (stdlib compress/gzip).
//
// gzip is the familiar, universally-decompressable option (`gunzip`, every OS's
// archive tool). zstd compresses better and faster; gzip trades ratio for the
// broadest tooling compatibility. This is the compressor behind the classic
// ".tar.gz" — for a single dump stream the tar layer adds nothing, so we expose
// gzip alone (the artifact ends in ".gz", still restorable with stock gunzip).
//
// Both sides are writer/reader-shaped: gzip.NewWriter is an io.WriteCloser whose
// Close flushes the final block, and gzip.NewReader is an io.ReadCloser.
package gzip

import (
	"compress/gzip"
	"io"

	"github.com/duskrun/duskrun/internal/plugin"
)

func init() {
	plugin.Codecs.Register("gzip", func(_ []byte) (plugin.Codec, error) {
		return Codec{}, nil
	})
}

// Codec is the gzip stream transformer.
type Codec struct{}

func (Codec) Name() string { return "gzip" }
func (Codec) Ext() string  { return ".gz" }

// NewWriter wraps dst; the returned *gzip.Writer implements io.WriteCloser and
// finalises the stream on Close.
func (Codec) NewWriter(dst io.Writer) (io.WriteCloser, error) {
	return gzip.NewWriter(dst), nil
}

// NewReader decompresses src (restore/verify); *gzip.Reader is an io.ReadCloser.
func (Codec) NewReader(src io.Reader) (io.ReadCloser, error) {
	return gzip.NewReader(src)
}
