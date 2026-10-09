-- Feature 19, event receiver: each device has a stable token; a camera is
-- pointed at the node's receiver URL for that token, and a capture records
-- the payloads it pushes (D44, D73).

-- +goose Up

ALTER TABLE devices ADD COLUMN receiver_token TEXT NOT NULL DEFAULT '';

-- +goose Down

ALTER TABLE devices DROP COLUMN receiver_token;
