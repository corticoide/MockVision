-- Feature 14, diagnostics: what each camera served, a minute at a time,
-- per client and route; the connections of each client; the requests its
-- profile did not know (D79); its log; and its metrics past the ten
-- minutes the service keeps in memory, every 10 s for a day and every
-- minute for a week (D92).

-- +goose Up

CREATE TABLE request_stats (
  camera_id     TEXT NOT NULL REFERENCES cameras (id) ON DELETE CASCADE,
  minute        INTEGER NOT NULL,
  client_ip     TEXT NOT NULL,
  route         TEXT NOT NULL,
  count         INTEGER NOT NULL,
  errors        INTEGER NOT NULL DEFAULT 0,
  auth_failures INTEGER NOT NULL DEFAULT 0,
  p50_ms        REAL NOT NULL DEFAULT 0,
  p95_ms        REAL NOT NULL DEFAULT 0,
  max_ms        REAL NOT NULL DEFAULT 0,
  first_at      INTEGER NOT NULL,
  last_at       INTEGER NOT NULL,
  PRIMARY KEY (camera_id, minute, client_ip, route)
) STRICT, WITHOUT ROWID;

CREATE INDEX request_stats_minute ON request_stats (minute);

CREATE TABLE connection_stats (
  camera_id       TEXT NOT NULL REFERENCES cameras (id) ON DELETE CASCADE,
  minute          INTEGER NOT NULL,
  protocol        TEXT NOT NULL,
  client_ip       TEXT NOT NULL,
  opened          INTEGER NOT NULL DEFAULT 0,
  closed          INTEGER NOT NULL DEFAULT 0,
  duration_ms     INTEGER NOT NULL DEFAULT 0,
  max_duration_ms INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (camera_id, minute, protocol, client_ip)
) STRICT, WITHOUT ROWID;

CREATE INDEX connection_stats_minute ON connection_stats (minute);

CREATE TABLE gaps (
  camera_id TEXT NOT NULL REFERENCES cameras (id) ON DELETE CASCADE,
  protocol  TEXT NOT NULL,
  summary   TEXT NOT NULL,
  client_ip TEXT NOT NULL,
  count     INTEGER NOT NULL,
  first_at  INTEGER NOT NULL,
  last_at   INTEGER NOT NULL,
  PRIMARY KEY (camera_id, protocol, summary, client_ip)
) STRICT, WITHOUT ROWID;

CREATE INDEX gaps_last ON gaps (last_at);

CREATE TABLE camera_logs (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  camera_id  TEXT NOT NULL REFERENCES cameras (id) ON DELETE CASCADE,
  at         INTEGER NOT NULL,
  level      TEXT NOT NULL CHECK (level IN ('DEBUG', 'INFO', 'WARN', 'ERROR')),
  source     TEXT NOT NULL CHECK (source IN ('camera', 'service')),
  msg        TEXT NOT NULL,
  attrs_json TEXT NOT NULL DEFAULT '{}'
) STRICT;

CREATE INDEX camera_logs_by_camera ON camera_logs (camera_id, id);
CREATE INDEX camera_logs_at ON camera_logs (at);

CREATE TABLE camera_metrics (
  camera_id   TEXT NOT NULL REFERENCES cameras (id) ON DELETE CASCADE,
  resolution  INTEGER NOT NULL CHECK (resolution IN (10, 60)),
  at          INTEGER NOT NULL,
  cpu_percent REAL NOT NULL,
  rss_bytes   INTEGER NOT NULL,
  clients     INTEGER NOT NULL,
  bytes_in    INTEGER NOT NULL,
  bytes_out   INTEGER NOT NULL,
  requests    INTEGER NOT NULL,
  PRIMARY KEY (camera_id, resolution, at)
) STRICT, WITHOUT ROWID;

CREATE INDEX camera_metrics_at ON camera_metrics (resolution, at);

-- +goose Down

DROP TABLE camera_metrics;
DROP TABLE camera_logs;
DROP TABLE gaps;
DROP TABLE connection_stats;
DROP TABLE request_stats;
