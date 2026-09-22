// Package plugin defines the orthogonal contracts that compose the backup
// pipeline: Connector → Dumper → Codec* → Storage, plus Notifier. Every node is
// a plugin registered by string key (see registry.go); the core wires them into
// a pipeline without knowing concrete implementations.
package plugin

import (
	"context"
	"io"
	"time"
)

// Endpoint is a local address a Dumper can dial: either a TCP host:port or a
// unix socket path. A Connector's job is to hand the worker one of these,
// possibly after standing up an SSH tunnel.
type Endpoint struct {
	Network string // "tcp" | "unix"
	Address string // "127.0.0.1:5432" | "/var/run/postgresql/.s.PGSQL.5432"
}

// Credentials is the resolved (decrypted) secret material a Dumper needs. It is
// assembled by the core from the secret store just before Dump and never logged.
type Credentials struct {
	Username string
	Password string
	Database string
}

// Connector yields a local Endpoint the Dumper can reach. `direct`/`socket`
// return immediately; `ssh-tunnel` decorates one of them, forwarding a remote
// port/socket over SSH and returning the local side. Close tears the tunnel down.
type Connector interface {
	Open(ctx context.Context) (Endpoint, error)
	Close() error
}

// DumpMode selects how a Dumper produces bytes.
type DumpMode int

const (
	// ModeStream: the dumper writes the archive to stdout; the worker streams
	// it straight through the codec chain into storage. pg_dump/mysqldump.
	ModeStream DumpMode = iota
	// ModeStaged: the dumper writes to disk on the DB server, then the bytes are
	// fetched (SFTP/script) and the remote temp is removed. MSSQL BACKUP DATABASE.
	// Not used in MVP but the contract carries it from v1 per TZ §5.2.
	ModeStaged
)

// DumpOptions are engine-specific knobs (format, jobs, exclude-table, ...).
// Stored as opaque JSON on the task and decoded by the concrete Dumper.
type DumpOptions map[string]any

// Fetcher pulls a staged archive from a RemotePath. Only used in ModeStaged.
type Fetcher interface {
	Fetch(ctx context.Context) (io.ReadCloser, error)
	Cleanup(ctx context.Context) error
}

// RemotePath identifies a staged archive on the DB server (ModeStaged).
type RemotePath struct {
	Host string
	Path string
}

// Dumper is the DB driver: given a reachable Endpoint and Credentials it yields
// a byte stream. MVP implements ModeStream for postgres and mysql.
type Dumper interface {
	Mode() DumpMode
	// Dump returns the archive as a stream. The returned ReadCloser MUST surface
	// a non-zero process exit as a read error (not a silent EOF): the worker
	// relies on this to fail the run. ModeStream only.
	Dump(ctx context.Context, ep Endpoint, cr Credentials, opt DumpOptions) (io.ReadCloser, error)
	// DumpStaged is the ModeStaged variant. Returns ErrModeUnsupported for
	// stream-only dumpers.
	DumpStaged(ctx context.Context, ep Endpoint, cr Credentials, opt DumpOptions) (RemotePath, Fetcher, error)
	// RestoreHint returns the human restore command shown in the UI/artifact.
	RestoreHint(opt DumpOptions) string
}

// Codec is a stream transformer in the chain (compression, then encryption).
// It is defined on the WRITE side because the backup path is write-driven and
// the underlying libraries are writer-shaped: age.Encrypt returns an
// io.WriteCloser and zstd.NewWriter wraps an io.Writer. NewReader is the inverse
// for restore/verify. This deviates from TZ §5.3's Wrap(io.Reader) signature on
// purpose — see docs/adr once written.
type Codec interface {
	Name() string
	// Ext is the suffix appended to the artifact name (".zst", ".age").
	Ext() string
	// NewWriter wraps dst; bytes written to the returned WriteCloser are
	// transformed into dst. Close MUST be called to flush/finalize (age writes
	// its last STREAM chunk on Close — omitting it corrupts the artifact).
	NewWriter(dst io.Writer) (io.WriteCloser, error)
	// NewReader is the inverse, for restore and verify-read.
	NewReader(src io.Reader) (io.ReadCloser, error)
}

// ObjectMeta is what Storage reports after a successful Write.
type ObjectMeta struct {
	Key      string
	Size     int64
	Checksum string // hex SHA-256, computed by the worker via io.TeeReader
}

// Object is a catalog entry returned by List.
type Object struct {
	Key      string
	Size     int64
	Modified time.Time
}

// Storage persists artifacts. S3 uses a streaming multipart Uploader so an
// unbounded io.Reader (the pg_dump pipe) can be written without a temp file.
type Storage interface {
	Write(ctx context.Context, key string, r io.Reader) (ObjectMeta, error)
	Read(ctx context.Context, key string) (io.ReadCloser, error)
	List(ctx context.Context, prefix string) ([]Object, error)
	Delete(ctx context.Context, key string) error
}

// Event is what a run reports to the Notifier channels.
type Event struct {
	Kind    EventKind
	Task    string
	RunID   int64
	Message string
	At      time.Time
}

// EventKind enumerates notifiable events (TZ §5.5).
type EventKind string

const (
	EventSuccess        EventKind = "success"
	EventFailure        EventKind = "failure"
	EventRetentionError EventKind = "retention_error"
	EventWatchdog       EventKind = "watchdog" // v1
)

// Notifier delivers an Event to one channel (telegram/webhook/email).
type Notifier interface {
	Name() string
	Notify(ctx context.Context, ev Event) error
}
