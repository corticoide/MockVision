-- name: InsertDevice :exec
INSERT INTO devices (id, name, host, ports_json, port_map_json, username, secret_enc, kind, authorized, detected_json, created_at, updated_at)
VALUES (@id, @name, @host, @ports_json, @port_map_json, @username, @secret_enc, @kind, @authorized, @detected_json, @created_at, @updated_at);

-- name: GetDevice :one
SELECT * FROM devices WHERE id = @id;

-- name: ListDevices :many
SELECT * FROM devices ORDER BY created_at DESC;

-- name: UpdateDevice :exec
UPDATE devices SET name = @name, host = @host, ports_json = @ports_json, port_map_json = @port_map_json, username = @username,
  secret_enc = @secret_enc, kind = @kind, authorized = @authorized, updated_at = @updated_at WHERE id = @id;

-- name: SetDeviceDetected :exec
UPDATE devices SET detected_json = @detected_json, kind = @kind, updated_at = @updated_at WHERE id = @id;

-- name: DeleteDevice :execrows
DELETE FROM devices WHERE id = @id;

-- name: CountDevicesByHost :one
SELECT count(*) FROM devices WHERE host = @host AND id <> @id;

-- name: InsertCapture :exec
INSERT INTO captures (id, device_id, job_id, program_ref, status, artifacts_json, draft_profile_id, created_at)
VALUES (@id, @device_id, @job_id, @program_ref, @status, @artifacts_json, @draft_profile_id, @created_at);

-- name: GetCapture :one
SELECT * FROM captures WHERE id = @id;

-- name: ListCapturesByDevice :many
SELECT * FROM captures WHERE device_id = @device_id ORDER BY created_at DESC LIMIT @lim;

-- name: ListCaptures :many
SELECT * FROM captures ORDER BY created_at DESC LIMIT @lim;

-- name: FinishCapture :exec
UPDATE captures SET status = @status, artifacts_json = @artifacts_json, draft_profile_id = @draft_profile_id, finished_at = @finished_at WHERE id = @id;

-- name: InsertProgram :exec
INSERT INTO programs (id, package_id, program_id, version, name, compatible_json, steps_json, installed_at)
VALUES (@id, @package_id, @program_id, @version, @name, @compatible_json, @steps_json, @installed_at);

-- name: ListPrograms :many
SELECT programs.*, packages.signature_status, packages.signer FROM programs
JOIN packages ON packages.id = programs.package_id
ORDER BY programs.program_id;

-- name: GetProgramByRef :one
SELECT * FROM programs WHERE program_id = @program_id AND version = @version;

-- name: SetCaptureJob :exec
UPDATE captures SET job_id = @job_id WHERE id = @id;
