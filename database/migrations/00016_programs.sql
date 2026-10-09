-- Feature 18, capture programs: read-only capture recipes (D73), shipped
-- in the binary and installable as .mvpkg packages of kind 'program'.

-- +goose Up

ALTER TABLE devices ADD COLUMN port_map_json TEXT NOT NULL DEFAULT '{}';

CREATE TABLE programs (
  id              TEXT PRIMARY KEY,
  package_id      TEXT NOT NULL REFERENCES packages (id) ON DELETE CASCADE,
  program_id      TEXT NOT NULL,
  version         TEXT NOT NULL,
  name            TEXT NOT NULL,
  compatible_json TEXT NOT NULL DEFAULT '{}',
  steps_json      TEXT NOT NULL DEFAULT '[]',
  installed_at    INTEGER NOT NULL,
  UNIQUE (program_id, version)
) STRICT;

-- +goose Down

DROP TABLE programs;
ALTER TABLE devices DROP COLUMN port_map_json;
