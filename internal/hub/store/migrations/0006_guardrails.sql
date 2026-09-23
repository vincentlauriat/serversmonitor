-- Lot 6. Guardrail alerts have no host, so they get their own append-only
-- journal rather than a fake host in alert_events. Deliveries must be able to
-- point at either journal, and SQLite cannot relax a NOT NULL column in
-- place, so the table is rebuilt.

CREATE TABLE azure_guardrail_events (
  id      INTEGER PRIMARY KEY,
  subject TEXT NOT NULL,        -- 'budget' | lowercased ARM id
  rule    TEXT NOT NULL,        -- budget_threshold | budget_projection | resource_share | orphan | schedule_failed
  detail  TEXT NOT NULL DEFAULT '',
  kind    TEXT NOT NULL,        -- fired | resolved
  value   REAL NOT NULL,
  at      TEXT NOT NULL
);
CREATE INDEX azure_guardrail_events_key ON azure_guardrail_events(subject, rule, detail, id);

CREATE TABLE azure_schedules (
  resource_id     TEXT PRIMARY KEY,   -- lowercased, as azure_resources.id
  off_windows     TEXT NOT NULL,      -- JSON: [{"days":[1,2,3,4,5],"from":"20:00","to":"07:00"}]
  enabled         INTEGER NOT NULL DEFAULT 1,
  last_boundary   TEXT,               -- RFC 3339 UTC of the last boundary acted on
  last_applied_at TEXT,
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL
) WITHOUT ROWID;

ALTER TABLE azure_resources ADD COLUMN orphan_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE azure_resources ADD COLUMN orphan_since  TEXT;
ALTER TABLE azure_actions   ADD COLUMN origin        TEXT NOT NULL DEFAULT 'user';

CREATE TABLE deliveries_new (
  id                 INTEGER PRIMARY KEY,
  event_id           INTEGER REFERENCES alert_events(id) ON DELETE CASCADE,
  guardrail_event_id INTEGER REFERENCES azure_guardrail_events(id) ON DELETE CASCADE,
  channel            TEXT NOT NULL,
  state              TEXT NOT NULL,
  attempts           INTEGER NOT NULL DEFAULT 0,
  last_error         TEXT NOT NULL DEFAULT '',
  created_at         TEXT NOT NULL,
  updated_at         TEXT NOT NULL,
  CHECK ((event_id IS NULL) <> (guardrail_event_id IS NULL))
);
INSERT INTO deliveries_new (id, event_id, channel, state, attempts, last_error, created_at, updated_at)
  SELECT id, event_id, channel, state, attempts, last_error, created_at, updated_at FROM deliveries;
DROP TABLE deliveries;
ALTER TABLE deliveries_new RENAME TO deliveries;
CREATE INDEX deliveries_state   ON deliveries(state, id);
CREATE INDEX deliveries_channel ON deliveries(channel, id);
