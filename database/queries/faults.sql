-- name: InsertFault :exec
INSERT INTO faults (id, camera_id, kind, params_json, started_at, expires_at, created_by)
VALUES (@id, @camera_id, @kind, @params_json, @started_at, @expires_at, @created_by);

-- name: GetFault :one
SELECT * FROM faults WHERE id = @id;

-- name: ListOpenFaults :many
SELECT * FROM faults WHERE ended_at IS NULL ORDER BY started_at;

-- name: ListCameraFaults :many
SELECT * FROM faults WHERE camera_id = @camera_id ORDER BY started_at DESC LIMIT @limit;

-- name: EndFault :execrows
UPDATE faults SET ended_at = @ended_at, ended_by = @ended_by WHERE id = @id AND ended_at IS NULL;

-- name: ListExpiredFaults :many
SELECT * FROM faults WHERE ended_at IS NULL AND expires_at IS NOT NULL AND expires_at <= @now;

-- name: DeleteFaultsBefore :execrows
DELETE FROM faults WHERE ended_at IS NOT NULL AND ended_at < @before;
