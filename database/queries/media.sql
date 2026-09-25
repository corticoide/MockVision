-- name: InsertAsset :exec
INSERT INTO assets (id, sha256, kind, mime, width, height, size, filename, builtin, created_at)
VALUES (@id, @sha256, @kind, @mime, @width, @height, @size, @filename, @builtin, @created_at);

-- name: GetAsset :one
SELECT * FROM assets WHERE id = @id;

-- name: GetAssetBySHA256 :one
SELECT * FROM assets WHERE sha256 = @sha256;

-- name: ListAssets :many
SELECT assets.*,
       (SELECT count(*) FROM camera_streams WHERE camera_streams.asset_id = assets.id) AS camera_count
FROM assets
ORDER BY assets.builtin DESC, assets.created_at DESC;

-- name: DeleteAsset :execrows
DELETE FROM assets WHERE id = @id;

-- name: InsertRendition :exec
INSERT INTO renditions (id, asset_id, codec, width, height, fps, gop, bitrate, sha256, status, error, created_at)
VALUES (@id, @asset_id, @codec, @width, @height, @fps, @gop, @bitrate, '', 'pending', '', @created_at)
ON CONFLICT (asset_id, codec, width, height, fps, gop, bitrate) DO NOTHING;

-- name: GetRenditionByParams :one
SELECT * FROM renditions
WHERE asset_id = @asset_id AND codec = @codec AND width = @width AND height = @height
  AND fps = @fps AND gop = @gop AND bitrate = @bitrate;

-- name: GetRendition :one
SELECT * FROM renditions WHERE id = @id;

-- name: SetRenditionStatus :exec
UPDATE renditions SET status = @status, sha256 = @sha256, error = @error WHERE id = @id;

-- name: ListRenditionsByStatus :many
SELECT * FROM renditions WHERE status = @status ORDER BY created_at;
