-- The two roles the migration expects. It grants keepsake_app behind an existence
-- check, so a missing role would leave the app ungranted rather than fail.
CREATE ROLE keepsake_owner LOGIN PASSWORD 'owner';
CREATE ROLE keepsake_app LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD 'app';
GRANT CREATE ON DATABASE keepsake TO keepsake_owner;
