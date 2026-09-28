-- Feature 7, rules and triggers (D39, D40): the VCA rules drawn on a
-- camera's picture, and the stored triggers that make events happen on
-- them. A rule's points go from 0 to 1 over the picture; its params_json
-- holds the direction of a line, the events of a region and the object
-- classes it detects. A trigger's params_json holds the event type, the
-- rule, the wait between events and the data it generates. Manual events
-- need no stored trigger.

-- +goose Up

CREATE TABLE rules (
  id            TEXT PRIMARY KEY,
  camera_id     TEXT NOT NULL REFERENCES cameras (id) ON DELETE CASCADE,
  position      INTEGER NOT NULL,
  name          TEXT NOT NULL,
  type          TEXT NOT NULL CHECK (type IN ('line', 'region')),
  geometry_json TEXT NOT NULL,
  params_json   TEXT NOT NULL DEFAULT '{}',
  enabled       INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1))
) STRICT;

CREATE INDEX rules_camera ON rules (camera_id, position);

CREATE TABLE triggers (
  id          TEXT PRIMARY KEY,
  camera_id   TEXT NOT NULL REFERENCES cameras (id) ON DELETE CASCADE,
  position    INTEGER NOT NULL,
  name        TEXT NOT NULL,
  type        TEXT NOT NULL CHECK (type IN ('manual', 'random', 'schedule', 'script', 'external')),
  params_json TEXT NOT NULL DEFAULT '{}',
  enabled     INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1))
) STRICT;

CREATE INDEX triggers_camera ON triggers (camera_id, position);

-- Events stored what produced them ("manual") in trigger_id and a stand-in
-- rule ID ("1") in rule_id; both columns now name stored triggers and rules.
UPDATE events SET trigger_id = NULL WHERE trigger_id = 'manual';
UPDATE events SET rule_id = NULL WHERE rule_id = '1';

-- +goose Down

DROP TABLE triggers;
DROP TABLE rules;
