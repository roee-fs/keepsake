-- The two roles the migration expects. It grants okf_app behind an existence
-- check, so a missing role would leave the app ungranted rather than fail.
CREATE ROLE okf_owner LOGIN PASSWORD 'owner';
CREATE ROLE okf_app LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD 'app';
GRANT CREATE ON DATABASE keepsake TO okf_owner;
