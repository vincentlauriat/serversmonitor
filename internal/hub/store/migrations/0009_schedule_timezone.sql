-- Lot 9: a stop schedule may carry its own IANA time zone. '' is the hub-wide
-- zone (the azure_timezone setting), read at every evaluation, so every
-- schedule that existed before this migration keeps following that setting.
ALTER TABLE azure_schedules ADD COLUMN timezone TEXT NOT NULL DEFAULT '';
