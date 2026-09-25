-- name: InsertPackage :exec
INSERT INTO packages (id, kind, pkg_id, version, sha256, signature_status, signer, manifest_json, report_json, enabled, installed_at)
VALUES (@id, @kind, @pkg_id, @version, @sha256, @signature_status, @signer, @manifest_json, @report_json, @enabled, @installed_at);

-- name: GetPackage :one
SELECT * FROM packages WHERE id = @id;

-- name: GetPackageByKey :one
SELECT * FROM packages WHERE kind = @kind AND pkg_id = @pkg_id AND version = @version;

-- name: ListPackages :many
SELECT * FROM packages ORDER BY installed_at DESC;

-- name: InsertProfile :exec
INSERT INTO profiles (id, package_id, profile_id, version, name, vendor, model, firmware_json, resolved_json, level, coverage_json, archived, created_at)
VALUES (@id, @package_id, @profile_id, @version, @name, @vendor, @model, @firmware_json, @resolved_json, @level, @coverage_json, 0, @created_at);

-- name: GetProfile :one
SELECT * FROM profiles WHERE id = @id;

-- name: GetProfileByRef :one
SELECT * FROM profiles WHERE profile_id = @profile_id AND version = @version;

-- name: ListProfiles :many
SELECT profiles.id, profiles.package_id, profiles.profile_id, profiles.version, profiles.name, profiles.vendor,
       profiles.model, profiles.firmware_json, profiles.level, profiles.coverage_json, profiles.archived,
       profiles.created_at, packages.signature_status,
       (SELECT count(*) FROM cameras WHERE cameras.profile_id = profiles.profile_id AND cameras.profile_version = profiles.version) AS camera_count
FROM profiles
JOIN packages ON packages.id = profiles.package_id
ORDER BY profiles.profile_id, profiles.created_at DESC;

-- name: SetProfileArchived :exec
UPDATE profiles SET archived = @archived WHERE id = @id;
