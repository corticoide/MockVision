-- name: UpsertRequestStat :exec
-- Partial reports of the same minute add up; the median is weighted by
-- the requests of each report and the 95th percentile is the highest.
INSERT INTO request_stats (camera_id, minute, client_ip, route, count, errors, auth_failures, p50_ms, p95_ms, max_ms, first_at, last_at)
VALUES (@camera_id, @minute, @client_ip, @route, @count, @errors, @auth_failures, @p50_ms, @p95_ms, @max_ms, @first_at, @last_at)
ON CONFLICT (camera_id, minute, client_ip, route) DO UPDATE SET
  p50_ms = (request_stats.p50_ms * request_stats.count + excluded.p50_ms * excluded.count) / (request_stats.count + excluded.count),
  count = request_stats.count + excluded.count,
  errors = request_stats.errors + excluded.errors,
  auth_failures = request_stats.auth_failures + excluded.auth_failures,
  p95_ms = max(request_stats.p95_ms, excluded.p95_ms),
  max_ms = max(request_stats.max_ms, excluded.max_ms),
  first_at = min(request_stats.first_at, excluded.first_at),
  last_at = max(request_stats.last_at, excluded.last_at);

-- name: UpsertConnectionStat :exec
INSERT INTO connection_stats (camera_id, minute, protocol, client_ip, opened, closed, duration_ms, max_duration_ms)
VALUES (@camera_id, @minute, @protocol, @client_ip, @opened, @closed, @duration_ms, @max_duration_ms)
ON CONFLICT (camera_id, minute, protocol, client_ip) DO UPDATE SET
  opened = connection_stats.opened + excluded.opened,
  closed = connection_stats.closed + excluded.closed,
  duration_ms = connection_stats.duration_ms + excluded.duration_ms,
  max_duration_ms = max(connection_stats.max_duration_ms, excluded.max_duration_ms);

-- name: UpsertGap :exec
INSERT INTO gaps (camera_id, protocol, summary, client_ip, count, first_at, last_at)
VALUES (@camera_id, @protocol, @summary, @client_ip, @count, @at, @at)
ON CONFLICT (camera_id, protocol, summary, client_ip) DO UPDATE SET
  count = gaps.count + excluded.count,
  last_at = max(gaps.last_at, excluded.last_at);

-- name: ListRequestStats :many
SELECT * FROM request_stats WHERE camera_id = @camera_id AND minute >= @since ORDER BY minute, client_ip, route;

-- name: ListConnectionStats :many
SELECT * FROM connection_stats WHERE camera_id = @camera_id AND minute >= @since ORDER BY minute, protocol, client_ip;

-- name: ListGaps :many
SELECT * FROM gaps WHERE camera_id = @camera_id ORDER BY last_at DESC LIMIT 500;

-- name: InsertCameraLog :exec
INSERT INTO camera_logs (camera_id, at, level, source, msg, attrs_json) VALUES (@camera_id, @at, @level, @source, @msg, @attrs_json);

-- name: ListCameraLogs :many
SELECT * FROM camera_logs WHERE camera_id = @camera_id AND id < @before ORDER BY id DESC LIMIT @lim;

-- name: InsertCameraMetric :exec
INSERT OR REPLACE INTO camera_metrics (camera_id, resolution, at, cpu_percent, rss_bytes, clients, bytes_in, bytes_out, requests)
VALUES (@camera_id, @resolution, @at, @cpu_percent, @rss_bytes, @clients, @bytes_in, @bytes_out, @requests);

-- name: ListCameraMetrics :many
SELECT * FROM camera_metrics WHERE camera_id = @camera_id AND resolution = @resolution AND at > @since ORDER BY at;

-- name: DeleteRequestStatsBefore :execrows
DELETE FROM request_stats WHERE minute < @before;

-- name: DeleteConnectionStatsBefore :execrows
DELETE FROM connection_stats WHERE minute < @before;

-- name: DeleteGapsBefore :execrows
DELETE FROM gaps WHERE last_at < @before;

-- name: DeleteCameraLogsBefore :execrows
DELETE FROM camera_logs WHERE at < @before;

-- name: TrimCameraLogs :execrows
-- Keeps the newest logs of a camera.
DELETE FROM camera_logs WHERE camera_logs.camera_id = @camera_id AND camera_logs.id <= (
  SELECT l.id FROM camera_logs AS l WHERE l.camera_id = @camera_id ORDER BY l.id DESC LIMIT 1 OFFSET @keep);

-- name: DeleteCameraMetricsBefore :execrows
DELETE FROM camera_metrics WHERE resolution = @resolution AND at < @before;

-- name: ListCameraIDs :many
SELECT id FROM cameras ORDER BY id;

-- name: ListCameraStates :many
SELECT cameras.id, cameras.name, COALESCE(camera_status.actual_state, 'stopped') AS state
FROM cameras LEFT JOIN camera_status ON camera_status.camera_id = cameras.id
ORDER BY cameras.name;
