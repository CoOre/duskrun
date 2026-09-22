package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	fage "filippo.io/age"

	drage "github.com/duskrun/duskrun/internal/codec/age"
	"github.com/duskrun/duskrun/internal/codec/zstd"
	"github.com/duskrun/duskrun/internal/plugin"
	"github.com/duskrun/duskrun/internal/storage/localfs"
)

// fakeDumper streams a fixed byte slice, optionally erroring partway through to
// exercise error propagation from the producer side.
type fakeDumper struct {
	data    []byte
	failErr error
}

func (fakeDumper) Mode() plugin.DumpMode { return plugin.ModeStream }
func (fakeDumper) DumpStaged(context.Context, plugin.Endpoint, plugin.Credentials, plugin.DumpOptions) (plugin.RemotePath, plugin.Fetcher, error) {
	return plugin.RemotePath{}, nil, plugin.ErrModeUnsupported
}
func (fakeDumper) RestoreHint(plugin.DumpOptions) string { return "hint" }
func (f fakeDumper) Dump(context.Context, plugin.Endpoint, plugin.Credentials, plugin.DumpOptions) (io.ReadCloser, error) {
	if f.failErr != nil {
		return io.NopCloser(&errReader{data: f.data, err: f.failErr}), nil
	}
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

type errReader struct {
	data []byte
	off  int
	err  error
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.off < len(r.data) {
		n := copy(p, r.data[r.off:])
		r.off += n
		return n, nil
	}
	return 0, r.err
}

// staticConnector satisfies plugin.Connector with a no-op endpoint.
type staticConnector struct{}

func (staticConnector) Open(context.Context) (plugin.Endpoint, error) {
	return plugin.Endpoint{Network: "tcp", Address: "127.0.0.1:5432"}, nil
}
func (staticConnector) Close() error { return nil }

func newLocalfs(t *testing.T) (plugin.Storage, string) {
	t.Helper()
	dir := t.TempDir()
	cfg, _ := json.Marshal(localfs.Config{Root: dir})
	st, err := localfs.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return st, dir
}

// TestPipelineRoundTrip streams payload → zstd → localfs, then verifies the
// stored artifact zstd-decodes back to the original and that the reported
// checksum equals the SHA-256 of the stored (compressed) bytes.
func TestPipelineRoundTrip(t *testing.T) {
	ctx := context.Background()
	payload := bytes.Repeat([]byte("duskrun-streaming-pipeline-0123456789\n"), 5000)
	storage, dir := newLocalfs(t)
	zc := zstd.Codec{}

	res, err := RunPipeline(ctx, PipelineInput{
		Connector: staticConnector{},
		Dumper:    fakeDumper{data: payload},
		Codecs:    []plugin.Codec{zc},
		Storage:   storage,
		Key:       "t/db/20260101_0000_db.dump.zst",
	})
	if err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}

	// Read stored bytes and check the checksum matches what the pipeline reported.
	stored, err := readAll(t, storage, res.Key)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(stored)
	if want := hex.EncodeToString(sum[:]); res.Checksum != want {
		t.Fatalf("checksum = %s, want %s (sha256 of stored bytes)", res.Checksum, want)
	}
	if res.Size != int64(len(stored)) {
		t.Fatalf("size = %d, want %d", res.Size, len(stored))
	}
	if int64(len(stored)) >= int64(len(payload)) {
		t.Fatalf("compressed size %d not smaller than payload %d", len(stored), len(payload))
	}

	// Decode and compare to the original — restore is possible with stock zstd.
	rc, err := zc.NewReader(bytes.NewReader(stored))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	decoded, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatal("decoded artifact does not match original payload")
	}
	_ = dir
}

// TestPipelineDumpErrorPropagates ensures a failing dump fails the run and does
// not silently produce a truncated artifact.
func TestPipelineDumpErrorPropagates(t *testing.T) {
	ctx := context.Background()
	storage, _ := newLocalfs(t)
	boom := errors.New("pg_dump: connection refused")

	_, err := RunPipeline(ctx, PipelineInput{
		Connector: staticConnector{},
		Dumper:    fakeDumper{data: []byte("partial data before failure"), failErr: boom},
		Codecs:    []plugin.Codec{zstd.Codec{}},
		Storage:   storage,
		Key:       "t/db/fail.dump.zst",
	})
	if err == nil {
		t.Fatal("RunPipeline succeeded despite dump error, want failure")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want wrapped %v", err, boom)
	}
}

// TestPipelineZstdAgeRoundTrip runs fake-dump → zstd → age → localfs, then
// reverses it OUTSIDE the pipeline (age-decrypt, then zstd-decode) to prove the
// stored artifact is restorable with the codecs alone.
func TestPipelineZstdAgeRoundTrip(t *testing.T) {
	ctx := context.Background()
	payload := bytes.Repeat([]byte("pg_dump-like output — rows and more rows\n"), 4000)
	storage, _ := newLocalfs(t)

	id, err := fage.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}
	ageCfg, _ := json.Marshal(drage.Config{
		Mode:       "x25519",
		Recipients: []string{id.Recipient().String()},
		Identity:   id.String(),
	})
	ageCodec, err := drage.New(ageCfg)
	if err != nil {
		t.Fatalf("age.New: %v", err)
	}
	zc := zstd.Codec{}

	res, err := RunPipeline(ctx, PipelineInput{
		Connector: staticConnector{},
		Dumper:    fakeDumper{data: payload},
		Codecs:    []plugin.Codec{zc, ageCodec}, // zstd first, then encrypt
		Storage:   storage,
		Key:       "t/db/20260101_0000_db.dump.zst.age",
	})
	if err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}

	stored, err := readAll(t, storage, res.Key)
	if err != nil {
		t.Fatal(err)
	}

	// Reverse: age-decrypt → zstd-decode → must equal the original payload.
	ar, err := ageCodec.NewReader(bytes.NewReader(stored))
	if err != nil {
		t.Fatalf("age NewReader: %v", err)
	}
	defer ar.Close()
	zr, err := zc.NewReader(ar)
	if err != nil {
		t.Fatalf("zstd NewReader: %v", err)
	}
	defer zr.Close()
	decoded, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatal("restored payload does not match original")
	}
}

func TestBuildArtifactKey(t *testing.T) {
	at := time.Date(2026, 7, 21, 2, 0, 0, 0, time.UTC)
	got := BuildArtifactKey("orders-db nightly", "orders_db", "postgres", "custom",
		[]plugin.Codec{zstd.Codec{}}, at)
	want := "orders-db-nightly/orders_db/20260721_0200_orders_db.dump.zst"
	if got != want {
		t.Fatalf("key = %q, want %q", got, want)
	}
}

func readAll(t *testing.T, s plugin.Storage, key string) ([]byte, error) {
	t.Helper()
	rc, err := s.Read(context.Background(), filepath.ToSlash(key))
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}
