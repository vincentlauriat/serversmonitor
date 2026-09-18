CREATE TABLE deliveries (
  id         INTEGER PRIMARY KEY,
  event_id   INTEGER NOT NULL REFERENCES alert_events(id) ON DELETE CASCADE,
  channel    TEXT NOT NULL,            -- smtp | webhook | teams
  state      TEXT NOT NULL,            -- pending | sent | failed
  attempts   INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX deliveries_state ON deliveries(state, id);
CREATE INDEX deliveries_channel ON deliveries(channel, id);
