-- name: GetCameraStorage :one
SELECT * FROM camera_storage WHERE camera_id = @camera_id;

-- name: ListCameraStorage :many
SELECT * FROM camera_storage;

-- name: UpsertCameraStorage :exec
INSERT INTO camera_storage (camera_id, kind, size_mb, overwrite, nas_url, nas_username, nas_secret_enc)
VALUES (@camera_id, @kind, @size_mb, @overwrite, @nas_url, @nas_username, @nas_secret_enc)
ON CONFLICT (camera_id) DO UPDATE SET kind = excluded.kind, size_mb = excluded.size_mb, overwrite = excluded.overwrite,
  nas_url = excluded.nas_url, nas_username = excluded.nas_username, nas_secret_enc = excluded.nas_secret_enc;

-- name: InsertRecording :exec
INSERT INTO recordings (id, camera_id, event_id, event_type, kind, stream, name, size, start_at, end_at, location, created_at)
VALUES (@id, @camera_id, @event_id, @event_type, @kind, @stream, @name, @size, @start_at, @end_at, @location, @created_at);

-- name: GetRecording :one
SELECT * FROM recordings WHERE id = @id;

-- name: GetRecordingByName :one
SELECT * FROM recordings WHERE camera_id = @camera_id AND location = @location AND name = @name;

-- name: PromisedSD :many
SELECT s.camera_id, s.size_mb, CAST(coalesce(sum(r.size), 0) AS INTEGER) AS used
FROM camera_storage s LEFT JOIN recordings r ON r.camera_id = s.camera_id AND r.location = 'sd'
WHERE s.kind = 'sd'
GROUP BY s.camera_id, s.size_mb;

-- name: ListNASHosts :many
SELECT nas_url FROM camera_storage WHERE kind = 'nas';

-- name: FindRecordings :many
SELECT * FROM recordings
WHERE camera_id = @camera_id AND location = @location
  AND end_at >= @from_at AND start_at <= @to_at
  AND (CAST(@kind AS TEXT) = '' OR kind = CAST(@kind AS TEXT))
  AND (CAST(@event_type AS TEXT) = '' OR event_type = CAST(@event_type AS TEXT))
ORDER BY start_at, name
LIMIT @limit;

-- name: ListRecordingsByEvent :many
SELECT * FROM recordings WHERE event_id = @event_id ORDER BY kind;

-- name: RecordingUsage :one
SELECT CAST(coalesce(sum(size), 0) AS INTEGER) AS used, count(*) AS files
FROM recordings WHERE camera_id = @camera_id AND location = @location;

-- name: OldestRecordings :many
SELECT * FROM recordings WHERE camera_id = @camera_id AND location = @location ORDER BY start_at, name LIMIT @limit;

-- name: DeleteRecording :exec
DELETE FROM recordings WHERE id = @id;

-- name: DeleteCameraRecordings :execrows
DELETE FROM recordings WHERE camera_id = @camera_id AND location = @location;

-- name: RecentRecordings :many
SELECT * FROM recordings
WHERE camera_id = @camera_id AND location = @location
  AND (CAST(@kind AS TEXT) = '' OR kind = CAST(@kind AS TEXT))
ORDER BY start_at DESC, name DESC
LIMIT @limit;
