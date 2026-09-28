-- name: ListCameraRules :many
SELECT * FROM rules WHERE camera_id = @camera_id ORDER BY position;

-- name: ListAllRules :many
SELECT * FROM rules ORDER BY camera_id, position;

-- name: InsertRule :exec
INSERT INTO rules (id, camera_id, position, name, type, geometry_json, params_json, enabled)
VALUES (@id, @camera_id, @position, @name, @type, @geometry_json, @params_json, @enabled);

-- name: DeleteCameraRules :exec
DELETE FROM rules WHERE camera_id = @camera_id;

-- name: ListCameraTriggers :many
SELECT * FROM triggers WHERE camera_id = @camera_id ORDER BY position;

-- name: ListAllTriggers :many
SELECT * FROM triggers ORDER BY camera_id, position;

-- name: InsertTrigger :exec
INSERT INTO triggers (id, camera_id, position, name, type, params_json, enabled)
VALUES (@id, @camera_id, @position, @name, @type, @params_json, @enabled);

-- name: DeleteCameraTriggers :exec
DELETE FROM triggers WHERE camera_id = @camera_id;
