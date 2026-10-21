-- 0008: second factors — TOTP, WebAuthn passkeys, and one-time recovery codes.
--
-- A user has two-factor authentication on exactly when they hold a TOTP secret
-- or at least one passkey; there is no separate "enabled" flag to drift out of
-- step with the factors themselves. Recovery codes only exist alongside a
-- factor and are dropped together with the last one.

-- webauthn_id is the WebAuthn user handle: random, never the row id, because
-- INTEGER PRIMARY KEY may hand a deleted user's id to the next account, and an
-- authenticator still holding the old handle must not resolve to the new one.
ALTER TABLE app_user ADD COLUMN webauthn_id BLOB;
UPDATE app_user SET webauthn_id = randomblob(32);

-- secret is sealed under the master key (secret.Box): the shared TOTP secret
-- is as good as the second factor itself, so a copy of this file must not hand
-- it over. last_step is the newest 30-second step already accepted — a code is
-- valid once, not for its whole window.
CREATE TABLE user_totp (
    user_id    INTEGER PRIMARY KEY REFERENCES app_user(id) ON DELETE CASCADE,
    secret     BLOB    NOT NULL,
    key_id     TEXT    NOT NULL,
    last_step  INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);

-- data is the msgp encoding of webauthn.Credential (public key, sign counter,
-- flags). credential_id is broken out only to look a credential up and keep it
-- unique; everything the library needs to verify an assertion is in data.
CREATE TABLE webauthn_credential (
    id            INTEGER PRIMARY KEY,
    user_id       INTEGER NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
    credential_id BLOB    NOT NULL UNIQUE,
    name          TEXT    NOT NULL DEFAULT '',
    data          BLOB    NOT NULL,
    created_at    INTEGER NOT NULL,
    last_used_at  INTEGER
);

CREATE INDEX ix_webauthn_credential_user ON webauthn_credential(user_id);

-- code_hash is SHA-256 of the normalised code. Used codes are kept, not
-- deleted, so "N left" is a count rather than a guess.
CREATE TABLE recovery_code (
    id        INTEGER PRIMARY KEY,
    user_id   INTEGER NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
    code_hash TEXT    NOT NULL,
    used_at   INTEGER
);

CREATE INDEX ix_recovery_code_user ON recovery_code(user_id);
