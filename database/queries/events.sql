-- name: InsertTarget :exec
INSERT INTO targets (id, name, type, config_json, secret_enc, enabled, created_at)
VALUES (@id, @name, @type, @config_json, @secret_enc, @enabled, @created_at);

-- name: GetTarget :one
SELECT * FROM targets WHERE id = @id;

-- name: ListTargets :many
SELECT targets.*,
       (SELECT count(*) FROM camera_targets WHERE camera_targets.target_id = targets.id) AS camera_count
FROM targets
ORDER BY targets.name;

-- name: UpdateTarget :exec
UPDATE targets SET name = @name, config_json = @config_json, secret_enc = @secret_enc, enabled = @enabled WHERE id = @id;

-- name: DeleteTarget :execrows
DELETE FROM targets WHERE id = @id;

-- name: InsertEvent :exec
INSERT INTO events (id, camera_id, type, at, data_json, rule_id, trigger_id)
VALUES (@id, @camera_id, @type, @at, @data_json, @rule_id, @trigger_id);

-- name: GetEvent :one
SELECT events.*, cameras.name AS camera_name
FROM events
JOIN cameras ON cameras.id = events.camera_id
WHERE events.id = @id;

-- name: ListEvents :many
SELECT events.*, cameras.name AS camera_name
FROM events
JOIN cameras ON cameras.id = events.camera_id
WHERE (CAST(@cursor AS TEXT) = '' OR events.id < CAST(@cursor AS TEXT))
  AND (CAST(@camera_id AS TEXT) = '' OR events.camera_id = CAST(@camera_id AS TEXT))
  AND (CAST(@type AS TEXT) = '' OR events.type = CAST(@type AS TEXT))
  AND (CAST(@delivery AS TEXT) = '' OR EXISTS (
        SELECT 1 FROM deliveries WHERE deliveries.event_id = events.id AND deliveries.status = CAST(@delivery AS TEXT)))
ORDER BY events.id DESC
LIMIT @limit;

-- name: DeleteEventsBefore :execrows
DELETE FROM events WHERE at < @before;

-- name: InsertDelivery :exec
INSERT INTO deliveries (id, event_id, target_id, attempt, at, status, http_status, latency_ms, error)
VALUES (@id, @event_id, @target_id, @attempt, @at, @status, @http_status, @latency_ms, @error);

-- name: ListDeliveriesForEvents :many
SELECT deliveries.*, coalesce(targets.name, '') AS target_name
FROM deliveries
LEFT JOIN targets ON targets.id = deliveries.target_id
WHERE deliveries.event_id IN (sqlc.slice('event_ids'))
ORDER BY deliveries.event_id, deliveries.target_id, deliveries.attempt;

-- name: DeleteDeliveriesBefore :execrows
DELETE FROM deliveries WHERE at < @before;
