-- Reverse 039_platform_api_key_lookup.
DROP INDEX IF EXISTS idx_platform_api_keys_lookup;
ALTER TABLE platform_api_keys DROP COLUMN IF EXISTS key_lookup;
