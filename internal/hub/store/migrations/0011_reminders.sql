-- Lot 12: reminders and quiet hours.
--
-- remind_minutes: how often a rule's alert is repeated while it stays fired;
-- 0, the default, never, so every existing rule behaves as before.
--
-- deliveries.reminder: a reminder is a delivery of the fired event it
-- repeats, never a row of its own in alert_events or azure_guardrail_events.
-- Those journals are read as "the last row is the current state", and a
-- reminder row there would make a firing alert stop looking like it fires.
-- Delivered against the fired event, it also counts for FiredChannels: a
-- channel that got the reminder is owed the resolution.
--
-- A delivery may now also be in state 'held': owed to a channel in its quiet
-- hours, released as 'pending' when they end. Not a schema change, a value.
ALTER TABLE alert_rules ADD COLUMN remind_minutes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE deliveries  ADD COLUMN reminder       INTEGER NOT NULL DEFAULT 0;
