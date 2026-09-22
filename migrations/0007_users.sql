-- 0007: named users with roles, login sessions, and instance settings.
--
-- Until now authentication was a single shared bearer token (DUSKRUN_API_TOKEN)
-- with no notion of who was acting. That token keeps working — it is the only
-- way into a running instance before the first user exists — but it becomes one
-- of two paths rather than the only one.

CREATE TABLE app_user (
    id            INTEGER PRIMARY KEY,
    email         TEXT    NOT NULL UNIQUE,   -- the login
    name          TEXT    NOT NULL DEFAULT '',
    role          TEXT    NOT NULL,          -- admin | operator | viewer
    password_hash TEXT    NOT NULL,          -- argon2id, params and salt encoded in the string
    disabled      INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    last_login_at INTEGER
);

-- token_hash holds SHA-256 of the session token, never the token itself: a dump
-- of this file must not hand over live sessions.
CREATE TABLE session (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
    token_hash   TEXT    NOT NULL UNIQUE,
    user_agent   TEXT    NOT NULL DEFAULT '',
    ip           TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL
);

CREATE INDEX ix_session_user ON session(user_id);

CREATE TABLE setting (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);
