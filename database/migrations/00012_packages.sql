-- Feature 12, packages and catalog: the keys an admin trusts besides the
-- official ones built into the binary (D83), where a package came from
-- (uploaded, the official catalog shipped in the binary, or a duplicate of
-- a profile, D19), and the parent a profile extends, pinned to the version
-- it was resolved with (D17).

-- +goose Up

CREATE TABLE trusted_keys (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  key_id     TEXT NOT NULL UNIQUE,
  public_key TEXT NOT NULL,
  added_at   INTEGER NOT NULL
) STRICT;

ALTER TABLE packages ADD COLUMN source TEXT NOT NULL DEFAULT 'upload' CHECK (source IN ('upload', 'catalog', 'duplicate'));
ALTER TABLE profiles ADD COLUMN extends_ref TEXT NOT NULL DEFAULT '';

-- +goose Down

ALTER TABLE profiles DROP COLUMN extends_ref;
ALTER TABLE packages DROP COLUMN source;
DROP TABLE trusted_keys;
