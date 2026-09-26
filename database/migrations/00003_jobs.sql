-- Background jobs (D72, D74): renditions and imports now, captures and
-- backups later. State, steps, checkpoints and questions are stored, so a
-- job survives the browser closing and resumes after a node restart.

-- +goose Up

CREATE TABLE jobs (
  id              TEXT PRIMARY KEY,
  type            TEXT NOT NULL,
  title           TEXT NOT NULL DEFAULT '',
  status          TEXT NOT NULL CHECK (status IN ('queued', 'running', 'waiting', 'completed', 'failed', 'canceled', 'interrupted')),
  progress        REAL NOT NULL DEFAULT 0,  -- 0 to 1
  step            TEXT NOT NULL DEFAULT '',
  params_json     TEXT NOT NULL DEFAULT '{}',
  checkpoint_json TEXT NOT NULL DEFAULT '{}',
  question_json   TEXT,                     -- the question waiting for an answer
  result_json     TEXT NOT NULL DEFAULT '{}',
  dedupe_key      TEXT,                     -- a second request for the same work joins the open job
  created_by      TEXT NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL,
  started_at      INTEGER,
  finished_at     INTEGER,
  error           TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX jobs_status ON jobs (status);
CREATE UNIQUE INDEX jobs_open_key ON jobs (dedupe_key)
WHERE dedupe_key IS NOT NULL AND status IN ('queued', 'running', 'waiting', 'interrupted');

CREATE TABLE job_events (
  job_id       TEXT NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
  seq          INTEGER NOT NULL,
  at           INTEGER NOT NULL,
  kind         TEXT NOT NULL, -- status | step | log | question | answer
  payload_json TEXT NOT NULL,
  PRIMARY KEY (job_id, seq)
) STRICT;

-- +goose Down

DROP TABLE job_events;
DROP INDEX jobs_open_key;
DROP INDEX jobs_status;
DROP TABLE jobs;
