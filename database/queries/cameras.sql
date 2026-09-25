-- name: InsertCamera :exec
INSERT INTO cameras (id, name, profile_id, profile_version, serial, desired_state, autostart, tags_json, created_at, updated_at)
VALUES (@id, @name, @profile_id, @profile_version, @serial, @desired_state, @autostart, @tags_json, @created_at, @updated_at);

-- name: GetCamera :one
SELECT * FROM cameras WHERE id = @id;

-- name: ListCameras :many
SELECT * FROM cameras ORDER BY name;

-- name: CountCameras :one
SELECT count(*) FROM cameras;

-- name: CameraIDByName :one
SELECT id FROM cameras WHERE name = @name;

-- name: SetCameraDesired :exec
UPDATE cameras SET desired_state = @desired_state, updated_at = @updated_at WHERE id = @id;

-- name: UpdateCameraMeta :exec
UPDATE cameras SET name = @name, autostart = @autostart, tags_json = @tags_json, updated_at = @updated_at WHERE id = @id;

-- name: TouchCamera :exec
UPDATE cameras SET updated_at = @updated_at WHERE id = @id;

-- name: DeleteCamera :execrows
DELETE FROM cameras WHERE id = @id;

-- name: InsertCameraNetwork :exec
INSERT INTO camera_network (camera_id, mode, parent_if, mac, ip_mode, ip, netmask, gateway, dns_json)
VALUES (@camera_id, @mode, @parent_if, @mac, @ip_mode, @ip, @netmask, @gateway, @dns_json);

-- name: GetCameraNetwork :one
SELECT * FROM camera_network WHERE camera_id = @camera_id;

-- name: ListCameraNetworks :many
SELECT * FROM camera_network;

-- name: UpdateCameraNetwork :exec
UPDATE camera_network SET parent_if = @parent_if, mac = @mac, ip = @ip, netmask = @netmask, gateway = @gateway, dns_json = @dns_json
WHERE camera_id = @camera_id;

-- name: CameraIDByIP :one
SELECT camera_id FROM camera_network WHERE ip = @ip;

-- name: CameraIDByMAC :one
SELECT camera_id FROM camera_network WHERE mac = @mac;

-- name: UpsertCameraState :exec
INSERT INTO camera_state (camera_id, key, value_json, origin, updated_at)
VALUES (@camera_id, @key, @value_json, @origin, @updated_at)
ON CONFLICT (camera_id, key) DO UPDATE SET
  value_json = excluded.value_json, origin = excluded.origin, updated_at = excluded.updated_at;

-- name: ListCameraState :many
SELECT * FROM camera_state WHERE camera_id = @camera_id ORDER BY key;

-- name: DeleteCameraState :exec
DELETE FROM camera_state WHERE camera_id = @camera_id;

-- name: InsertCameraProtocol :exec
INSERT INTO camera_protocols (camera_id, engine_key, enabled, port, options_json)
VALUES (@camera_id, @engine_key, @enabled, @port, @options_json);

-- name: ListCameraProtocols :many
SELECT * FROM camera_protocols WHERE camera_id = @camera_id ORDER BY engine_key;

-- name: UpdateCameraProtocol :exec
UPDATE camera_protocols SET enabled = @enabled, port = @port WHERE camera_id = @camera_id AND engine_key = @engine_key;

-- name: InsertCameraUser :exec
INSERT INTO camera_users (id, camera_id, username, password_enc, role)
VALUES (@id, @camera_id, @username, @password_enc, @role);

-- name: ListCameraUsers :many
SELECT * FROM camera_users WHERE camera_id = @camera_id ORDER BY username;

-- name: DeleteCameraUsers :exec
DELETE FROM camera_users WHERE camera_id = @camera_id;

-- name: UpsertCameraStatus :exec
INSERT INTO camera_status (camera_id, actual_state, reason, started_at, last_heartbeat, updated_at)
VALUES (@camera_id, @actual_state, @reason, @started_at, @last_heartbeat, @updated_at)
ON CONFLICT (camera_id) DO UPDATE SET
  actual_state = excluded.actual_state, reason = excluded.reason, started_at = excluded.started_at,
  last_heartbeat = excluded.last_heartbeat, updated_at = excluded.updated_at;

-- name: GetCameraStatus :one
SELECT * FROM camera_status WHERE camera_id = @camera_id;

-- name: ListCameraStatuses :many
SELECT * FROM camera_status;

-- name: UpsertCameraStream :exec
INSERT INTO camera_streams (camera_id, stream, asset_id, rendition_id)
VALUES (@camera_id, @stream, @asset_id, @rendition_id)
ON CONFLICT (camera_id, stream) DO UPDATE SET asset_id = excluded.asset_id, rendition_id = excluded.rendition_id;

-- name: ListCameraStreams :many
SELECT * FROM camera_streams WHERE camera_id = @camera_id ORDER BY stream;

-- name: InsertCameraTarget :exec
INSERT INTO camera_targets (camera_id, target_id, event_types_json, overrides_json)
VALUES (@camera_id, @target_id, @event_types_json, @overrides_json);

-- name: DeleteCameraTargets :exec
DELETE FROM camera_targets WHERE camera_id = @camera_id;

-- name: ListCameraTargets :many
SELECT camera_targets.camera_id, camera_targets.event_types_json, camera_targets.overrides_json,
       targets.id, targets.name, targets.type, targets.config_json, targets.secret_enc, targets.enabled
FROM camera_targets
JOIN targets ON targets.id = camera_targets.target_id
WHERE camera_targets.camera_id = @camera_id
ORDER BY targets.name;

-- name: CamerasUsingTarget :many
SELECT camera_id FROM camera_targets WHERE target_id = @target_id;
