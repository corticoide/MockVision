-- Events keep when the node received them and how many deliveries they
-- were expected to get (audit B15 and B5). Retention goes by the reception
-- time: an event's own time comes from the camera's clock, which the
-- clients of its emulated API can move. The expected deliveries tell an
-- event no target wanted from one still being delivered; -1 means unknown,
-- for events stored before this migration.

-- +goose Up

ALTER TABLE events ADD COLUMN received_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE events ADD COLUMN expected_deliveries INTEGER NOT NULL DEFAULT -1;
UPDATE events SET received_at = at;
CREATE INDEX events_received_at ON events (received_at);

-- +goose Down

DROP INDEX events_received_at;
ALTER TABLE events DROP COLUMN expected_deliveries;
ALTER TABLE events DROP COLUMN received_at;
