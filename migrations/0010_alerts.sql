-- 0010: more notification events, and the state that keeps them from repeating.
--
-- alert_state holds the alert "episodes" that are open right now: a task whose
-- last run failed terminally, a storage short of room or not answering. An
-- alert fires when its episode opens (or, for a level, gets worse), and the
-- "back to normal" event when it closes. Persisting it means a restart neither
-- re-sends an open alert nor forgets to report the recovery.
CREATE TABLE alert_state (
    kind      TEXT    NOT NULL,              -- failure | storage_low | storage_unreachable
    subject   TEXT    NOT NULL,              -- task:<id> | storage:<id>
    level     INTEGER NOT NULL DEFAULT 1,    -- storage_low: 1 = warn, 2 = critical
    opened_at INTEGER NOT NULL,
    PRIMARY KEY (kind, subject)
);

-- known_device remembers where each user has signed in from, so a login from a
-- browser or network not seen before can be reported. fingerprint is SHA-256 of
-- browser family, OS and IP prefix; label is the readable form of the same.
CREATE TABLE known_device (
    user_id     INTEGER NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
    fingerprint TEXT    NOT NULL,
    label       TEXT    NOT NULL DEFAULT '',
    first_seen  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL,
    PRIMARY KEY (user_id, fingerprint)
);

-- A channel that already reports failures wants the new alerts that mean the
-- same thing — a backup lost or about to be. Security and the routine sweep
-- report are left for the operator to switch on: where security events belong
-- depends on who reads the channel.
UPDATE notifier
   SET events = (SELECT json_group_array(value) FROM (
                    SELECT value FROM json_each(notifier.events)
                    UNION ALL
                    SELECT n.value FROM json_each('["recovered","size_anomaly","slow_run","interrupted","storage_low","storage_unreachable"]') n
                     WHERE n.value NOT IN (SELECT value FROM json_each(notifier.events))))
 WHERE EXISTS (SELECT 1 FROM json_each(notifier.events) WHERE value = 'failure');

-- The built-in log channel records everything.
UPDATE notifier
   SET events = '["success","failure","recovered","size_anomaly","slow_run","interrupted","retention_done","retention_error","watchdog","storage_low","storage_unreachable","security"]'
 WHERE name = 'log' AND type = 'log';
