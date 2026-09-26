-- name: InsertAudit :exec
INSERT INTO audit_log (id, at, actor_type, actor_id, actor_name, origin, origin_ip, token_id, token_name, action, entity_type, entity_id, diff_json)
VALUES (@id, @at, @actor_type, @actor_id, @actor_name, @origin, @origin_ip, @token_id, @token_name, @action, @entity_type, @entity_id, @diff_json);

-- The entity name is resolved while it still exists; entries of deleted
-- entities keep the name in their diff.

-- name: ListAudit :many
SELECT audit_log.*,
       CAST(coalesce(cameras.name, targets.name, assets.filename, users.username, api_tokens.name,
                     profiles.profile_id || '@' || profiles.version, '') AS TEXT) AS entity_name
FROM audit_log
LEFT JOIN cameras ON audit_log.entity_type = 'camera' AND cameras.id = audit_log.entity_id
LEFT JOIN targets ON audit_log.entity_type = 'target' AND targets.id = audit_log.entity_id
LEFT JOIN assets ON audit_log.entity_type = 'asset' AND assets.id = audit_log.entity_id
LEFT JOIN users ON audit_log.entity_type = 'user' AND users.id = audit_log.entity_id
LEFT JOIN api_tokens ON audit_log.entity_type = 'token' AND api_tokens.id = audit_log.entity_id
LEFT JOIN profiles ON audit_log.entity_type = 'profile' AND profiles.id = audit_log.entity_id
WHERE (CAST(@cursor AS TEXT) = '' OR audit_log.id < CAST(@cursor AS TEXT))
  AND (CAST(@origin AS TEXT) = '' OR audit_log.origin = CAST(@origin AS TEXT))
  AND (CAST(@entity_type AS TEXT) = '' OR audit_log.entity_type = CAST(@entity_type AS TEXT))
  AND (CAST(@entity_id AS TEXT) = '' OR audit_log.entity_id = CAST(@entity_id AS TEXT))
  AND (CAST(@token_id AS TEXT) = '' OR audit_log.token_id = CAST(@token_id AS TEXT))
  AND (CAST(@action AS TEXT) = '' OR audit_log.action = CAST(@action AS TEXT) OR audit_log.action LIKE CAST(@action AS TEXT) || '.%')
  AND (CAST(@since AS INTEGER) = 0 OR audit_log.at >= CAST(@since AS INTEGER))
  AND (CAST(@until AS INTEGER) = 0 OR audit_log.at < CAST(@until AS INTEGER))
ORDER BY audit_log.id DESC
LIMIT @limit;

-- name: GetAudit :one
SELECT audit_log.*,
       CAST(coalesce(cameras.name, targets.name, assets.filename, users.username, api_tokens.name,
                     profiles.profile_id || '@' || profiles.version, '') AS TEXT) AS entity_name
FROM audit_log
LEFT JOIN cameras ON audit_log.entity_type = 'camera' AND cameras.id = audit_log.entity_id
LEFT JOIN targets ON audit_log.entity_type = 'target' AND targets.id = audit_log.entity_id
LEFT JOIN assets ON audit_log.entity_type = 'asset' AND assets.id = audit_log.entity_id
LEFT JOIN users ON audit_log.entity_type = 'user' AND users.id = audit_log.entity_id
LEFT JOIN api_tokens ON audit_log.entity_type = 'token' AND api_tokens.id = audit_log.entity_id
LEFT JOIN profiles ON audit_log.entity_type = 'profile' AND profiles.id = audit_log.entity_id
WHERE audit_log.id = @id;

-- name: DeleteAuditBefore :execrows
DELETE FROM audit_log WHERE at < @before;
