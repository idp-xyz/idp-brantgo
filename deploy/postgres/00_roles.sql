-- Roles and database privileges for Brant. Run once per database as a superuser:
--
--   psql -v ON_ERROR_STOP=1 -v db=brant -v migrator_password=... -v app_password=... -f 00_roles.sql
--
-- brant_migrator owns every Brant schema and runs brant-migrate. brant_app runs the API and the
-- worker: no superuser, no BYPASSRLS, no CREATEDB or CREATEROLE, no schema ownership, and never a
-- member of brant_migrator (internal/foundation/database refuses to run otherwise).

SELECT format('CREATE ROLE brant_migrator LOGIN PASSWORD %L NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE',
              :'migrator_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'brant_migrator') \gexec

SELECT format('CREATE ROLE brant_app LOGIN PASSWORD %L NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT',
              :'app_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'brant_app') \gexec

REVOKE ALL ON DATABASE :"db" FROM PUBLIC;
GRANT CONNECT, TEMPORARY ON DATABASE :"db" TO brant_app;
GRANT CONNECT, CREATE, TEMPORARY ON DATABASE :"db" TO brant_migrator;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
