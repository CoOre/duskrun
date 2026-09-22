// Types mirror the Go DTOs in internal/api/handlers_read.go and
// handlers_write.go. Keep them in sync with the backend.

export type RunStatus = 'queued' | 'running' | 'success' | 'failed' | 'skipped'

// Live run progress (SSE) — mirrors core.Phase / core.ProgressEvent in Go.
export type RunPhase = 'queued' | 'resolve' | 'stream' | 'record'

export type RunEventKind = 'log' | 'phase' | 'bytes' | 'total' | 'status'

export interface RunProgressEvent {
  run_id: number
  seq: number
  kind: RunEventKind
  phase?: RunPhase
  message?: string
  bytes?: number
  total?: number
  throughput_bps?: number
  status?: RunStatus
  at: string
}

export interface Task {
  id: number
  name: string
  connection_id: number
  storage_id: number
  cron: string
  dumper_opts?: unknown
  codec_chain: string[]
  retention?: Retention
  notifiers: string[]
  enabled: boolean
  retries?: number
  timeout_sec?: number
  // Watchdog staleness threshold in seconds: 0 = derived from cron, -1 = off.
  watchdog_sec?: number
  // Next cron activation (UTC, ISO-8601), computed by the backend. Absent for a
  // paused task or an unparsable cron — never parse `cron` in the UI instead.
  next_run?: string
}

export interface Run {
  id: number
  task_id: number
  status: RunStatus
  attempt: number
  worker?: string
  error?: string
  log?: string
  started_at?: string
  finished_at?: string
  created_at: string
  artifact?: Artifact
}

export interface Artifact {
  id: number
  run_id: number
  storage_id: number
  key: string
  size: number
  checksum: string
  created_at: string
}

export interface Connection {
  id: number
  name: string
  engine: string
  connector_type: string
  connector_config?: unknown
  username: string
  secret_ref: string
}

export interface Storage {
  id: number
  name: string
  type: string
  config?: unknown
  secret_ref: string
}

export interface Secret {
  id: number
  name: string
  type: string
  key_id: string
  created_at: string
  last_used_at?: string
}

export interface DatabaseList {
  databases: string[]
}

// WatchdogAlert is a task whose last successful backup is older than its
// threshold — see handlers_watchdog.go. threshold_sec is already resolved, so
// the UI never re-derives it from cron.
export interface WatchdogAlert {
  task_id: number
  task: string
  threshold_sec: number
  since_sec: number
  last_success?: string
  reason: string
}

export type EventKind = 'success' | 'failure' | 'retention_error' | 'watchdog'

// NotifierChannel is a configured delivery channel — see handlers_notifier.go.
// `events` is the set of kinds it forwards; empty means it delivers nothing.
export interface NotifierChannel {
  id: number
  name: string
  type: string
  config?: unknown
  events: EventKind[]
  enabled: boolean
  created_at: string
  used_by: string[]
}

export interface CreateNotifierReq {
  name: string
  type: string
  config?: unknown
  events?: EventKind[]
  enabled?: boolean
}

// Notification is one delivery attempt.
export interface Notification {
  id: number
  kind: EventKind
  task?: string
  run_id?: number
  channel: string
  status: 'sent' | 'failed'
  error?: string
  created_at: string
}

export interface NotifierTestResult {
  status: 'ok' | 'failed'
  error?: string
}

export type SweepStatus = 'success' | 'failed'
export type SweepSource = 'schedule' | 'manual'

// Sweep is one completed pass of the Retention Manager — see handlers_retention.go.
export interface Sweep {
  id: number
  started_at: string
  finished_at: string
  status: SweepStatus
  source: SweepSource
  deleted_count: number
  freed_bytes: number
  orphan_count: number
  error?: string
}

// RetentionTask is one row of the per-task policy table: the policy plus the
// task's current footprint in the catalog.
export interface RetentionTask {
  task_id: number
  name: string
  enabled: boolean
  retention: Retention
  artifacts: number
  bytes: number
}

// RetentionSummary backs the retention page. An empty `cron` means the
// scheduled sweep is disabled and retention only runs on demand.
export interface RetentionSummary {
  cron: string
  next_sweep?: string
  last_sweep?: Sweep
  tasks: RetentionTask[]
}

export type ConnectionTestStatus = 'ok' | 'failed'

// A single check can also be 'skipped': the engine has no probe for that stage
// (mongodb/redis have no database list), so it was never attempted.
export type ConnectionCheckStatus = ConnectionTestStatus | 'skipped'

export interface ConnectionTestCheck {
  key: string
  status: ConnectionCheckStatus
  label: string
}

export interface ConnectionTestError {
  code: string
  message: string
  hint: string
}

export interface ConnectionTestResult {
  status: ConnectionTestStatus
  latency_ms: number
  checks: ConnectionTestCheck[]
  databases?: string[]
  error?: ConnectionTestError
}

// Create payloads (POST bodies) — see handlers_write.go.
export interface Retention {
  keep_last?: number
  gfs?: GFS
}

// GFS is grandfather-father-son retention: keep the freshest artifact in each of
// the last N days / ISO weeks / months. The three counts are unioned with each
// other and with keep_last — an artifact any rule keeps is kept.
export interface GFS {
  daily: number
  weekly: number
  monthly: number
}

export interface CreateConnectionReq {
  name: string
  engine: string
  connector_type: string
  connector_config?: unknown
  username?: string
  secret_ref?: string
}

export interface CreateStorageReq {
  name: string
  type: string
  config?: unknown
  secret_ref?: string
}

export interface CreateSecretReq {
  name: string
  type: string
  value: string
}

export interface CreateTaskReq {
  name: string
  connection_id: number
  storage_id: number
  dumper_opts?: unknown
  codec_chain?: string[]
  cron: string
  retention?: Retention
  notifiers?: string[]
  enabled?: boolean
  retries?: number
  timeout_sec?: number
  watchdog_sec?: number
}

// --- users, sessions, settings -------------------------------------------

export type Role = 'admin' | 'operator' | 'viewer'

export interface User {
  id: number
  email: string
  name: string
  role: Role
  disabled: boolean
  created_at: string
  last_login_at?: string
}

// Me is the identity behind the current credential. `static: true` means the
// caller came in on DUSKRUN_API_TOKEN — a machine account with no user row, and
// the sidebar says so instead of inventing a person.
export interface Me {
  static: boolean
  role: Role
  email?: string
  name?: string
  user_id?: number
  session_id?: number
  expires_at?: string
}

export interface LoginResp {
  token: string
  expires_at: string
  user: User
}

export interface Session {
  id: number
  user_id: number
  email?: string
  user_agent: string
  ip: string
  created_at: string
  last_seen_at: string
  expires_at: string
  current: boolean
}

// InstanceInfo is read-only: these come from environment variables and a
// restart, so the settings page reports them rather than offering to edit.
export interface InstanceInfo {
  version: string
  db_path: string
  workers: number
  retention_cron: string
  listen_addr: string
  timezone: string
  timezone_offset: string
  now: string
}

export interface Settings {
  instance_name: string
  ssh_host_key_mode_default: 'tofu' | 'strict'
  instance: InstanceInfo
}

export interface CreateUserReq {
  email: string
  name?: string
  role: Role
  password: string
}

export interface UpdateUserReq {
  name?: string
  role?: Role
  disabled?: boolean
  password?: string
}
