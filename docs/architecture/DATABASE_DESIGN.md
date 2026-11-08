# Database Design

Neon PostgreSQL, SQL first. The schema lives in [`db/migrations`](../../db/migrations) (goose), queries in [`db/queries`](../../db/queries) (sqlc → `internal/db/sqlc`). This document explains the shape and the guarantees; the SQL is the source of truth.

## 1. Tables

| Table | Holds | Mutability |
|---|---|---|
| `fermentation_types` | Kombucha, water kefir, custom: stages, typical days, code prefix, blank-batch defaults | Seed data, read-only for the API |
| `profiles` | One row per user, id = JWT `sub`; unit and time zone preferences | Mutable |
| `measurement_definitions` | System (`user_id` null: ph, temp, brix, sg) and custom definitions. Kind: numeric, scale, boolean, text | Mutable label; archived, not deleted, once used |
| `recipes` | A named recipe. `scope`: `batch` (base F1 recipe) or `bottle` (F2 bottle recipe) | Name and archive only |
| `recipe_versions` | Immutable versions v1..vN: params, base volume, default bottles (batch scope) or `bottle_spec` (bottle scope), parent, source batch/bottle, changed fields | **Immutable** |
| `recipe_ingredients` | Ingredients of a batch-scope version | **Immutable** |
| `batches` | One F1 (or single-stage) ferment: code, status, stage, dates, volume, params, pinned version, frozen snapshot, parent batch, hypothesis, changed fields, rating | Locked once finished/discarded (except name, vessel, reference flag, room temp, archive) |
| `batch_ingredients` | What actually went into the batch | Append-only |
| `bottles` | One F2 experiment: label, flavour, additions, priming sugar, headspace, size, status, dates, pinned bottle version, frozen snapshot, parent bottle, hypothesis, changed fields, best flag, rating | Frozen once finished/discarded (a closed unrated bottle may be rated once; best flag may change) |
| `batch_logs` | Journal: note, taste, burp, measure, stage_change, feed, issue; optionally about one bottle | Append-only, soft delete only |
| `batch_measurements` | Readings in a log entry: value as entered + canonical value | Append-only |
| `reminders` | Taste check, burp, chill, custom | Mutable status |
| `batch_code_counters` | Per-user, per-prefix counter for `KB-001` codes | Internal |
| `idempotency_keys` | Stored responses for retried writes (24 h) | Insert/delete |
| `subscriptions`, `billing_events` | Plan state from the Merchant of Record (not wired yet) | Webhook processing only |

## 2. Relationships that the database enforces

Every user-owned row carries `user_id`, and children reference parents with composite keys that include it, so a row can never point at another user's data, whatever the application does.

| Rule | Mechanism |
|---|---|
| A bottle belongs to one batch of the same user | `bottles (batch_id, user_id) → batches (id, user_id)` |
| A log about a bottle names a bottle **of the same batch** | `batch_logs (bottle_id, batch_id, user_id) → bottles (id, batch_id, user_id)` |
| A measurement belongs to its log's batch | `batch_measurements (log_id, batch_id, user_id) → batch_logs (id, batch_id, user_id)` |
| A reminder about a bottle names a bottle of that batch | `reminders (bottle_id, batch_id, user_id) → bottles (...)` |
| A version belongs to its recipe, with the recipe's scope and type | `recipe_versions (recipe_id, user_id, scope, fermentation_type_id) → recipes (...)` |
| A parent version is a version **of the same recipe** | `recipe_versions (parent_version_id, recipe_id, user_id) → recipe_versions (id, recipe_id, user_id)` |
| A batch is pinned only to a **batch-scope** version of the same type | `batches (recipe_version_id, user_id, recipe_scope='batch', fermentation_type_id) → recipe_versions (...)` |
| A bottle is pinned only to a **bottle-scope** version | `bottles (recipe_version_id, user_id, recipe_scope='bottle') → recipe_versions (id, user_id, scope)` |
| A measurement uses a system definition or the user's own | Trigger `batch_measurements_definition_check` |
| Labels are unique within a batch; one best bottle per batch | `UNIQUE (batch_id, label)`; partial unique index on `is_reference` |
| Codes are unique per user and never reused | `UNIQUE (user_id, code)`, counter table |
| Version numbers are gap-free | `UNIQUE (recipe_id, version_no)`; numbers come from `UPDATE recipes SET latest_version_no = latest_version_no + 1 ... RETURNING`, which row-locks the recipe |

Check constraints cover states (`finished ⇔ stage done`, `active ⇔ no finished_at`, `finished ⇒ rating`, `reference ⇒ finished`, `archived ⇒ not active`), dates (`finished_at ≥ started_at`, `bottled_at ≥ started_at`), score ranges and value kinds (`exactly one of value_num / value_bool / value_text`). A trigger refuses a bottle that starts before its batch.

## 3. History that can't change

Triggers raise custom SQLSTATEs that the API maps to stable error codes (`internal/db/errors.go`):

| SQLSTATE | Raised when | API code |
|---|---|---|
| `JL001` | Any UPDATE/DELETE on `recipe_versions` or `recipe_ingredients` | 409 `recipe_version_immutable` |
| `JL002` | A batch's or bottle's snapshot changes, or a set version pointer changes | 422 `field_immutable` |
| `JL003` | A closed batch or bottle changes outside the allowed fields; identity fields change; a bottle moves batch | 409 `invalid_state_transition` |
| `JL004` | Editing ingredients, measurements or log entries (beyond a soft delete); deleting batches or bottles | 409 `append_only` |
| `JL005` | A measurement uses another user's definition | 404 |

The only path that may delete is the (future) account-deletion job, which sets `jarlore.purge = on` for its transaction and must run as a role with the needed grants.

## 4. Snapshots and versions

- A batch or bottle created from a version stores `recipe_version_id` **and** `recipe_snapshot` (a self-contained JSON copy: recipe name and scope, version number, base volume, ingredients, params, default bottles or bottle spec). History survives even if relations are reorganised later.
- `changed_fields` on batches, bottles and versions caches the diff keys against the source at creation time.
- A version created by Brew Again or by a changed bottle records `source_batch_id` / `source_bottle_id` of the batch or bottle that created it. These foreign keys are `DEFERRABLE INITIALLY DEFERRED`, because the version and its batch/bottle are inserted in the same transaction.

## 5. Measurements and units

`batch_measurements.unit` keeps what the user entered; `value_canonical` stores the number in the definition's canonical unit (°C, ml, g). Temperature, volume and mass definitions accept any unit of their quantity (22 °C and 71.6 °F are stored as the same canonical value, so comparisons don't show a false difference). Definition `min`/`max` are sanity ranges on the canonical value, not judgements; nothing is ever labelled safe or ready.

## 6. Roles, privileges and row level security

| Role | Who | Privileges |
|---|---|---|
| Owner (`neondb_owner` on Neon; non-superuser, `CREATEROLE`) | Migrations only, over `DIRECT_DATABASE_URL` | Owns the tables. On Neon it has `BYPASSRLS` through `neon_superuser`, so never use it for the API |
| `jarlore_app` (NOLOGIN group, created by migration 00002) | Privilege bundle | Exactly what the API needs, see below |
| API login role (e.g. `jarlore_api`) | The API, over the pooled `DATABASE_URL` | Member of `jarlore_app`; `NOSUPERUSER NOBYPASSRLS`; owns nothing |

`jarlore_app` grants: SELECT on reference data; SELECT/INSERT/UPDATE on mutable user tables; **SELECT/INSERT only** on `recipe_versions`, `recipe_ingredients`, `batch_ingredients`, `batch_measurements`, `batch_logs` (plus `UPDATE (deleted_at)` on logs); **SELECT only** on `subscriptions` (the API can't grant Pro); nothing on `billing_events`; no DELETE except on `idempotency_keys` and own measurement definitions.

RLS is enabled on every user table with `user_id = jarlore_user_id()`, where `jarlore_user_id()` reads the transaction-local `jarlore.user_id`. Without it set, the runtime role sees no rows at all. `FORCE ROW LEVEL SECURITY` is not used: the owner role runs migrations and fixes data, and bypasses RLS on Neon anyway.

## 7. Connections (Neon)

- Runtime: pooled endpoint (`-pooler` host, PgBouncer in transaction mode). pgx's default statement cache works (Neon's PgBouncer supports protocol-level prepared statements); `DB_QUERY_EXEC_MODE=exec` is a tested fallback.
- No session state: the only setting is `set_config(..., is_local => true)` inside each transaction.
- Pool: `MinConns 0`, `MaxConns` from `DB_MAX_CONNS` (default 4), idle connections closed after 1 minute, lifetime 30 minutes, no background queries. The API never keeps Neon awake when there is no traffic.
- A connection that died while the compute was suspended is retried once when nothing was sent yet (`pgconn.SafeToRetry`).

## 8. Migrations

- `go run ./cmd/migrate up` (and `status`). Embedded files, so the binary is self-contained.
- No down migrations: a rollback would drop user data. Fix forward with a new migration.
- Migrations must stay compatible with the running API version during a deploy (add, then use, then remove).

## 9. Tests that prove it

`internal/db/db_test.go` runs as the runtime role and as the owner against a real PostgreSQL:
RLS hides other users' rows (even by id) and blocks writing them; the user setting never leaks between transactions on one pooled connection; the runtime role can't write subscriptions, edit versions, rewrite logs, delete batches or read billing events; triggers stop edits to versions, snapshots and closed experiments even for the owner; composite keys stop cross-batch and cross-user references and scope mismatches.
