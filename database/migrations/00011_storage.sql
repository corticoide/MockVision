-- Feature 10, storage: a camera keeps the recordings of its events on a
-- simulated SD card, a directory of the node with a quota that overwrites
-- the oldest when full (D68, D69), or on a NAS share for models without a
-- card. A camera without a row records nothing. recordings indexes what
-- every camera recorded, on its card or its share.

-- +goose Up

CREATE TABLE camera_storage (
  camera_id      TEXT PRIMARY KEY REFERENCES cameras (id) ON DELETE CASCADE,
  kind           TEXT NOT NULL CHECK (kind IN ('none', 'sd', 'nas')),
  size_mb        INTEGER NOT NULL DEFAULT 0,
  overwrite      INTEGER NOT NULL DEFAULT 1 CHECK (overwrite IN (0, 1)),
  nas_url        TEXT NOT NULL DEFAULT '',
  nas_username   TEXT NOT NULL DEFAULT '',
  nas_secret_enc BLOB
) STRICT;

CREATE TABLE recordings (
  id         TEXT PRIMARY KEY,
  camera_id  TEXT NOT NULL REFERENCES cameras (id) ON DELETE CASCADE,
  event_id   TEXT NOT NULL DEFAULT '',
  event_type TEXT NOT NULL,
  kind       TEXT NOT NULL CHECK (kind IN ('snapshot', 'clip')),
  stream     TEXT NOT NULL DEFAULT '',
  name       TEXT NOT NULL,
  size       INTEGER NOT NULL,
  start_at   INTEGER NOT NULL,
  end_at     INTEGER NOT NULL,
  location   TEXT NOT NULL CHECK (location IN ('sd', 'nas')),
  created_at INTEGER NOT NULL,
  UNIQUE (camera_id, location, name)
) STRICT;

CREATE INDEX recordings_camera_start ON recordings (camera_id, start_at);
CREATE INDEX recordings_event ON recordings (event_id);

-- +goose Down

DROP TABLE recordings;
DROP TABLE camera_storage;
