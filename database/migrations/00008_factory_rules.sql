-- Whether a camera got its profile's factory rules (feature 7). Cameras
-- created before profiles declared them stood in a default line or region;
-- at boot the node gives them their profile's factory rules, once, if they
-- have no rules.

-- +goose Up

ALTER TABLE cameras ADD COLUMN factory_rules_applied INTEGER NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE cameras DROP COLUMN factory_rules_applied;
