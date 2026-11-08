# Neon setup

Facts checked on 2026-11-06 against neon.com docs and pricing. Re-check before relying on limits.

## 1. Create the project

1. Sign up at neon.com and choose the Free plan.
2. Create a project:
   - **Region: AWS Asia Pacific (Singapore), `aws-ap-southeast-1`.** It sits in the same city as Cloud Run `asia-southeast1`, so each query is a short in-city hop. There is no Jakarta region; distance to the founder doesn't matter, distance between the API and the database does (a request runs several queries).
   - Postgres version: the default (18). The schema uses nothing version-specific beyond Postgres 15 (`UNIQUE NULLS NOT DISTINCT`).
   - Database name: `neondb` (default) is fine.
3. Keep the default owner role (`neondb_owner`). It is for migrations only.

Free plan, as published: 100 projects; 10 branches per project; 100 CU-hours of compute a month per project; autoscaling up to 2 CU; 1 GB storage per project; 5 GB egress a month; scale to zero after 5 minutes idle (not configurable on Free). Pooled connections go through PgBouncer in transaction mode (`max_client_conn` 10,000, `max_prepared_statements` 1000, `query_wait_timeout` 120 s).

## 2. Get the two connection strings

In the console, **Connect**:

- **Direct** (no `-pooler` in the host), role `neondb_owner` → `DIRECT_DATABASE_URL`. Used by `cmd/migrate` only.
- **Pooled** (toggle "Connection pooling"; the host contains `-pooler`), role `jarlore_api` (created in step 4) → `DATABASE_URL`. Used by the API.

Use `sslmode=verify-full` (Neon refuses non-TLS connections; `verify-full` also checks the host). `channel_binding=require` is supported too. The API uses pgx v5.12.0 or later, which fixes a `channel_binding` bypass present in v5.9.0 to v5.11.0.

## 3. Run the migrations

```bash
cd jarlore-be
DIRECT_DATABASE_URL='postgres://neondb_owner:...@ep-xxx.ap-southeast-1.aws.neon.tech/neondb?sslmode=verify-full' \
  go run ./cmd/migrate up
go run ./cmd/migrate status   # same env var; lists applied migrations
```

The first connection wakes a suspended compute (usually well under a second). Migration 00002 creates the `jarlore_app` group role with exactly the API's privileges.

## 4. Create the API's login role

Run in the Neon SQL editor **as `neondb_owner`** (do not create the API role in the console: console-created roles join `neon_superuser`, which has `BYPASSRLS`):

```sql
CREATE ROLE jarlore_api LOGIN PASSWORD '<60+ bits of randomness, e.g. openssl rand -base64 24>'
  NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
GRANT jarlore_app TO jarlore_api;
```

Check it:

```sql
SELECT rolname, rolbypassrls, rolsuper FROM pg_roles WHERE rolname IN ('jarlore_api', 'jarlore_app');
-- both rows: rolbypassrls = false, rolsuper = false
```

Put the password only in Secret Manager (see CLOUD_RUN.md). Rotate with `ALTER ROLE jarlore_api PASSWORD '...'` and a new secret version.

## 5. Turn on Neon Auth

1. In the console, open **Auth** and enable it on `main` (and on any `dev` branch you sign in against). Copy the **Auth URL** (`NEON_AUTH_BASE_URL`) exactly as shown.
2. Configure the API (CLOUD_RUN.md): `AUTH_ISSUER` = the Auth URL's **origin** (scheme + host, no path), `AUTH_JWKS_URL` = `<Auth URL>/.well-known/jwks.json`. `AUTH_AUDIENCE` defaults to `AUTH_ISSUER`.
3. Configure the frontend: `NEON_AUTH_BASE_URL` and `NEON_AUTH_COOKIE_SECRET` (32+ characters, `openssl rand -base64 32`), server-side only.
4. Check the API role cannot read auth data (expect `false`):
   ```sql
   SELECT has_schema_privilege('jarlore_api', 'neon_auth', 'USAGE');
   ```
5. Before launch (Neon's auth production checklist): custom SMTP provider (the shared sender is rate-limited and cannot send verification links), our own Google OAuth app, trusted domains, email verification on, "Allow Localhost" off, application name "Jarlore".

Free plan: up to 60,000 monthly active users per project. Neon Auth is offered on AWS regions; confirm it appears for the Singapore project when enabling it.

## 6. Branches for development and tests

- Create a `dev` branch from `main` for local work: same schema, separate data, separate compute (counts toward the project's CU-hours).
- Integration tests need a server where the connecting role can create roles and databases (`JARLORE_TEST_DATABASE_URL`). A throwaway branch's `neondb_owner` works; each test package creates and drops its own database. Local PostgreSQL is cheaper and faster for day-to-day runs.

## 7. Living with scale to zero

- After 5 idle minutes the compute suspends. The next query resumes it; the API's 10-second connect timeout and one safe retry cover it. Expect a slower first request after a quiet period.
- The API keeps no idle connections longer than a minute and sends no background queries, so it never keeps the compute awake.
- `/health/live` never touches the database (Cloud Run probes use it). `/health/ready` does, and wakes the compute: don't point frequent uptime checks at it, or the database never sleeps and burns CU-hours.

## 8. Watching usage

Console → project → **Usage**: compute hours, storage, data transfer. 100 CU-hours a month is about 400 hours of the smallest compute (0.25 CU); a compute that never sleeps would need ~180 CU-hours at 0.25 CU, so scale to zero is what keeps this inside Free.
