-- ARM's own casing for the resource id. The join key in `id` went through
-- NormalizeID and is lowercase; the action URL is built from this one instead,
-- so the first real action is not a bet on every ARM path segment being
-- case-insensitive. Empty for rows written before this migration; the next
-- inventory sync fills them in.
ALTER TABLE azure_resources ADD COLUMN arm_id TEXT NOT NULL DEFAULT '';

CREATE TABLE azure_actions (
  id            INTEGER PRIMARY KEY,
  resource_id   TEXT NOT NULL,   -- lowercased, as in azure_resources.id
  resource_name TEXT NOT NULL,   -- copied, not joined: see below
  action        TEXT NOT NULL,   -- start | stop | restart
  status        TEXT NOT NULL,   -- pending | running | succeeded | failed | interrupted
  requested_at  TEXT NOT NULL,
  finished_at   TEXT,
  error         TEXT NOT NULL DEFAULT '',
  state_before  TEXT,            -- NULL = it was not known either
  state_after   TEXT             -- NULL = not read back
);

-- resource_name is copied and there is no foreign key, for the same reason
-- azure_costs has neither: a resource deleted next week must not erase the
-- record that it was stopped today.

CREATE INDEX azure_actions_recent ON azure_actions(requested_at DESC, id DESC);

-- One action in flight per resource, enforced by the database rather than by a
-- check-then-insert two requests can interleave through.
CREATE UNIQUE INDEX azure_actions_in_flight
  ON azure_actions(resource_id) WHERE status IN ('pending','running');
