-- Retention sweep history. One row per completed sweep (scheduled or manual),
-- written when the sweep finishes so a crash mid-sweep simply leaves no record
-- rather than a dangling "running" row that would need reaping.

CREATE TABLE retention_sweep (
    id            INTEGER PRIMARY KEY,
    started_at    INTEGER NOT NULL,
    finished_at   INTEGER NOT NULL,
    status        TEXT    NOT NULL,                      -- success | failed
    source        TEXT    NOT NULL DEFAULT 'schedule',   -- schedule | manual
    deleted_count INTEGER NOT NULL DEFAULT 0,
    freed_bytes   INTEGER NOT NULL DEFAULT 0,
    orphan_count  INTEGER NOT NULL DEFAULT 0,
    error         TEXT    NOT NULL DEFAULT ''
);

-- History listing, newest first.
CREATE INDEX ix_retention_sweep_started ON retention_sweep(started_at DESC);
