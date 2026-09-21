-- Creating a VM is several calls creating several resources, which is why this
-- is not a reuse of azure_actions: an action is one call on one resource that
-- already exists. A provision has to be able to name each resource it created,
-- individually, because a run that fails halfway leaves some of them behind and
-- the page offers a Delete button beside each one.
CREATE TABLE azure_provisions (
  id            INTEGER PRIMARY KEY,
  name          TEXT NOT NULL,   -- the VM name that was asked for
  host_id       INTEGER,         -- the host row created first; see below
  status        TEXT NOT NULL,   -- pending | running | succeeded | failed | interrupted
  requested_at  TEXT NOT NULL,
  finished_at   TEXT,
  error         TEXT NOT NULL DEFAULT ''
);

-- host_id has no foreign key, deliberately, like azure_costs and
-- azure_actions before it: deleting the host row must not erase the record
-- that a machine was once created for it. The token that went into cloud-init
-- is NOT stored here, or anywhere else — the host row already holds it.

CREATE TABLE azure_provision_resources (
  id           INTEGER PRIMARY KEY,
  provision_id INTEGER NOT NULL REFERENCES azure_provisions(id) ON DELETE CASCADE,
  arm_id       TEXT NOT NULL,   -- ARM's own casing: the delete URL is built from it
  kind         TEXT NOT NULL,   -- nic | vm | disk
  created_at   TEXT NOT NULL,
  deleted_at   TEXT             -- set when a person deletes it; never by the hub on its own
);

CREATE INDEX azure_provision_resources_by_run ON azure_provision_resources(provision_id, id);
CREATE INDEX azure_provisions_recent ON azure_provisions(requested_at DESC, id DESC);

-- One provision in flight per name, enforced by the database rather than by a
-- check-then-insert two requests can interleave through. Two runs of the same
-- name would PUT the same resource ids and race each other into a state
-- neither of them recorded.
CREATE UNIQUE INDEX azure_provisions_in_flight
  ON azure_provisions(name) WHERE status IN ('pending','running');
