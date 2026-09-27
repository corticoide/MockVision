-- API tokens for automation (D54) and the origin of every audit entry:
-- the panel, the API with a token, a client of a camera's emulated API or
-- the node itself (RN-08).

-- +goose Up

CREATE TABLE api_tokens (
  id           TEXT PRIMARY KEY,
  user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  token_hash   TEXT NOT NULL UNIQUE, -- sha256 of the token, hex
  prefix       TEXT NOT NULL,        -- first characters, to recognize it
  scopes_json  TEXT NOT NULL DEFAULT '[]',
  expires_at   INTEGER,
  last_used_at INTEGER,
  last_used_ip TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  UNIQUE (user_id, name)
) STRICT;

ALTER TABLE audit_log ADD COLUMN origin TEXT NOT NULL DEFAULT ''; -- panel | api | camera | system
ALTER TABLE audit_log ADD COLUMN actor_name TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN token_id TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN token_name TEXT NOT NULL DEFAULT '';

-- Entries written before tokens existed came from the panel.
UPDATE audit_log SET origin = CASE actor_type WHEN 'camera' THEN 'camera' WHEN 'system' THEN 'system' ELSE 'panel' END;
UPDATE audit_log SET actor_name = coalesce((SELECT username FROM users WHERE users.id = audit_log.actor_id), actor_id)
WHERE actor_type = 'user';
-- Failed logins stored the typed username as the actor ID.
UPDATE audit_log SET actor_id = ''
WHERE actor_type = 'user' AND NOT EXISTS (SELECT 1 FROM users WHERE users.id = audit_log.actor_id);

CREATE INDEX audit_log_entity ON audit_log (entity_type, entity_id);

-- +goose Down

DROP INDEX audit_log_entity;
ALTER TABLE audit_log DROP COLUMN token_name;
ALTER TABLE audit_log DROP COLUMN token_id;
ALTER TABLE audit_log DROP COLUMN actor_name;
ALTER TABLE audit_log DROP COLUMN origin;
DROP TABLE api_tokens;
