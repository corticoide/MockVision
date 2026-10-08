-- Feature 13, external plugins: the engine an installed plugin package
-- provides, its program unpacked under the data directory, the
-- permissions it asks for and its descriptor, as the program gave it when
-- it was installed (D85, D86). A plugin runs only once an admin enables
-- it, which approves its permissions; one version of an engine is enabled
-- at a time.

-- +goose Up

CREATE TABLE plugins (
  id               TEXT PRIMARY KEY,
  package_id       TEXT NOT NULL UNIQUE REFERENCES packages(id),
  engine           TEXT NOT NULL,
  engine_version   TEXT NOT NULL,
  executable       TEXT NOT NULL,
  dir              TEXT NOT NULL,
  permissions_json TEXT NOT NULL DEFAULT '[]',
  descriptor_json  TEXT NOT NULL,
  enabled          INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)),
  approved_by      TEXT NOT NULL DEFAULT '',
  approved_at      INTEGER,
  installed_at     INTEGER NOT NULL,
  UNIQUE (engine, engine_version)
) STRICT;

CREATE UNIQUE INDEX plugins_one_enabled ON plugins (engine) WHERE enabled = 1;

-- +goose Down

DROP INDEX plugins_one_enabled;
DROP TABLE plugins;
