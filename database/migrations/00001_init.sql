-- MockVision initial schema: the tables used by the technical demo.
-- IDs are ULIDs as text, times are Unix milliseconds in UTC, *_json columns
-- are validated by the application and *_enc columns are encrypted.

-- +goose Up

CREATE TABLE users (
  id            TEXT PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE COLLATE NOCASE,
  password_hash TEXT NOT NULL,
  role          TEXT NOT NULL CHECK (role IN ('admin')),
  disabled      INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL
) STRICT;

CREATE TABLE sessions (
  id         TEXT PRIMARY KEY, -- sha256 of the session token, hex
  user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  expires_at INTEGER NOT NULL,
  ip         TEXT NOT NULL DEFAULT '',
  user_agent TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
) STRICT;

CREATE INDEX sessions_user_id ON sessions (user_id);

CREATE TABLE audit_log (
  id          TEXT PRIMARY KEY,
  at          INTEGER NOT NULL,
  actor_type  TEXT NOT NULL, -- user | camera | system
  actor_id    TEXT NOT NULL DEFAULT '',
  action      TEXT NOT NULL,
  entity_type TEXT NOT NULL,
  entity_id   TEXT NOT NULL DEFAULT '',
  origin_ip   TEXT NOT NULL DEFAULT '',
  diff_json   TEXT NOT NULL DEFAULT '{}'
) STRICT;

CREATE INDEX audit_log_at ON audit_log (at);

CREATE TABLE packages (
  id               TEXT PRIMARY KEY,
  kind             TEXT NOT NULL CHECK (kind IN ('profile', 'plugin', 'program')),
  pkg_id           TEXT NOT NULL,
  version          TEXT NOT NULL,
  sha256           TEXT NOT NULL,
  signature_status TEXT NOT NULL CHECK (signature_status IN ('official', 'trusted', 'unsigned', 'invalid')),
  signer           TEXT NOT NULL DEFAULT '',
  manifest_json    TEXT NOT NULL,
  report_json      TEXT NOT NULL DEFAULT '{}',
  enabled          INTEGER NOT NULL DEFAULT 1,
  installed_at     INTEGER NOT NULL,
  UNIQUE (kind, pkg_id, version)
) STRICT;

CREATE TABLE profiles (
  id            TEXT PRIMARY KEY,
  package_id    TEXT NOT NULL REFERENCES packages (id),
  profile_id    TEXT NOT NULL,
  version       TEXT NOT NULL,
  name          TEXT NOT NULL,
  vendor        TEXT NOT NULL,
  model         TEXT NOT NULL,
  firmware_json TEXT NOT NULL DEFAULT '[]',
  resolved_json TEXT NOT NULL,
  level         TEXT NOT NULL CHECK (level IN ('draft', 'documented', 'captured', 'verified')),
  coverage_json TEXT NOT NULL DEFAULT '{}',
  archived      INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  UNIQUE (profile_id, version)
) STRICT;

CREATE TABLE cameras (
  id              TEXT PRIMARY KEY,
  name            TEXT NOT NULL UNIQUE COLLATE NOCASE,
  profile_id      TEXT NOT NULL,
  profile_version TEXT NOT NULL,
  serial          TEXT NOT NULL,
  desired_state   TEXT NOT NULL CHECK (desired_state IN ('running', 'stopped')),
  autostart       INTEGER NOT NULL DEFAULT 1,
  tags_json       TEXT NOT NULL DEFAULT '[]',
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  FOREIGN KEY (profile_id, profile_version) REFERENCES profiles (profile_id, version)
) STRICT;

CREATE TABLE camera_network (
  camera_id TEXT PRIMARY KEY REFERENCES cameras (id) ON DELETE CASCADE,
  mode      TEXT NOT NULL CHECK (mode IN ('macvlan', 'ipvlan')),
  parent_if TEXT NOT NULL DEFAULT '',
  mac       TEXT NOT NULL UNIQUE,
  ip_mode   TEXT NOT NULL CHECK (ip_mode IN ('static', 'dhcp')),
  ip        TEXT NOT NULL DEFAULT '',
  netmask   TEXT NOT NULL DEFAULT '',
  gateway   TEXT NOT NULL DEFAULT '',
  dns_json  TEXT NOT NULL DEFAULT '[]'
) STRICT;

CREATE UNIQUE INDEX camera_network_ip ON camera_network (ip) WHERE ip <> '';

CREATE TABLE camera_state (
  camera_id  TEXT NOT NULL REFERENCES cameras (id) ON DELETE CASCADE,
  key        TEXT NOT NULL,
  value_json TEXT NOT NULL,
  origin     TEXT NOT NULL, -- profile | panel | client:<ip> | system
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (camera_id, key)
) STRICT;

CREATE TABLE camera_protocols (
  camera_id    TEXT NOT NULL REFERENCES cameras (id) ON DELETE CASCADE,
  engine_key   TEXT NOT NULL,
  enabled      INTEGER NOT NULL DEFAULT 1,
  port         INTEGER NOT NULL,
  options_json TEXT NOT NULL DEFAULT '{}',
  PRIMARY KEY (camera_id, engine_key)
) STRICT;

CREATE TABLE camera_users (
  id           TEXT PRIMARY KEY,
  camera_id    TEXT NOT NULL REFERENCES cameras (id) ON DELETE CASCADE,
  username     TEXT NOT NULL,
  password_enc BLOB NOT NULL,
  role         TEXT NOT NULL,
  UNIQUE (camera_id, username)
) STRICT;

CREATE TABLE camera_status (
  camera_id      TEXT PRIMARY KEY REFERENCES cameras (id) ON DELETE CASCADE,
  actual_state   TEXT NOT NULL,
  reason         TEXT NOT NULL DEFAULT '',
  started_at     INTEGER,
  last_heartbeat INTEGER,
  updated_at     INTEGER NOT NULL
) STRICT;

CREATE TABLE assets (
  id         TEXT PRIMARY KEY,
  sha256     TEXT NOT NULL UNIQUE,
  kind       TEXT NOT NULL CHECK (kind IN ('image')),
  mime       TEXT NOT NULL,
  width      INTEGER NOT NULL,
  height     INTEGER NOT NULL,
  size       INTEGER NOT NULL,
  filename   TEXT NOT NULL,
  builtin    INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
) STRICT;

CREATE TABLE renditions (
  id         TEXT PRIMARY KEY,
  asset_id   TEXT NOT NULL REFERENCES assets (id) ON DELETE CASCADE,
  codec      TEXT NOT NULL,
  width      INTEGER NOT NULL,
  height     INTEGER NOT NULL,
  fps        INTEGER NOT NULL,
  gop        INTEGER NOT NULL,
  bitrate    INTEGER NOT NULL,
  sha256     TEXT NOT NULL DEFAULT '',
  status     TEXT NOT NULL CHECK (status IN ('pending', 'ready', 'failed')),
  error      TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  UNIQUE (asset_id, codec, width, height, fps, gop, bitrate)
) STRICT;

CREATE TABLE camera_streams (
  camera_id    TEXT NOT NULL REFERENCES cameras (id) ON DELETE CASCADE,
  stream       TEXT NOT NULL,
  asset_id     TEXT NOT NULL REFERENCES assets (id),
  rendition_id TEXT REFERENCES renditions (id) ON DELETE SET NULL,
  PRIMARY KEY (camera_id, stream)
) STRICT;

CREATE TABLE targets (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE COLLATE NOCASE,
  type        TEXT NOT NULL CHECK (type IN ('http')),
  config_json TEXT NOT NULL,
  secret_enc  BLOB,
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  INTEGER NOT NULL
) STRICT;

CREATE TABLE camera_targets (
  camera_id        TEXT NOT NULL REFERENCES cameras (id) ON DELETE CASCADE,
  target_id        TEXT NOT NULL REFERENCES targets (id),
  event_types_json TEXT NOT NULL DEFAULT '[]',
  overrides_json   TEXT NOT NULL DEFAULT '{}',
  PRIMARY KEY (camera_id, target_id)
) STRICT;

CREATE TABLE events (
  id         TEXT PRIMARY KEY,
  camera_id  TEXT NOT NULL REFERENCES cameras (id) ON DELETE CASCADE,
  type       TEXT NOT NULL,
  at         INTEGER NOT NULL,
  data_json  TEXT NOT NULL,
  rule_id    TEXT,
  trigger_id TEXT
) STRICT;

CREATE INDEX events_at ON events (at);
CREATE INDEX events_camera_id_at ON events (camera_id, at);

CREATE TABLE deliveries (
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

CREATE INDEX deliveries_event_id ON deliveries (event_id);
CREATE INDEX deliveries_at ON deliveries (at);

CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value_json TEXT NOT NULL
) STRICT;

-- +goose Down

DROP TABLE settings;
DROP TABLE deliveries;
DROP TABLE events;
DROP TABLE camera_targets;
DROP TABLE targets;
DROP TABLE camera_streams;
DROP TABLE renditions;
DROP TABLE assets;
DROP TABLE camera_status;
DROP TABLE camera_users;
DROP TABLE camera_protocols;
DROP TABLE camera_state;
DROP TABLE camera_network;
DROP TABLE cameras;
DROP TABLE profiles;
DROP TABLE packages;
DROP TABLE audit_log;
DROP TABLE sessions;
DROP TABLE users;
