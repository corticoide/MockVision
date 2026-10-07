-- Feature 9, faults: failures injected into a camera (D43). Every fault
-- ends when it expires or by hand (RN-14): expires_at NULL lasts until
-- ended. Ended faults stay a while as history. The kind is checked by the
-- application, so new kinds need no migration.

-- +goose Up

CREATE TABLE faults (
  id          TEXT PRIMARY KEY,
  camera_id   TEXT NOT NULL REFERENCES cameras (id) ON DELETE CASCADE,
  kind        TEXT NOT NULL,
  params_json TEXT NOT NULL DEFAULT '{}',
  started_at  INTEGER NOT NULL,
  expires_at  INTEGER,
  ended_at    INTEGER,
  ended_by    TEXT NOT NULL DEFAULT '',
  created_by  TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX faults_camera_id ON faults (camera_id, started_at);
CREATE INDEX faults_open ON faults (expires_at) WHERE ended_at IS NULL;

-- +goose Down

DROP TABLE faults;
