-- Watchdog: alert when a task has no recent SUCCESSFUL backup.
--
-- watchdog_sec is the staleness threshold in seconds:
--   0  → derive from the task's cron (see core.WatchdogThreshold)
--  >0  → explicit threshold
--  -1  → watchdog disabled for this task
-- Deriving by default matters: a threshold nobody configures is a watchdog
-- nobody has, and a silent backup gap is exactly what this is meant to catch.
ALTER TABLE task ADD COLUMN watchdog_sec INTEGER NOT NULL DEFAULT 0;

-- When the task last became enabled. The watchdog measures staleness from the
-- later of this and the last success: a task coming back from a pause has not
-- had a chance to run yet, and judging it by a success from before the pause
-- would alert the instant it is switched on.
ALTER TABLE task ADD COLUMN enabled_at INTEGER NOT NULL DEFAULT 0;
UPDATE task SET enabled_at = created_at;

-- One row per task currently in a "stale" episode, so the alert fires once when
-- the gap opens rather than on every tick, and survives a daemon restart. The
-- row is removed when a fresh success closes the episode.
CREATE TABLE watchdog_alert (
    task_id      INTEGER PRIMARY KEY REFERENCES task(id) ON DELETE CASCADE,
    alerted_at   INTEGER NOT NULL,
    last_success INTEGER            -- NULL when the task has never succeeded
);

-- The watchdog asks "when did each task last succeed?" every minute, and again
-- on every dashboard refresh. ix_run_task_created does not cover the status
-- filter, so without this the query scans the whole run table each time.
CREATE INDEX ix_run_success_task ON run(task_id, finished_at DESC) WHERE status = 'success';
