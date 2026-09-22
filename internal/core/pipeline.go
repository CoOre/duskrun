package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/duskrun/duskrun/internal/plugin"
)

// PipelineInput is a fully-resolved pipeline ready to execute: concrete plugin
// instances plus the credentials, options and target key.
type PipelineInput struct {
	Connector plugin.Connector
	Dumper    plugin.Dumper
	Codecs    []plugin.Codec // applied in order, e.g. [zstd, age]
	Storage   plugin.Storage
	Creds     plugin.Credentials
	DumpOpts  plugin.DumpOptions
	Key       string
	// OnBytes, if set, is called with the cumulative post-codec byte count as the
	// stream flows into storage: throttled during the copy, plus a final exact
	// tick at completion. Optional; nil disables live byte progress.
	OnBytes func(total int64)
}

// Result reports what the pipeline produced.
type Result struct {
	Key      string
	Size     int64
	Checksum string // hex SHA-256 of the stored (post-codec) bytes
}

// RunPipeline executes dump → codec chain → storage as a single stream, without
// temp files. The dump is produced by a subprocess writing into an io.Pipe in a
// goroutine; the codec chain wraps the pipe's writer; the storage consumes the
// pipe's reader through an io.TeeReader that computes the artifact's SHA-256 on
// the fly. Errors from either side propagate — a failed dump or a failed upload
// both fail the whole run.
func RunPipeline(ctx context.Context, in PipelineInput) (Result, error) {
	ep, err := in.Connector.Open(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("connector: %w", err)
	}
	defer in.Connector.Close()

	dump, err := in.Dumper.Dump(ctx, ep, in.Creds, in.DumpOpts)
	if err != nil {
		return Result{}, fmt.Errorf("dumper: %w", err)
	}
	defer dump.Close()

	pr, pw := io.Pipe()
	prodErr := make(chan error, 1)

	// Producer: raw dump → codec chain → pipe writer. Runs async so the pipe
	// never deadlocks (a write with no reader would block).
	go func() {
		prodErr <- feed(pw, dump, in.Codecs)
	}()

	hasher := sha256.New()
	src := io.TeeReader(pr, hasher)
	var counter *countReader
	if in.OnBytes != nil {
		counter = &countReader{r: src, onBytes: in.OnBytes, now: time.Now, interval: 200 * time.Millisecond}
		src = counter
	}

	meta, storageErr := in.Storage.Write(ctx, in.Key, src)
	if storageErr != nil {
		// Unblock the producer if it is still writing into the pipe.
		pr.CloseWithError(storageErr)
	}
	producerErr := <-prodErr
	// Emit a final exact byte count once the upload has drained the stream.
	if counter != nil && storageErr == nil {
		in.OnBytes(counter.total)
	}

	switch {
	case storageErr != nil:
		return Result{}, fmt.Errorf("storage: %w", storageErr)
	case producerErr != nil:
		return Result{}, producerErr
	}
	return Result{
		Key:      in.Key,
		Size:     meta.Size,
		Checksum: hex.EncodeToString(hasher.Sum(nil)),
	}, nil
}

// feed writes src through the codec chain into pw and always closes pw (with the
// terminating error, or nil for a clean EOF). Codecs are layered so that data
// flows src → codec[0] → codec[1] → … → pw; writers are closed inner-to-outer so
// each flushes into the next (age, for one, finalizes on Close).
func feed(pw *io.PipeWriter, src io.Reader, codecs []plugin.Codec) (err error) {
	var closers []io.WriteCloser
	var dst io.Writer = pw
	// Wrap from the pipe inward, in reverse codec order, so codec[0] ends up
	// innermost (closest to the raw dump).
	for i := len(codecs) - 1; i >= 0; i-- {
		w, werr := codecs[i].NewWriter(dst)
		if werr != nil {
			pw.CloseWithError(werr)
			return fmt.Errorf("codec %s: %w", codecs[i].Name(), werr)
		}
		closers = append(closers, w) // closers[0] is outermost
		dst = w
	}

	_, copyErr := io.Copy(dst, src)

	// Close inner-to-outer to flush the chain in order.
	var closeErr error
	for i := len(closers) - 1; i >= 0; i-- {
		if e := closers[i].Close(); e != nil && closeErr == nil {
			closeErr = e
		}
	}

	err = copyErr
	if err == nil {
		err = closeErr
	}
	pw.CloseWithError(err) // nil → normal EOF for the reader
	return err
}

// countReader tallies bytes read and reports the running total via onBytes, at
// most once per interval (plus whenever a read errors). It wraps the tee at the
// storage-facing end of the pipe, so the count is post-codec bytes uploaded.
type countReader struct {
	r        io.Reader
	total    int64
	onBytes  func(total int64)
	now      func() time.Time
	interval time.Duration
	last     time.Time
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.total += int64(n)
		now := c.now()
		if c.last.IsZero() || now.Sub(c.last) >= c.interval {
			c.last = now
			c.onBytes(c.total)
		}
	}
	return n, err
}

// CloseStorage releases a storage that holds a live session. plugin.Storage has
// no Close in the contract — localfs and s3 open nothing per instance — but sftp
// keeps a TCP+ssh+sftp session with its goroutines for the life of the value, so
// every caller that builds one must release it when it is done with it.
func CloseStorage(s plugin.Storage) {
	if c, ok := s.(io.Closer); ok {
		_ = c.Close()
	}
}

// BuildArtifactKey composes the storage key per TZ §5.4:
//
//	{task}/{db}/{ts}_{db}{dumpExt}{codecExts}
func BuildArtifactKey(task, db, engine, format string, codecs []plugin.Codec, at time.Time) string {
	var exts strings.Builder
	for _, c := range codecs {
		exts.WriteString(c.Ext())
	}
	ts := at.UTC().Format("20060102_1504")
	return fmt.Sprintf("%s/%s/%s_%s%s%s",
		slug(task), slug(db), ts, slug(db), dumpExt(engine, format), exts.String())
}

// ArtifactPrefix is the storage prefix every artifact of a task shares — the
// first segment of BuildArtifactKey. The Retention Manager lists under it to
// find orphan objects.
func ArtifactPrefix(task string) string {
	return slug(task) + "/"
}

func dumpExt(engine, format string) string {
	if engine == "postgres" && format == "plain" {
		return ".sql"
	}
	switch engine {
	case "mongodb":
		// mongodump --archive produces one archive file, not a directory dump;
		// ".archive" is what the mongo tooling calls it.
		return ".archive"
	case "mssql":
		return ".bak"
	case "redis":
		return ".rdb"
	case "sqlite":
		return ".sqlite"
	default:
		return ".dump"
	}
}

// slug keeps key segments filesystem/S3-safe.
func slug(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	repl := func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}
	return strings.Map(repl, s)
}
