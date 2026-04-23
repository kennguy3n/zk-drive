ALTER TABLE files DROP CONSTRAINT IF EXISTS fk_files_current_version;
DROP TABLE IF EXISTS file_versions;
DROP TABLE IF EXISTS files;
