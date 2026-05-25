-- Multilingual search backbone.
--
-- Phase 4 search work — extends the existing FTS path (migration 019
-- added files.content_text) with:
--
--   1. unaccent: accent-folding ("café" → "cafe"). Strips diacritics
--      so a French user searching "cafe" finds "café" and vice versa.
--      Required for any FTS dictionary path that isn't language-aware
--      enough to do it itself, and for the trigram path which compares
--      raw bytes.
--
--   2. pg_trgm: trigram index for CJK + fuzzy fallback. Postgres' FTS
--      dictionaries split on whitespace which is useless for Chinese
--      / Japanese / Korean (no word boundaries). pg_trgm indexes
--      every 3-character window, so a substring of a CJK phrase still
--      matches the indexed full string, and "reportt" (typo) still
--      similarity-matches "report" above the default 0.3 threshold.
--
--   3. workspaces.search_language: per-workspace knob to select the
--      FTS dictionary used for stemming. Default 'simple' (no
--      stemming, works for every language by trigram). An admin sets
--      this via PUT /api/admin/workspace/search-language to pick a
--      language-specific dictionary (english, french, german, etc.)
--      so "running" matches "ran" / "runs" via Snowball stemmer.
--
--   4. Two GIN indexes per searched column (files.name,
--      files.content_text, folders.name): one trigram and one
--      tsvector('simple', ...). The search service picks the cheaper
--      path based on query characteristics (FTS for word-boundary
--      languages, trigram for CJK / fuzzy). Both indexes coexist so
--      strategy switching at query time doesn't pay an index-rebuild
--      cost.
--
-- # Index expression immutability
--
-- Postgres' unaccent() is marked STABLE (not IMMUTABLE) in the
-- catalog because it consults a configuration dictionary that an
-- operator can rebuild. Index expressions require IMMUTABLE
-- functions, so we wrap it in immutable_unaccent() which fixes the
-- dictionary ('public.unaccent') and re-declares the wrapper as
-- IMMUTABLE. This is the standard pattern (see PostgreSQL wiki:
-- https://wiki.postgresql.org/wiki/Fuzzystrmatch_-_an_implementation
-- of_some_string-comparing_algorithms#unaccent) and is safe as long
-- as no operator alters the unaccent dictionary in place — if that
-- ever happens, the indexes would need a REINDEX.
--
-- # CONCURRENTLY
--
-- The migration runner wraps each .up.sql in a single transaction
-- (internal/database/postgres.go:Migrate uses pool.Begin → tx.Exec),
-- and CREATE INDEX CONCURRENTLY is forbidden inside a transaction
-- block. Plain CREATE INDEX is used here: the migration is run
-- ahead of pod rollout by the migrate Job, at which point the table
-- is either empty (fresh deploy) or briefly write-locked
-- (production) — a short hold is acceptable. If a future operator
-- needs CONCURRENTLY for a hot-deploy migration, they can run the
-- CREATE INDEX manually and then mark this migration applied by
-- INSERTing into schema_migrations.
CREATE EXTENSION IF NOT EXISTS unaccent;
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- The unaccent dictionary lives in the public schema by default. We
-- pin the dictionary name in the function body so the IMMUTABLE
-- declaration is honest — unaccent(text) (one-arg form) is STABLE
-- because it resolves the dictionary at call time; unaccent(regdic,
-- text) is the form that lets us pin the dictionary, and wrapping
-- it in a SQL function lets the planner inline the call.
CREATE OR REPLACE FUNCTION immutable_unaccent(text) RETURNS text AS $$
  SELECT public.unaccent('public.unaccent', $1)
$$ LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT;

ALTER TABLE workspaces
  ADD COLUMN IF NOT EXISTS search_language TEXT NOT NULL DEFAULT 'simple';

-- Trigram indexes for files.name + files.content_text + folders.name.
-- Used for CJK + fuzzy paths. The expression is unaccented so an
-- accented query string ("café") and an accented stored value
-- normalize to the same trigrams before comparison.
CREATE INDEX IF NOT EXISTS idx_files_trgm_name
  ON files USING gin ((immutable_unaccent(name)) gin_trgm_ops)
  WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_files_trgm_content
  ON files USING gin ((immutable_unaccent(COALESCE(content_text, ''))) gin_trgm_ops)
  WHERE deleted_at IS NULL AND content_text IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_folders_trgm_name
  ON folders USING gin ((immutable_unaccent(name)) gin_trgm_ops)
  WHERE deleted_at IS NULL;

-- FTS index using the 'simple' dictionary on the same unaccented
-- expression. The 'simple' path is the fallback used for the
-- default search_language; language-specific dictionaries are
-- consulted via inline to_tsvector calls in the search query and
-- don't get their own dedicated indexes (the trigram path is the
-- only one fast enough to back that variety without bloating disk
-- usage by 16× — one GIN tsvector per supported language).
CREATE INDEX IF NOT EXISTS idx_files_fts_unaccent_simple
  ON files USING gin (
    to_tsvector('simple', immutable_unaccent(name || ' ' || COALESCE(content_text, '')))
  )
  WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_folders_fts_unaccent_simple
  ON folders USING gin (
    to_tsvector('simple', immutable_unaccent(name))
  )
  WHERE deleted_at IS NULL;
