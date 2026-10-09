-- Feature 20: a profile compiled from a capture is installed with source
-- 'capture'. Widen the packages.source check to allow it; SQLite needs a
-- table rebuild to change a CHECK, done with foreign keys off so the
-- children (profiles, plugins, programs) keep their rows.

-- +goose NO TRANSACTION
-- +goose Up
PRAGMA foreign_keys=OFF;

-- +goose StatementBegin
CREATE TABLE packages_new (
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
  source           TEXT NOT NULL DEFAULT 'upload' CHECK (source IN ('upload', 'catalog', 'duplicate', 'capture')),
  UNIQUE (kind, pkg_id, version)
) STRICT;
-- +goose StatementEnd

INSERT INTO packages_new SELECT id, kind, pkg_id, version, sha256, signature_status, signer, manifest_json, report_json, enabled, installed_at, source FROM packages;
DROP TABLE packages;
ALTER TABLE packages_new RENAME TO packages;

PRAGMA foreign_keys=ON;

-- +goose Down
PRAGMA foreign_keys=OFF;

-- +goose StatementBegin
CREATE TABLE packages_old (
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
  source           TEXT NOT NULL DEFAULT 'upload' CHECK (source IN ('upload', 'catalog', 'duplicate')),
  UNIQUE (kind, pkg_id, version)
) STRICT;
-- +goose StatementEnd

INSERT INTO packages_old SELECT id, kind, pkg_id, version, sha256, signature_status, signer, manifest_json, report_json, enabled, installed_at, source FROM packages WHERE source <> 'capture';
DROP TABLE packages;
ALTER TABLE packages_old RENAME TO packages;

PRAGMA foreign_keys=ON;
