// Package zstd implements the zstd Codec (pure-Go klauspost/compress).
//
// Write side (backup): zstd.NewWriter returns an *Encoder whose Close() error
// makes it a ready io.WriteCloser — Close flushes the final frame.
//
// Read side (restore/verify): zstd.NewReader returns a *Decoder whose Close()
// returns NOTHING, so it does not satisfy io.ReadCloser. We wrap it (this is the
// concrete gotcha the research flagged for this library).
package zstd

import (
	"io"

	kzstd "github.com/klauspost/compress/zstd"

	"github.com/duskrun/duskrun/internal/plugin"
)

func init() {
	plugin.Codecs.Register("zstd", func(_ []byte) (plugin.Codec, error) {
		return Codec{}, nil
	})
}

// Codec is the zstd stream transformer.
type Codec struct{}

func (Codec) Name() string { return "zstd" }
func (Codec) Ext() string  { return ".zst" }

// NewWriter wraps dst; the returned *Encoder already implements io.WriteCloser.
func (Codec) NewWriter(dst io.Writer) (io.WriteCloser, error) {
	return kzstd.NewWriter(dst, kzstd.WithEncoderLevel(kzstd.SpeedDefault))
}

// NewReader adapts the *Decoder (whose Close returns nothing) to io.ReadCloser.
func (Codec) NewReader(src io.Reader) (io.ReadCloser, error) {
	dec, err := kzstd.NewReader(src)
	if err != nil {
		return nil, err
	}
	return decoderCloser{dec}, nil
}

type decoderCloser struct{ *kzstd.Decoder }

func (d decoderCloser) Close() error { d.Decoder.Close(); return nil }
