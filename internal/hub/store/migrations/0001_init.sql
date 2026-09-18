CREATE TABLE hosts (
  id            INTEGER PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  token_hash    TEXT NOT NULL UNIQUE,
  status        TEXT NOT NULL DEFAULT 'never_seen',   -- online | offline | never_seen
  last_seen     TEXT,
  os            TEXT NOT NULL DEFAULT '',
  arch          TEXT NOT NULL DEFAULT '',
  hostname      TEXT NOT NULL DEFAULT '',
  agent_version TEXT NOT NULL DEFAULT '',
  cores         INTEGER NOT NULL DEFAULT 0,
  mem_total     INTEGER NOT NULL DEFAULT 0,
  muted         INTEGER NOT NULL DEFAULT 0,
  created_at    TEXT NOT NULL
);

-- Raw samples. NULL means "not collected", never zero.
CREATE TABLE samples (
  host_id      INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
  at           TEXT NOT NULL,
  cpu          REAL,
  mem_used     INTEGER, mem_total INTEGER, swap_used INTEGER, swap_total INTEGER,
  load1 REAL, load5 REAL, load15 REAL,
  uptime       INTEGER,
  net_sent_bps REAL, net_recv_bps REAL,
  disks        TEXT,   -- JSON array of proto.Disk
  temps        TEXT,   -- JSON array of proto.Temp
  PRIMARY KEY (host_id, at)
) WITHOUT ROWID;

-- Aggregates share one shape: avg/min/max for scalars, last disks JSON, max temps JSON.
CREATE TABLE samples_10m (
  host_id INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
  at TEXT NOT NULL,
  cpu_avg REAL, cpu_min REAL, cpu_max REAL,
  mem_used_avg REAL, mem_used_min REAL, mem_used_max REAL, mem_total REAL, swap_used_avg REAL,
  load1_avg REAL, load5_avg REAL, load15_avg REAL,
  net_sent_bps_avg REAL, net_sent_bps_max REAL, net_recv_bps_avg REAL, net_recv_bps_max REAL,
  disks TEXT, temps TEXT,
  PRIMARY KEY (host_id, at)
) WITHOUT ROWID;
-- samples_1h and samples_1d repeat the DDL on purpose: CREATE TABLE ... AS SELECT
-- would drop the foreign key, the primary key and every NOT NULL, so DeleteHost
-- would stop cascading into them.
CREATE TABLE samples_1h (
  host_id INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
  at TEXT NOT NULL,
  cpu_avg REAL, cpu_min REAL, cpu_max REAL,
  mem_used_avg REAL, mem_used_min REAL, mem_used_max REAL, mem_total REAL, swap_used_avg REAL,
  load1_avg REAL, load5_avg REAL, load15_avg REAL,
  net_sent_bps_avg REAL, net_sent_bps_max REAL, net_recv_bps_avg REAL, net_recv_bps_max REAL,
  disks TEXT, temps TEXT,
  PRIMARY KEY (host_id, at)
) WITHOUT ROWID;
CREATE TABLE samples_1d (
  host_id INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
  at TEXT NOT NULL,
  cpu_avg REAL, cpu_min REAL, cpu_max REAL,
  mem_used_avg REAL, mem_used_min REAL, mem_used_max REAL, mem_total REAL, swap_used_avg REAL,
  load1_avg REAL, load5_avg REAL, load15_avg REAL,
  net_sent_bps_avg REAL, net_sent_bps_max REAL, net_recv_bps_avg REAL, net_recv_bps_max REAL,
  disks TEXT, temps TEXT,
  PRIMARY KEY (host_id, at)
) WITHOUT ROWID;

CREATE TABLE containers (
  host_id      INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  image        TEXT NOT NULL DEFAULT '',
  status       TEXT NOT NULL DEFAULT '',
  cpu          REAL, mem_used INTEGER, net_sent_bps REAL, net_recv_bps REAL,
  updated_at   TEXT NOT NULL,
  PRIMARY KEY (host_id, name)
) WITHOUT ROWID;

CREATE TABLE container_samples (
  host_id INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  at TEXT NOT NULL,
  cpu REAL, mem_used INTEGER, net_sent_bps REAL, net_recv_bps REAL,
  PRIMARY KEY (host_id, name, at)
) WITHOUT ROWID;

CREATE TABLE alert_rules (
  id        INTEGER PRIMARY KEY,
  host_id   INTEGER REFERENCES hosts(id) ON DELETE CASCADE,  -- NULL = all hosts
  metric    TEXT NOT NULL,        -- cpu | memory | disk | load | temperature | bandwidth
  threshold REAL NOT NULL,
  duration_sec INTEGER NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE alert_events (
  id       INTEGER PRIMARY KEY,
  rule_id  INTEGER NOT NULL,      -- 0 = implicit status rule
  host_id  INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
  metric   TEXT NOT NULL,
  kind     TEXT NOT NULL,         -- fired | resolved
  value    REAL NOT NULL,
  at       TEXT NOT NULL
);
CREATE INDEX alert_events_host_at ON alert_events(host_id, at);
CREATE INDEX alert_events_rule_host ON alert_events(rule_id, host_id, id);

CREATE TABLE users (
  id            INTEGER PRIMARY KEY,
  email         TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  created_at    TEXT NOT NULL
);

CREATE TABLE sessions (
  token      TEXT PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at TEXT NOT NULL
) WITHOUT ROWID;

CREATE TABLE settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
) WITHOUT ROWID;

INSERT INTO settings(key, value) VALUES
  ('agent_interval_sec', '10'),
  ('retention_raw_hours', '24'),
  ('retention_10m_days', '30'),
  ('retention_1h_days', '365');
