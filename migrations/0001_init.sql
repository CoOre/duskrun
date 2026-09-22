-- Duskrun metadata schema (TZ §6). Applied by internal/store/sqlite.Migrate.
-- Engine assumptions: WAL journal, foreign keys on, busy_timeout set per-conn
-- via DSN (see store/sqlite/db.go). All timestamps are unix seconds (INTEGER)
-- for driver-agnostic handling under modernc.org/sqlite.

CREATE TABLE secret (
    id          INTEGER PRIMARY KEY,
    name        TEXT    NOT NULL UNIQUE,
    type        TEXT    NOT NULL,
    ciphertext  BLOB    NOT NULL,           -- value encrypted by a per-secret DEK
    key_id      TEXT    NOT NULL,           -- master key version wrapping the DEK
    created_at  INTEGER NOT NULL,
    last_used_at INTEGER
);

CREATE TABLE connection (
    id               INTEGER PRIMARY KEY,
    name             TEXT    NOT NULL UNIQUE,
    engine           TEXT    NOT NULL,      -- postgres | mysql | ...
    connector_type   TEXT    NOT NULL,      -- direct | socket | ssh-tunnel
    connector_config TEXT    NOT NULL DEFAULT '{}',  -- opaque JSON
    secret_ref       TEXT    NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL
);

CREATE TABLE storage (
    id         INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE,
    type       TEXT    NOT NULL,            -- localfs | s3 | sftp
    config     TEXT    NOT NULL DEFAULT '{}',
    secret_ref TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE TABLE task (
    id            INTEGER PRIMARY KEY,
    name          TEXT    NOT NULL UNIQUE,
    connection_id INTEGER NOT NULL REFERENCES connection(id) ON DELETE RESTRICT,
    dumper_opts   TEXT    NOT NULL DEFAULT '{}',
    codec_chain   TEXT    NOT NULL DEFAULT '[]',   -- JSON array, ordered
    storage_id    INTEGER NOT NULL REFERENCES storage(id) ON DELETE RESTRICT,
    cron          TEXT    NOT NULL,
    retention     TEXT    NOT NULL DEFAULT '{}',    -- JSON: {keep_last, gfs}
    notifiers     TEXT    NOT NULL DEFAULT '[]',    -- JSON array of channel names
    enabled       INTEGER NOT NULL DEFAULT 1,
    misfire       TEXT    NOT NULL DEFAULT 'run_once_now',
    retries       INTEGER NOT NULL DEFAULT 0,
    timeout_sec   INTEGER NOT NULL DEFAULT 1800,
    created_at    INTEGER NOT NULL
);

CREATE TABLE run (
    id          INTEGER PRIMARY KEY,
    task_id     INTEGER NOT NULL REFERENCES task(id) ON DELETE CASCADE,
    status      TEXT    NOT NULL,           -- queued|running|success|failed|skipped
    worker      TEXT    NOT NULL DEFAULT '',
    attempt     INTEGER NOT NULL DEFAULT 1,
    started_at  INTEGER,
    finished_at INTEGER,
    error       TEXT    NOT NULL DEFAULT '',
    log         TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL
);

-- Core invariant (TZ §7): at most ONE active run per task. A partial unique
-- index makes "two runs of one task at once" impossible at the storage layer —
-- the enqueue/claim logic cannot violate it even under a race.
CREATE UNIQUE INDEX ux_run_active ON run(task_id) WHERE status IN ('queued', 'running');

-- Claim scan: fetch the oldest queued run cheaply.
CREATE INDEX ix_run_queued ON run(id) WHERE status = 'queued';

-- History listing per task, newest first.
CREATE INDEX ix_run_task_created ON run(task_id, created_at DESC);

CREATE TABLE artifact (
    id           INTEGER PRIMARY KEY,
    run_id       INTEGER NOT NULL REFERENCES run(id) ON DELETE CASCADE,
    storage_id   INTEGER NOT NULL REFERENCES storage(id) ON DELETE RESTRICT,
    key          TEXT    NOT NULL,
    size         INTEGER NOT NULL,
    checksum     TEXT    NOT NULL,          -- hex SHA-256
    created_at   INTEGER NOT NULL,
    expires_hint INTEGER
);

CREATE INDEX ix_artifact_run ON artifact(run_id);
CREATE INDEX ix_artifact_storage_key ON artifact(storage_id, key);
