-- name: InsertPlugin :exec
INSERT INTO plugins (id, package_id, engine, engine_version, executable, dir, permissions_json, descriptor_json, installed_at)
VALUES (@id, @package_id, @engine, @engine_version, @executable, @dir, @permissions_json, @descriptor_json, @installed_at);

-- name: GetPlugin :one
SELECT plugins.*, packages.pkg_id, packages.signature_status, packages.signer, packages.sha256
FROM plugins JOIN packages ON packages.id = plugins.package_id
WHERE plugins.id = @id;

-- name: GetPluginByPackage :one
SELECT * FROM plugins WHERE package_id = @package_id;

-- name: ListPlugins :many
SELECT plugins.*, packages.pkg_id, packages.signature_status, packages.signer, packages.sha256
FROM plugins JOIN packages ON packages.id = plugins.package_id
ORDER BY plugins.engine, plugins.installed_at DESC;

-- name: ListEnabledPlugins :many
SELECT * FROM plugins WHERE enabled = 1 ORDER BY engine;

-- name: DisableEngine :exec
UPDATE plugins SET enabled = 0 WHERE engine = @engine AND enabled = 1;

-- name: EnablePlugin :exec
UPDATE plugins SET enabled = 1, approved_by = @approved_by, approved_at = @approved_at WHERE id = @id;

-- name: DisablePlugin :execrows
UPDATE plugins SET enabled = 0 WHERE id = @id AND enabled = 1;
