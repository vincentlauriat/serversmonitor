-- Lot 11: several people, two roles, two ways in.
--
-- role: admin can change anything; viewer sees everything and changes
-- nothing. Every account that exists before this migration is the local
-- admin created at setup, so admin is the default.
--
-- provider: local signs in with the password in password_hash; entra signs
-- in through Microsoft Entra ID and has no password at all (password_hash is
-- ''), so the password route must refuse it outright rather than compare a
-- hash. The row is the allow list: an Entra account nobody added is refused.
ALTER TABLE users ADD COLUMN role     TEXT NOT NULL DEFAULT 'admin';
ALTER TABLE users ADD COLUMN provider TEXT NOT NULL DEFAULT 'local';
