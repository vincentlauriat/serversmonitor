-- Lot 8: what the subnet's own properties said about reaching the internet,
-- read in the same VNet GET that finds the region, before anything is created.
-- A VM in a subnet with no outbound access is created anyway (a warning, not a
-- refusal), so the card has to be able to say why its agent never called in.
-- '' is a run from before this migration, or one refused before the read.
ALTER TABLE azure_provisions ADD COLUMN outbound        TEXT NOT NULL DEFAULT '';
ALTER TABLE azure_provisions ADD COLUMN outbound_detail TEXT NOT NULL DEFAULT '';
