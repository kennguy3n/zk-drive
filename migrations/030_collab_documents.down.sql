-- Drop in reverse dependency order so the FKs don't block. RLS
-- policies are dropped automatically with the table.
DROP TABLE IF EXISTS document_deltas;
DROP TABLE IF EXISTS documents;
