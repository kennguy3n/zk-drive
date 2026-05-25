-- Drop the multilingual search indexes and per-workspace language
-- column. The unaccent / pg_trgm extensions are LEFT INSTALLED — they
-- are cheap, schema-level resources and other features (current or
-- future) may rely on them. A migration that genuinely needs them
-- removed should DROP EXTENSION explicitly.
DROP INDEX IF EXISTS idx_files_fts_unaccent_simple;
DROP INDEX IF EXISTS idx_folders_fts_unaccent_simple;
DROP INDEX IF EXISTS idx_files_trgm_name;
DROP INDEX IF EXISTS idx_files_trgm_content;
DROP INDEX IF EXISTS idx_folders_trgm_name;
DROP INDEX IF EXISTS idx_file_tags_trgm_tag;

ALTER TABLE workspaces DROP COLUMN IF EXISTS search_language;

DROP FUNCTION IF EXISTS immutable_unaccent(text);
