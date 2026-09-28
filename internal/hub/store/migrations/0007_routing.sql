-- Lot 7: which channels a rule notifies. NULL is every enabled channel, now
-- and when one is enabled later, so every rule that existed before this
-- migration behaves as it did. '' is no channel at all: the rule still fires
-- and shows, it just tells nobody. Otherwise a comma list, e.g. 'smtp,teams'.
ALTER TABLE alert_rules ADD COLUMN channels TEXT;
