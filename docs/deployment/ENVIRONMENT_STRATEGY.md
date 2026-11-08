# Environment strategy

Written 2026-11-08. Goal: real user data lives only in production, and nothing automated ever writes to it.

## Environments

| Environment | Database | Identity | Who writes | Credentials live in |
|---|---|---|---|---|
| **Test (automated)** | Throwaway PostgreSQL 18: GitHub Actions service container, or any local PostgreSQL | `cmd/devauth` (loopback test issuer) | `go test`, Playwright | Nowhere: test-only roles and passwords created by the tests |
| **Local** | Local PostgreSQL, **or** the Neon `dev` branch | `cmd/devauth`, or the `dev` branch's Neon Auth | One developer | `jarlore-be/.env`, `jarlore-fe/.env.local` (git-ignored) |
| **Dev / staging** | Neon branch `dev` (child of `production`) | Neon Auth on `dev` (its own Auth URL, its own users) | Developers, manual staging smoke tests | Same local files; a future staging deploy uses its own Secret Manager secrets |
| **Production** | Neon branch `production` (project `jarlore-db`, `aws-ap-southeast-1`) | Neon Auth on `production` | Real users, through the deployed API only | Secret Manager (Cloud Run); locally only `jarlore-be/.env.production` for migrations |

Rules:

1. **Tests never use Neon.** `go test` needs `JARLORE_TEST_DATABASE_URL` pointing at a server where it may create roles and databases; CI uses the PostgreSQL 18 service container. Playwright needs `E2E_DATABASE_URL` and refuses a `*.neon.tech` host unless `E2E_NEON_BRANCH_IS_DISPOSABLE=1`, and any host listed in `JARLORE_PRODUCTION_DB_HOSTS`.
2. **CI has no database secrets.** Both workflows use only service containers and the local test issuer.
3. **`.env` is development.** The default env file points at the `dev` branch (or local PostgreSQL). Production connection strings are kept apart in `.env.production` (git-ignored by `.env.*`) and loaded only on purpose, e.g. to run migrations:
   ```bash
   set -a; . ./.env.production; set +a; go run ./cmd/migrate up
   ```
   Variable names are the same in every environment (`DATABASE_URL`, `DIRECT_DATABASE_URL`); which file you load decides the target. Nothing reads `PRODUCTION_*` variables.
4. **The API refuses a privileged role.** Whatever the environment, `cmd/api` checks at startup that `DATABASE_URL`'s role is not a superuser, has no `BYPASSRLS`, cannot `SET ROLE` to one, and does not own the RLS-protected tables (`internal/db/guard.go`). An owner URL pasted into `DATABASE_URL` stops the API before it listens.
5. **Branches have separate identities.** Each Neon branch has its own Neon Auth URL; a token from `dev` is rejected by an API configured for `production` (different issuer and keys).

## Creating the `dev` branch: why from production, and when not to

Neon branches are copy-on-write clones of their parent's data **and** its `neon_auth` users. Options considered:

| Option | Result | Verdict |
|---|---|---|
| Branch `dev` from `production` (current data) | Schema, seed data, roles (`jarlore_api` with its password) and the current rows are copied | **Chosen now**: on 2026-11-08 production holds only test data (inventory below) and **zero** Neon Auth users. Nothing real is cloned. |
| Schema-only branch | Tables without rows, including an empty `goose_db_version` and no seed data | Rejected: goose would try to re-apply migrations, and the reference data (fermentation types, system measurements) would be missing. |
| Separate Neon project | Clean, but a second Neon Auth setup and another 100 CU-hour budget to watch | Not needed while production has no real users. |

**Once real users exist, never branch from production's current state for development.** Instead either (a) reset `dev` from production at a point in time before any real user signed up, or (b) create a fresh project/branch, run `cmd/migrate up` on it, create `jarlore_api`, and `GRANT jarlore_app TO jarlore_api`, as in NEON_SETUP.md.

### Steps (founder, Neon console)

1. Branches → **Create branch**: name `dev`, parent `production`, "Current point in time". Free plan: counts toward 10 branches and the project's CU-hours; scale to zero applies.
2. Open `dev` → **Auth** and copy its Auth URL (it differs from production's).
3. Local files:
   - `jarlore-be/.env`: `DATABASE_URL` = the `dev` branch's **pooled** host with role `jarlore_api` (same password as production, copied with the branch; rotate it on `dev` if you want them independent), `DIRECT_DATABASE_URL` = `dev` direct host with `neondb_owner`, `AUTH_ISSUER`/`AUTH_JWKS_URL` from the `dev` Auth URL.
   - `jarlore-fe/.env.local`: `NEON_AUTH_BASE_URL` = the `dev` Auth URL.
   - Move the production values into `jarlore-be/.env.production`.

## Production data inventory (read-only, 2026-11-08)

Queried as the owner role with `SELECT` statements only. Nothing was modified.

| What | Rows | Owner | Origin |
|---|---|---|---|
| `neon_auth.user` | **0** | — | No one has signed up |
| `profiles` | 2 | `00000000-0000-4000-8000-000000000001` (devauth "demo"), `…0002` (devauth "other") | Created by development sign-in |
| `batches` | 3: `KB-001` "blak tea base" (active, F2), `KB-002` "Ginger kombucha" (finished), `KB-003` "Ginger kombucha" (active, F2) | demo | KB-001 by the founder's manual test; KB-002/KB-003 by the Playwright acceptance run and the API security probes |
| `bottles` | 6 | demo | Same |
| `batch_logs` / `batch_measurements` | 11 / 3 | demo | Same (includes one note "idempotency probe") |
| `recipes` / `recipe_versions` | 1 ("Ginger juice bottle") / 2 | demo | Acceptance run |
| `reminders` | 12 | demo | Created by the batches above |
| `idempotency_keys` | 18 | demo | Writes above |
| `subscriptions` / `billing_events` | 0 / 0 | — | — |

Every row belongs to the two development-issuer accounts, which can never sign in through Neon Auth (their tokens are signed by a local key production does not trust). **Proposed cleanup, not executed, needs the founder's approval:** delete these rows (child tables first, or `DELETE FROM profiles` for the two IDs after deleting their batches and recipes) with the owner role in a single transaction, then verify counts. Until then they are inert.
