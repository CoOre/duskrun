// Package core holds the domain model and the execution engine (scheduler,
// queue, worker, retention). The model mirrors TZ §6 and the metadata schema in
// migrations/0001_init.sql.
package core

import (
	"encoding/json"
	"fmt"
	"time"
)

// RunStatus is the lifecycle of a single execution (TZ §6).
type RunStatus string

const (
	StatusQueued  RunStatus = "queued"  // enqueued, awaiting a worker
	StatusRunning RunStatus = "running" // claimed by a worker
	StatusSuccess RunStatus = "success"
	StatusFailed  RunStatus = "failed"
	StatusSkipped RunStatus = "skipped" // misfire policy = skip
)

// MisfirePolicy decides what happens to a schedule tick that could not run on
// time (worker saturation, or a run still active at restart). TZ §7.
type MisfirePolicy string

const (
	MisfireRunOnceNow MisfirePolicy = "run_once_now"
	MisfireSkip       MisfirePolicy = "skip"
)

// Connection is a reusable way to reach a DB server: an engine + a connector
// config (direct/socket/ssh-tunnel) + a reference to credentials in the secret
// store. connector_config and secret material stay opaque to the core.
type Connection struct {
	ID              int64
	Name            string
	Engine          string          // "postgres" | "mysql" | ...
	ConnectorType   string          // "direct" | "socket" | "ssh-tunnel"
	ConnectorConfig json.RawMessage // decoded by the concrete Connector
	Username        string          // DB login user; secret carries only the password
	SecretRef       string          // "secret://db/pg-primary"
	CreatedAt       time.Time
}

// Storage is a configured artifact destination (local FS, S3, ...).
type Storage struct {
	ID        int64
	Name      string
	Type      string          // "localfs" | "s3" | "sftp"
	Config    json.RawMessage // decoded by the concrete Storage plugin
	SecretRef string
	CreatedAt time.Time
}

// Retention captures the keep policy for a task. keep_last and GFS may both be
// active (TZ §8); the Retention Manager unions them.
type Retention struct {
	KeepLast int  `json:"keep_last,omitempty"` // 0 = off
	GFS      *GFS `json:"gfs,omitempty"`       // nil = off
}

// Enabled reports whether any keep rule is configured. An unconfigured policy
// means "keep everything": callers must not hand it to Forget, which would
// classify every artifact as deletable.
func (r Retention) Enabled() bool {
	if r.KeepLast > 0 {
		return true
	}
	return r.GFS != nil && (r.GFS.Daily > 0 || r.GFS.Weekly > 0 || r.GFS.Monthly > 0)
}

// Validate rejects a policy that cannot mean what it says. A negative count is
// the dangerous case: Forget treats every count <= 0 as an inactive rule, so a
// typo would silently switch retention OFF on a task whose operator believes it
// is pruning — or, with keep_last alone mistyped, leave GFS as the only rule and
// delete far more than intended. Both failures are invisible until the artifacts
// are already gone, so they are refused at the door.
func (r Retention) Validate() error {
	if r.KeepLast < 0 {
		return fmt.Errorf("keep_last must be >= 0 (0 = off), got %d", r.KeepLast)
	}
	if r.GFS == nil {
		return nil
	}
	for _, f := range []struct {
		name string
		v    int
	}{
		{"gfs.daily", r.GFS.Daily},
		{"gfs.weekly", r.GFS.Weekly},
		{"gfs.monthly", r.GFS.Monthly},
	} {
		if f.v < 0 {
			return fmt.Errorf("%s must be >= 0 (0 = off), got %d", f.name, f.v)
		}
	}
	return nil
}

// Normalized drops an all-zero GFS block. The UI always sends the three fields,
// so a task with GFS switched off would otherwise persist `{"gfs":{...zeros}}`
// — a policy that reads as "GFS configured" to anything checking for the key
// while behaving as "off". Storing nil keeps the two in agreement.
func (r Retention) Normalized() Retention {
	if r.GFS != nil && r.GFS.Daily == 0 && r.GFS.Weekly == 0 && r.GFS.Monthly == 0 {
		r.GFS = nil
	}
	return r
}

// GFS is grandfather-father-son retention.
type GFS struct {
	Daily   int `json:"daily"`
	Weekly  int `json:"weekly"`
	Monthly int `json:"monthly"`
}

// Task is the unit of scheduling: a connection + dump options + a codec chain +
// a storage + cron + retention + notifiers.
type Task struct {
	ID           int64
	Name         string
	ConnectionID int64
	DumperOpts   json.RawMessage // engine-specific
	CodecChain   []string        // ordered: e.g. ["zstd","age"]
	StorageID    int64
	Cron         string
	Retention    Retention
	Notifiers    []string
	Enabled      bool
	Misfire      MisfirePolicy
	Retries      int           // additional attempts after the first
	Timeout      time.Duration // per-run
	// Watchdog is how stale the last SUCCESSFUL run may get before the task is
	// reported. WatchdogAuto derives it from Cron; WatchdogOff disables it.
	Watchdog time.Duration
	// EnabledAt is when the task last became enabled. The watchdog measures from
	// the later of this and the last success, so un-pausing grants a fresh
	// window instead of alerting immediately.
	EnabledAt time.Time
	CreatedAt time.Time
}

// Watchdog threshold sentinels stored in task.watchdog_sec.
const (
	// WatchdogAuto derives the threshold from the task's cron, so a task is
	// watched from the moment it is created without anyone configuring it.
	WatchdogAuto time.Duration = 0
	// WatchdogOff disables the watchdog for a task. Stored as -1.
	WatchdogOff time.Duration = -1 * time.Second
)

// Run is one execution of a Task.
type Run struct {
	ID         int64
	TaskID     int64
	Status     RunStatus
	Worker     string
	Attempt    int
	StartedAt  *time.Time
	FinishedAt *time.Time
	Error      string
	Log        string
	CreatedAt  time.Time
}

// Artifact is a produced backup file recorded in the catalog (TZ §6).
type Artifact struct {
	ID          int64
	RunID       int64
	StorageID   int64
	Key         string
	Size        int64
	Checksum    string // hex SHA-256
	CreatedAt   time.Time
	ExpiresHint *time.Time
}

// Sweep is one completed pass of the Retention Manager over every task: the
// artifacts it deleted, the bytes that freed, and the orphan objects it saw in
// storage but not in the catalog. A sweep that failed part-way still records
// what it managed to delete, plus the error that stopped it.
type Sweep struct {
	ID           int64
	StartedAt    time.Time
	FinishedAt   time.Time
	Status       SweepStatus
	Source       SweepSource
	DeletedCount int
	FreedBytes   int64
	OrphanCount  int
	Error        string
}

// SweepStatus is the terminal outcome of a sweep.
type SweepStatus string

const (
	SweepSuccess SweepStatus = "success"
	SweepFailed  SweepStatus = "failed"
)

// SweepSource records what triggered a sweep.
type SweepSource string

const (
	SweepSchedule SweepSource = "schedule" // the retention cron
	SweepManual   SweepSource = "manual"   // POST /api/retention/sweep
)

// NotifierChannel is a configured delivery channel. Tasks reference it by Name;
// Type selects the plugin that implements it.
type NotifierChannel struct {
	ID     int64
	Name   string
	Type   string          // plugin name: "log" | "telegram" | "webhook"
	Config json.RawMessage // opaque to the core; *_ref keys resolve from secrets
	// Events is the set of event kinds this channel forwards. Empty means the
	// channel is configured but subscribed to nothing, so it delivers nothing.
	Events    []string
	Enabled   bool
	CreatedAt time.Time
}

// Wants reports whether the channel forwards this event kind.
func (c NotifierChannel) Wants(kind string) bool {
	if !c.Enabled {
		return false
	}
	for _, e := range c.Events {
		if e == kind {
			return true
		}
	}
	return false
}

// Notification is one delivery attempt, recorded so an operator can answer
// "did the alert actually go out?" without reading daemon logs.
type Notification struct {
	ID        int64
	Kind      string
	Task      string
	RunID     *int64
	Channel   string
	Status    NotificationStatus
	Error     string
	CreatedAt time.Time
}

// NotificationStatus is the outcome of a delivery attempt.
type NotificationStatus string

const (
	NotificationSent   NotificationStatus = "sent"
	NotificationFailed NotificationStatus = "failed"
)

// Role is what a principal is allowed to do. Permission is checked server-side
// in middleware; hiding buttons in the UI is convenience, not enforcement.
type Role string

const (
	RoleViewer   Role = "viewer"   // read-only: dashboard, history, run logs
	RoleOperator Role = "operator" // + tasks, connections, storages, run, sweep
	RoleAdmin    Role = "admin"    // + users, sessions, secrets, channels, settings
)

// roleRank orders roles so a permission check is a comparison rather than a set
// membership test. Unknown roles rank below viewer and so are allowed nothing.
var roleRank = map[Role]int{RoleViewer: 1, RoleOperator: 2, RoleAdmin: 3}

// Valid reports whether r is one of the three known roles.
func (r Role) Valid() bool { return roleRank[r] > 0 }

// AtLeast reports whether r carries at least the privileges of want.
func (r Role) AtLeast(want Role) bool {
	return roleRank[r] > 0 && roleRank[r] >= roleRank[want]
}

// User is a named login. PasswordHash carries its own argon2id parameters and
// salt, so they can be raised later without touching stored rows.
type User struct {
	ID           int64
	Email        string
	Name         string
	Role         Role
	PasswordHash string
	Disabled     bool
	CreatedAt    time.Time
	LastLoginAt  *time.Time
}

// Session is one logged-in client. The token itself is never stored — only
// TokenHash — so a copy of the database does not yield live sessions.
type Session struct {
	ID         int64
	UserID     int64
	TokenHash  string
	UserAgent  string
	IP         string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

// Setting is one instance-level key/value pair (instance name, defaults for new
// connections). Values are strings; callers parse what they wrote.
type Setting struct {
	Key       string
	Value     string
	UpdatedAt time.Time
}

// Secret is an encrypted value. Envelope encryption (see Q1 in the roadmap):
// Ciphertext is the value encrypted with a per-secret DEK, itself wrapped by the
// master key identified by KeyID — so master-key rotation only rewraps DEKs.
type Secret struct {
	ID         int64
	Name       string
	Type       string // "db-password" | "ssh-key" | "age-key" | "smtp-password"
	Ciphertext []byte
	KeyID      string // master key version that wrapped this secret's DEK
	CreatedAt  time.Time
	LastUsedAt *time.Time
}
