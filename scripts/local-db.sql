-- Local PostgreSQL set up like Neon: a non-superuser owner role runs migrations,
-- and the API logs in as a separate role that only gets jarlore_app's privileges.
-- Run as a superuser (psql -U postgres -f scripts/local-db.sql), then:
--   DIRECT_DATABASE_URL=postgres://jarlore_owner:jarlore_owner_local@localhost:5432/jarlore?sslmode=disable go run ./cmd/migrate up
--   psql "postgres://jarlore_owner:jarlore_owner_local@localhost:5432/jarlore" -c "GRANT jarlore_app TO jarlore_api"
-- Local passwords only. Never reuse them anywhere else.
CREATE ROLE jarlore_owner LOGIN CREATEROLE PASSWORD 'jarlore_owner_local';
CREATE ROLE jarlore_api LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD 'jarlore_api_local';
CREATE DATABASE jarlore OWNER jarlore_owner;
