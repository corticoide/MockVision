-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: CreateUser :exec
INSERT INTO users (id, username, password_hash, role, disabled, created_at)
VALUES (@id, @username, @password_hash, @role, 0, @created_at);

-- name: GetUserByUsername :one
SELECT * FROM users WHERE username = @username;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = @id;

-- name: CreateSession :exec
INSERT INTO sessions (id, user_id, expires_at, ip, user_agent, created_at)
VALUES (@id, @user_id, @expires_at, @ip, @user_agent, @created_at);

-- name: GetSession :one
SELECT sessions.id, sessions.user_id, sessions.expires_at, users.username, users.role, users.disabled
FROM sessions
JOIN users ON users.id = sessions.user_id
WHERE sessions.id = @id;

-- name: ExtendSession :exec
UPDATE sessions SET expires_at = @expires_at WHERE id = @id;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id = @id;

-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions WHERE expires_at < @now;

-- name: GetSetting :one
SELECT value_json FROM settings WHERE key = @key;

-- name: ListSettings :many
SELECT * FROM settings ORDER BY key;

-- name: UpsertSetting :exec
INSERT INTO settings (key, value_json) VALUES (@key, @value_json)
ON CONFLICT (key) DO UPDATE SET value_json = excluded.value_json;
