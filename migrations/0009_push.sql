-- 0009: browser push notifications (Web Push).
--
-- A browser subscribes once per profile and the subscription belongs to the
-- user who switched it on. Which events reach which users is decided by a
-- `push` notifier channel; these tables only say where to deliver.

-- The VAPID key pair signs every push request, so there is exactly one per
-- instance. private_key is sealed under the master key (secret.Box), like
-- user_totp.secret: a copy of this file must not let anyone push in the
-- server's name. It is kept out of the secret table so it never shows up on
-- the secrets page, where it could be deleted by mistake.
CREATE TABLE push_vapid (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    public_key  TEXT    NOT NULL,
    private_key BLOB    NOT NULL,
    key_id      TEXT    NOT NULL,
    created_at  INTEGER NOT NULL
);

-- endpoint is unique: one browser profile holds one subscription. When another
-- user signs in to the same browser and turns push on, the row moves to them
-- rather than leaving the previous owner's alerts arriving there.
CREATE TABLE push_subscription (
    id              INTEGER PRIMARY KEY,
    user_id         INTEGER NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
    endpoint        TEXT    NOT NULL UNIQUE,
    p256dh          TEXT    NOT NULL,
    auth            TEXT    NOT NULL,
    user_agent      TEXT    NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    last_success_at INTEGER,
    last_error      TEXT    NOT NULL DEFAULT '',
    fail_count      INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX ix_push_subscription_user ON push_subscription(user_id);
