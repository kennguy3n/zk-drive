-- Reverse of 026_user_totp.up.sql.
--
-- Drop order: recovery codes (which FK to users), then credentials
-- (which also FK to users), then the workspaces.mfa_required column.
-- The partial index is implicitly dropped with its parent table.

DROP TABLE IF EXISTS user_totp_recovery_codes;
DROP TABLE IF EXISTS user_totp_credentials;

ALTER TABLE workspaces DROP COLUMN IF EXISTS mfa_required;
