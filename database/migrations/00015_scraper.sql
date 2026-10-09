-- Feature 16 to 20, the scraper: real or simulated devices the user
-- registers to capture, with their credentials encrypted (RN-18), and the
-- captures a program runs against one, read only (RN-17), compiled into a
-- draft profile (D44, D72 to D77).

-- +goose Up

CREATE TABLE devices (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL,
  host          TEXT NOT NULL,
  ports_json    TEXT NOT NULL DEFAULT '[]',
  username      TEXT NOT NULL DEFAULT '',
  secret_enc    BLOB,
  -- kind is 'real' or 'simulated'; a device whose host is one of this
  -- node's cameras is simulated (D75).
  kind          TEXT NOT NULL DEFAULT 'real' CHECK (kind IN ('real', 'simulated')),
  -- authorized records that the user owns or may capture this device; no
  -- probe runs until it is set (RN-17).
  authorized    INTEGER NOT NULL DEFAULT 0 CHECK (authorized IN (0, 1)),
  detected_json TEXT NOT NULL DEFAULT '{}',
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
) STRICT;

CREATE TABLE captures (
  id               TEXT PRIMARY KEY,
  device_id        TEXT NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
  job_id           TEXT NOT NULL DEFAULT '',
  program_ref      TEXT NOT NULL,
  status           TEXT NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'done', 'failed')),
  -- The recordings and the sanitized report of the capture, as JSON kept
  -- in the database; the raw artifacts (pcap, HAR) never go in a profile.
  artifacts_json   TEXT NOT NULL DEFAULT '{}',
  draft_profile_id TEXT REFERENCES profiles (id) ON DELETE SET NULL,
  created_at       INTEGER NOT NULL,
  finished_at      INTEGER
) STRICT;

CREATE INDEX captures_by_device ON captures (device_id, created_at DESC);

-- +goose Down

DROP TABLE captures;
DROP TABLE devices;
