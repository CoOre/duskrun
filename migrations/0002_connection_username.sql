-- The login username belongs with the connection: host/port/user together
-- identify the server login, and only the password is a secret. Older tasks
-- kept the username in dumper_opts; that still works and overrides this column
-- when set (see dumper buildArgs fallbackUser).
ALTER TABLE connection ADD COLUMN username TEXT NOT NULL DEFAULT '';
