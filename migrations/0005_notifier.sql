-- Per-channel notifier configuration.
--
-- Until now channels were constructed from the plugin registry with nil config,
-- so only zero-config channels (log) could be built: selecting telegram or
-- webhook on a task silently delivered nothing. A channel is now a configured
-- row, and task.notifiers references these rows by name.
CREATE TABLE notifier (
    id         INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE,           -- referenced by task.notifiers
    type       TEXT    NOT NULL,                  -- plugin: log | telegram | webhook
    config     TEXT    NOT NULL DEFAULT '{}',     -- opaque JSON; *_ref keys resolve from secrets
    events     TEXT    NOT NULL DEFAULT '[]',     -- JSON array of event kinds this channel forwards
    enabled    INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL
);

-- Existing tasks reference the built-in log channel by name; seeding it keeps
-- them delivering across the upgrade instead of silently losing their only
-- working channel.
INSERT INTO notifier (name, type, config, events, enabled, created_at)
VALUES ('log', 'log', '{}', '["success","failure","retention_error","watchdog"]', 1, unixepoch());

-- Delivery log: what was sent, where, and whether it landed. Without it there is
-- no way to answer "did my alert actually go out?" short of reading daemon logs.
CREATE TABLE notification (
    id         INTEGER PRIMARY KEY,
    kind       TEXT    NOT NULL,                  -- event kind
    task       TEXT    NOT NULL DEFAULT '',
    run_id     INTEGER,
    channel    TEXT    NOT NULL,                  -- notifier.name
    status     TEXT    NOT NULL,                  -- sent | failed
    error      TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE INDEX ix_notification_created ON notification(created_at DESC);
