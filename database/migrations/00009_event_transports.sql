-- Feature 8, event transports: besides HTTP, a target may be an MQTT
-- broker, an FTP or SFTP server or a mail server (D31), and a delivery may
-- be skipped on purpose, as a mail within the camera's interval. SQLite
-- cannot change a CHECK constraint, so both tables are rebuilt with
-- foreign keys off, as its documentation prescribes.

-- +goose NO TRANSACTION

-- +goose Up

-- One statement, so the pragmas apply to the connection that rebuilds.
-- +goose StatementBegin
PRAGMA foreign_keys = OFF;
BEGIN;

CREATE TABLE targets_new (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE COLLATE NOCASE,
  type        TEXT NOT NULL CHECK (type IN ('http', 'mqtt', 'ftp', 'sftp', 'smtp')),
  config_json TEXT NOT NULL,
  secret_enc  BLOB,
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  INTEGER NOT NULL
) STRICT;
INSERT INTO targets_new (id, name, type, config_json, secret_enc, enabled, created_at)
  SELECT id, name, type, config_json, secret_enc, enabled, created_at FROM targets;
DROP TABLE targets;
ALTER TABLE targets_new RENAME TO targets;

CREATE TABLE deliveries_new (
  id          TEXT PRIMARY KEY,
  event_id    TEXT NOT NULL REFERENCES events (id) ON DELETE CASCADE,
  target_id   TEXT NOT NULL,
  attempt     INTEGER NOT NULL,
  at          INTEGER NOT NULL,
  status      TEXT NOT NULL CHECK (status IN ('ok', 'retry', 'failed', 'skipped')),
  http_status INTEGER,
  latency_ms  INTEGER,
  error       TEXT NOT NULL DEFAULT ''
) STRICT;
INSERT INTO deliveries_new (id, event_id, target_id, attempt, at, status, http_status, latency_ms, error)
  SELECT id, event_id, target_id, attempt, at, status, http_status, latency_ms, error FROM deliveries;
DROP TABLE deliveries;
ALTER TABLE deliveries_new RENAME TO deliveries;
CREATE INDEX deliveries_event_id ON deliveries (event_id);
CREATE INDEX deliveries_at ON deliveries (at);

COMMIT;
PRAGMA foreign_keys = ON;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
PRAGMA foreign_keys = OFF;
BEGIN;

DELETE FROM deliveries WHERE status = 'skipped';
CREATE TABLE deliveries_old (
  id          TEXT PRIMARY KEY,
  event_id    TEXT NOT NULL REFERENCES events (id) ON DELETE CASCADE,
  target_id   TEXT NOT NULL,
  attempt     INTEGER NOT NULL,
  at          INTEGER NOT NULL,
  status      TEXT NOT NULL CHECK (status IN ('ok', 'retry', 'failed')),
  http_status INTEGER,
  latency_ms  INTEGER,
  error       TEXT NOT NULL DEFAULT ''
) STRICT;
INSERT INTO deliveries_old SELECT * FROM deliveries;
DROP TABLE deliveries;
ALTER TABLE deliveries_old RENAME TO deliveries;
CREATE INDEX deliveries_event_id ON deliveries (event_id);
CREATE INDEX deliveries_at ON deliveries (at);

DELETE FROM camera_targets WHERE target_id IN (SELECT id FROM targets WHERE type <> 'http');
DELETE FROM targets WHERE type <> 'http';
CREATE TABLE targets_old (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE COLLATE NOCASE,
  type        TEXT NOT NULL CHECK (type IN ('http')),
  config_json TEXT NOT NULL,
  secret_enc  BLOB,
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  INTEGER NOT NULL
) STRICT;
INSERT INTO targets_old SELECT * FROM targets;
DROP TABLE targets;
ALTER TABLE targets_old RENAME TO targets;

COMMIT;
PRAGMA foreign_keys = ON;
-- +goose StatementEnd
