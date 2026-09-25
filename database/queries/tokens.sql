-- name: CreateAPIToken :exec
INSERT INTO api_tokens (id, user_id, name, token_hash, prefix, scopes_json, expires_at, created_at)
VALUES (@id, @user_id, @name, @token_hash, @prefix, @scopes_json, @expires_at, @created_at);

-- name: ListAPITokens :many
SELECT * FROM api_tokens WHERE user_id = @user_id ORDER BY created_at DESC, id DESC;

-- name: GetAPIToken :one
SELECT * FROM api_tokens WHERE id = @id AND user_id = @user_id;

-- name: GetAPITokenByHash :one
SELECT api_tokens.id, api_tokens.name, api_tokens.scopes_json, api_tokens.expires_at, api_tokens.last_used_at,
       users.id AS user_id, users.username, users.role, users.disabled
FROM api_tokens
JOIN users ON users.id = api_tokens.user_id
WHERE api_tokens.token_hash = @token_hash;

-- name: TouchAPIToken :exec
UPDATE api_tokens SET last_used_at = @last_used_at, last_used_ip = @last_used_ip WHERE id = @id;

-- name: DeleteAPIToken :execrows
DELETE FROM api_tokens WHERE id = @id AND user_id = @user_id;
