-- The code of the reason behind a camera's state (ip_in_use, dhcp_waiting,
-- no_heartbeat…), so the panel explains it in the user's language; the
-- reason itself stays in English for logs and the API.

-- +goose Up

ALTER TABLE camera_status ADD COLUMN reason_code TEXT NOT NULL DEFAULT '';

-- +goose Down

ALTER TABLE camera_status DROP COLUMN reason_code;
