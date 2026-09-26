-- name: InsertJob :exec
INSERT INTO jobs (id, type, title, status, params_json, dedupe_key, created_by, created_at)
VALUES (@id, @type, @title, 'queued', @params_json, @dedupe_key, @created_by, @created_at);

-- name: GetJob :one
SELECT * FROM jobs WHERE id = @id;

-- name: GetOpenJobByKey :one
SELECT * FROM jobs
WHERE dedupe_key = @dedupe_key AND status IN ('queued', 'running', 'waiting', 'interrupted');

-- name: UpdateJob :exec
UPDATE jobs
SET status = @status, progress = @progress, step = @step, checkpoint_json = @checkpoint_json,
    question_json = @question_json, result_json = @result_json, started_at = @started_at,
    finished_at = @finished_at, error = @error
WHERE id = @id;

-- Active covers what still needs the node or the user: queued, running,
-- waiting and interrupted jobs.

-- name: ListJobs :many
SELECT * FROM jobs
WHERE (CAST(@cursor AS TEXT) = '' OR id < CAST(@cursor AS TEXT))
  AND (CAST(@type AS TEXT) = '' OR type = CAST(@type AS TEXT))
  AND (CAST(@status AS TEXT) = ''
       OR status = CAST(@status AS TEXT)
       OR (CAST(@status AS TEXT) = 'active' AND status IN ('queued', 'running', 'waiting', 'interrupted'))
       OR (CAST(@status AS TEXT) = 'finished' AND status IN ('completed', 'failed', 'canceled')))
ORDER BY id DESC
LIMIT @limit;

-- name: ListJobsByStatus :many
SELECT * FROM jobs WHERE status = @status ORDER BY id LIMIT @limit;

-- name: ListFinishedJobsBefore :many
SELECT id FROM jobs WHERE finished_at < @before AND status IN ('completed', 'failed', 'canceled');

-- name: DeleteJob :exec
DELETE FROM jobs WHERE id = @id;

-- name: InsertJobEvent :exec
INSERT INTO job_events (job_id, seq, at, kind, payload_json)
VALUES (@job_id, @seq, @at, @kind, @payload_json);

-- name: MaxJobEventSeq :one
SELECT CAST(coalesce(max(seq), 0) AS INTEGER) AS seq FROM job_events WHERE job_id = @job_id;

-- name: ListJobEvents :many
SELECT * FROM job_events WHERE job_id = @job_id AND seq > @after ORDER BY seq LIMIT @limit;
