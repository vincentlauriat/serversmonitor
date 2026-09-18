CREATE TABLE azure_resources (
  id                 TEXT PRIMARY KEY,   -- ARM resource id, lowercased
  name               TEXT NOT NULL,
  type               TEXT NOT NULL,
  resource_group     TEXT NOT NULL,
  location           TEXT NOT NULL,
  kind               TEXT NOT NULL DEFAULT '',
  sku                TEXT NOT NULL DEFAULT '',
  state              TEXT,               -- NULL = not read, never "unknown"
  provisioning_state TEXT NOT NULL DEFAULT '',
  host               TEXT NOT NULL DEFAULT '',
  tags               TEXT NOT NULL DEFAULT '{}',
  first_seen         TEXT NOT NULL,
  last_seen          TEXT NOT NULL,
  deleted_at         TEXT                -- NULL = still there
) WITHOUT ROWID;

CREATE INDEX azure_resources_group ON azure_resources(resource_group, id);

-- No foreign key to azure_resources, deliberately: a resource deleted before
-- ServersMonitor first ran still has cost rows, and they must still be stored.
-- Dropping them would understate the bill.
CREATE TABLE azure_costs (
  resource_id TEXT NOT NULL,
  period      TEXT NOT NULL,             -- "2026-09"
  amount      REAL NOT NULL,
  currency    TEXT NOT NULL,
  as_of       TEXT NOT NULL,
  PRIMARY KEY (resource_id, period)
) WITHOUT ROWID;

CREATE TABLE azure_sync (
  scope      TEXT PRIMARY KEY,           -- "inventory" | "cost"
  ok         INTEGER NOT NULL,
  message    TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  ended_at   TEXT NOT NULL
) WITHOUT ROWID;
