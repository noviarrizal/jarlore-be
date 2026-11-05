# Architecture Decisions

Format: lightweight ADRs. Status values: **Proposed** (needs founder sign-off), **Accepted**, **Superseded**. Date of all entries: 2026-11-05. "Blueprint" refers to the Fermentation Lab blueprint (product now Jarlore).

| ADR | Title | Status |
|---|---|---|
| 001 | Separate HTTP API instead of the blueprint's Next.js + Supabase monolith | Accepted (founder), trade-offs documented |
| 002 | Backend language and framework | **Proposed, open** |
| 003 | Auth provider: Supabase Auth, API verifies JWT via JWKS | Proposed |
| 004 | PostgreSQL host: Supabase (default) or Neon | Proposed |
| 005 | Immutable recipe versions via copy-on-write, frozen batch snapshots | Accepted |
| 006 | Diffs and comparisons computed server-side | Accepted |
| 007 | Cursor pagination | Accepted |
| 008 | RFC 9457 problem+json errors | Accepted |
| 009 | Merchant-of-Record billing behind a provider interface | Accepted |
| 010 | Background jobs on Postgres, no queue or broker | Accepted |
| 011 | Defense-in-depth RLS for the API connection | Proposed |

---

## ADR-001: Separate HTTP API instead of the blueprint's monolith

**Context.** The blueprint chose one Next.js 16 app that reads and writes Supabase directly, using RLS policies, a `create_recipe_version` RPC and triggers for integrity, and Server Actions for writes. The founder has created a separate backend repository (`jarlore-be`), and a separate frontend (`jarlore-fe`) is being built in parallel.

**Decision.** The frontend calls a versioned HTTP JSON API (`/v1`). Only the API talks to PostgreSQL. The API is the authority for ownership, recipe version integrity, batch snapshots and experiment integrity, validation, entitlements, diffs, comparisons and history. OpenAPI (`docs/api/openapi.yaml`) is the contract.

**Consequences.**

| Gains | Costs |
|---|---|
| One place for business rules; FE cannot bypass them | One more deployable, host and on-call surface |
| Contract-first lets FE and BE be built in parallel | Extra network hop (FE server or browser → API → DB); target same region |
| Backend is free of Next.js release churn (monthly security releases noted in the blueprint) | Lose Server Actions' "call the DB from the component" ergonomics; need typed client from OpenAPI |
| Native/mobile clients later reuse the same API | Authorization moves from RLS to application code (mitigated by composite FKs and optional RLS, ADR-011) |
| Database host and auth provider become swappable | CORS, rate limiting and idempotency must be built explicitly |
| Easier to test domain logic in isolation | Two repos to keep in sync (mitigated by generated FE types) |

Blueprint elements that change: RLS is no longer the primary guard; `auth.uid()`-based RPCs move into API transactions; Supabase Edge Functions for reminders are replaced by the API's job runner (ADR-010); `/api/export`, `/api/calendar`, `/api/webhooks/billing` move from Next.js route handlers to the API.

Blueprint elements kept: single Postgres, no microservices, composite FKs, immutability triggers, Resend, MoR billing, PostHog/Sentry, `.ics` feed.

---

## ADR-002: Backend language and framework

**Status: Proposed, open.** Nothing is scaffolded until this is decided.

**Context.** Solo developer, frontend-leaning, fluent in TypeScript. Workload is CRUD-heavy with transactional rules, a pure diff engine, a few background jobs (email digests, ZIP export, webhooks), and low traffic (≤ 10,000 users in the blueprint's horizon). Requirements: OpenAPI 3.1 contract-first, Postgres with transactions and row locks, easy hosting, cheap.

**Options.**

| Criterion | A. TypeScript on Node: Hono (or Fastify) | B. TypeScript: NestJS | C. Go (net/http or chi + sqlc + oapi-codegen) |
|---|---|---|---|
| Learning cost for this developer | Lowest | Medium (decorators, DI, modules) | Highest (new language and idioms) |
| Contract-first with openapi.yaml | Good: generate types (`openapi-typescript`), validate requests with JSON Schema (Ajv) or Zod-based OpenAPI tooling | Code-first by default (`@nestjs/swagger`); contract-first fights the framework | Good: `oapi-codegen` generates server interfaces and types |
| Structure for a modular monolith | Up to the developer (folders per module) | Built in (modules, providers) | Up to the developer (packages per module) |
| Postgres access | Kysely, Drizzle or `postgres`/`pg` with SQL | TypeORM/Prisma common; Kysely possible | `sqlc` (typed SQL), `pgx` |
| Runtime footprint | Small; Hono also runs on Bun, Workers, Vercel Functions | Heavier, slower cold starts | Smallest, single static binary |
| Background jobs | Same process, simple loop or tick endpoint | Same, via `@nestjs/schedule` | Goroutines, very simple |
| Sharing with FE | Same language; diff test fixtures and generated types reusable | Same language | None beyond the OpenAPI file |
| Risk | Unopinionated, so structure must be self-imposed | Over-engineering for a solo MVP | Slower delivery while learning |

Hono vs Fastify inside option A: Hono is lighter and portable across runtimes, which keeps the hosting choice open. Fastify is Node-only but mature, with built-in JSON Schema validation and a large plugin ecosystem. Both are acceptable; the module structure and contract-first tooling matter more than the router.

**Recommendation.** Option A: TypeScript on Node.js (LTS) with **Hono**, contract-first from `openapi.yaml`, typed SQL through Kysely or plain SQL, Vitest for the diff engine and integration tests against a real Postgres. Run as a long-running container (ADR-010 works best there).

**What would change the recommendation.**
- A second backend developer who prefers Go, or a requirement for very low memory/cost on many instances → C.
- Several developers needing enforced conventions and DI → B.
- A decision to host on a container platform only and a preference for a plugin ecosystem → Fastify instead of Hono (still A).
- A decision to host serverless/edge only → A with Hono, and jobs must use host cron or pg_cron calling the tick endpoint.
- If the founder wants to keep business logic close to Supabase (RPC-heavy) → revisit ADR-001 rather than ADR-002.

---

## ADR-003: Auth provider

**Status: Proposed (decision to confirm).**

**Context.** The blueprint chose Supabase Auth with email OTP/magic link and Google, plus custom SMTP via Resend because the built-in sender is limited to 2 emails/hour per project. With a separate API, the provider must issue JWTs that a third-party service can verify.

**Decision (proposed).** Keep **Supabase Auth**. The frontend signs in with the Supabase SDK. The API verifies access tokens via the project's JWKS endpoint (asymmetric signing keys), checks `iss`, `aud`, `exp`, `sub`, and lazily creates the profile. The API uses the admin API only for account deletion.

**Alternatives.**
- *Clerk / similar hosted auth*: polished UI and JWKS, but another vendor and per-MAU pricing.
- *Auth inside the API (e.g. a TS auth library)*: full control, but the developer owns email flows, OAuth, session security and rate limits.
- *Neon Auth or other DB-coupled auth*: relevant only if ADR-004 picks Neon; not evaluated.

**Consequences.** Auth stays independent of the Postgres host. If the database moves to Neon, Supabase may be used for Auth only. The `profiles` table no longer has an FK to `auth.users`. Verify that asymmetric JWT signing keys are enabled on the project before launch.

---

## ADR-004: PostgreSQL host

**Status: Proposed.**

**Context.** The blueprint chose Supabase over Neon for one-vendor Auth, Storage and Cron, and noted Supabase Free pauses after a week of inactivity (Pro $25 before public beta). With the API owning data access, Supabase's client-side features (PostgREST, RLS-as-API) are no longer used.

**Decision (proposed).** **Supabase Postgres** as the default, with **Supabase Storage** for exports and **pg_cron** (or host cron) as the 5-minute scheduler. Connect from the API through the pooler; use the direct URL for migrations.

**When to choose Neon instead.** If cost at low usage matters more than vendor count, or branching databases per preview environment is wanted. Then: object storage moves to an S3-compatible bucket, the scheduler must be host cron (a suspended compute does not run jobs), and Auth stays on Supabase or changes per ADR-003.

**Consequences.** Use only standard Postgres features (plus `pgcrypto`/`gen_random_uuid`) so the host remains swappable. Avoid Supabase-specific schemas (`auth.*`, `storage.*`) in application tables. Transaction-mode pooling limits session features (prepared statements, session `SET`); use `set_config(..., true)` inside transactions.

---

## ADR-005: Immutable recipe versions via copy-on-write, frozen batch snapshots

**Status: Accepted** (from the blueprint).

**Decision.** `recipe_versions` and `recipe_ingredients` are full copies, never updated or deleted. A change creates version n+1 with `parentVersionId`, `changeNote` and `sourceBatchId`. Each batch stores two anchors: `recipe_version_id` (relation) and `recipe_snapshot` (frozen JSON). Actual batch ingredients and params are stored separately, so deviations are visible without changing the recipe. The version rule (DOMAIN_MODEL.md §8.3) decides when Brew Again creates a version.

**Enforcement.** API: no update/delete use cases; versions created only inside the batch-creation and save-as-recipe transactions while holding a lock on the recipe row. Database: `BEFORE UPDATE OR DELETE` triggers on version tables, `batches_guard` trigger on the snapshot and version pointer. Account deletion is the only delete path (cascade under a session flag).

**Consequences.** Storage grows with versions (negligible at this scale). Corrections to a version are expressed as a new version. Exports and history remain correct even if relations are broken.

---

## ADR-006: Diffs and comparisons computed server-side

**Status: Accepted.**

**Context.** The blueprint placed a pure TypeScript diff engine in the Next.js app. With a separate API, the frontend would otherwise duplicate normalization, unit conversion and entitlement logic.

**Decision.** The API computes all diffs: Brew Again preview (`POST /v1/batches/diff-preview`), recipe version diffs, batch snapshot deviations and N-way compare (`POST /v1/compare`). Output is `DiffRow[]` with stable keys, groups, units and directions. The engine is a pure module with exhaustive unit tests (fixtures stored as JSON so the FE can reuse them in UI tests).

**Consequences.** One source of truth for "what changed". Live preview costs a network round-trip (debounce ~300 ms; payloads are small). Entitlement checks sit next to the computation.

---

## ADR-007: Cursor pagination

**Status: Accepted.**

**Decision.** List endpoints return `Page<T> = { data, nextCursor }`. Cursors are opaque, signed, and encode the sort key plus `id`. No offset pagination and no total counts.

**Consequences.** Stable pages while new logs arrive (quick log is write-heavy). Clients cannot jump to page N, which the UI does not need. Changing filters requires starting from the first page.

---

## ADR-008: RFC 9457 problem+json errors

**Status: Accepted.**

**Decision.** All errors use `application/problem+json` with `type, title, status, detail, code, errors[], requestId`. `code` is a stable machine code the frontend switches on (`entitlement_required`, `validation_failed`, `recipe_version_immutable`, `not_found`, ...). Other users' resources return 404.

**Consequences.** One error parser in the FE. New codes are additive. Messages are for humans and may change; codes may not.

---

## ADR-009: Merchant-of-Record billing behind a provider interface

**Status: Accepted** (from the blueprint).

**Context.** The founder is an Indonesian sole proprietor. Stripe Indonesia cannot accept cross-border payments; local gateways leave global tax obligations with the founder. The blueprint therefore requires a Merchant of Record.

**Decision.** Paddle Billing primary; Creem fallback; Polar second fallback. Avoid Lemon Squeezy (migrating to Stripe Managed Payments, which does not list Indonesia). The billing module exposes `createCheckout`, `parseWebhook`, `portalUrl` (plus `cancelSubscription` for account deletion); one adapter per provider. Webhooks land in `billing_events` (unique per provider event), are processed idempotently into `subscriptions`, and entitlements are derived from `subscriptions` only (statuses `trialing`, `active`, `past_due`, with a 3-day grace after `current_period_end` [Blueprint `has_pro`]). Prices: Pro $4.99/month, $39/year, Founding $29/year [Blueprint].

**Consequences.** Switching provider means a new adapter and a new webhook path, no schema change. `priceId` values are allow-listed per environment.

---

## ADR-010: Background jobs on Postgres, no queue or broker

**Status: Accepted.**

**Decision.** A `jobs` table (`id, type, payload, run_at, attempts, max_attempts, locked_at, locked_by, last_error, done_at`) claimed with `SELECT ... FOR UPDATE SKIP LOCKED`. A scheduler (host cron or pg_cron) calls `POST /internal/jobs/tick` every 5 minutes; a long-running API instance may also poll every few seconds for low-latency jobs (exports, billing events). Recurring jobs: reminder dispatch (5 min), purge (daily). The reminder dispatcher claims due reminders directly with `SKIP LOCKED` (up to 200 per run, one digest per user, max 3 attempts) [Blueprint].

**Consequences.** No Redis, broker or separate worker service. Throughput is far above MVP needs. Jobs must be idempotent and safe to retry. If job volume grows, the same table can be drained by a second process without code changes.

---

## ADR-011: Defense-in-depth RLS for the API connection

**Status: Proposed.**

**Decision (proposed).** Keep application-level ownership as the primary control (ADR-001). Before public launch, enable RLS on user-owned tables with policies on `current_setting('app.user_id')`, set per transaction by the API. Background jobs use a separate role with `BYPASSRLS`.

**Consequences.** A forgotten `WHERE user_id` returns nothing instead of leaking data. Every request must run in a transaction; tests must cover both roles. Can be postponed without contract changes.

---

## Unresolved questions

Contract concerns are kept as-is in the OpenAPI file and listed here for the founder and the frontend engineer.

**Decisions to make**
1. **Backend language and framework** (ADR-002). Recommendation: TypeScript + Hono on Node.
2. **Auth provider and Postgres host** (ADR-003, ADR-004). Default: Supabase Auth + Supabase Postgres. Confirm asymmetric JWT signing keys are available on the project.
3. **API hosting target** (container PaaS vs serverless) and region co-located with the database.
4. **Migration tool and schema owner.** The blueprint used the Supabase CLI as the only owner; with a separate API, choose between the Supabase CLI, plain SQL with a runner, or the framework's tool. Keep exactly one owner.
5. **Defense-in-depth RLS** (ADR-011): now, before launch, or never.

**Contract concerns (kept unchanged)**
6. **Stage vocabulary mismatch.** `FermentationType.stages[].key` includes `ferment`, but `BatchStage` is `f1 | f2 | done`. A `custom` single-stage batch is documented as `stage: f1`. Should `BatchStage` include `ferment`?
7. **`PATCH /v1/batches/{id}` "params.roomTempC notes".** `Batch` has no notes field; only `params.roomTempC` is accepted. Should there be a batch-level note (blueprint `outcome_note` maps to `Rating.note`)?
8. **No edit endpoints for batch ingredients or log entries.** The blueprint FR5 allows editing logs; typo correction on an active batch's ingredients is likely needed. Consider `PATCH /v1/logs/{id}` and an ingredient correction endpoint for active batches (which must re-run the version rule).
9. **`RecipeVersion` has no base volume.** Scaling amounts to a new batch volume needs it. Stored internally as `base_volume_ml`; expose as a field?
10. **Entitlement limits are incomplete.** `limits` covers `compareItems` and `customMeasurements`, but the blueprint also gates non-adjacent version diffs (implemented as 402) and "what changed vs last good batch" for older batches (free sees the latest only; not implemented until clarified). Add boolean feature flags to `Entitlements`?
11. **Measurement kinds.** The contract is numeric-only; the blueprint had `scale`, `boolean`, `text` kinds. Numeric covers scales like firmness 0-3; text values are not supported.
12. **`planned` batch status** from blueprint FR3 is absent from the contract. Dropped for MVP.
13. **Compare curves.** Blueprint FR9 includes pH/temperature curves aligned by day (Pro). `CompareResult` has no series; a `GET /v1/compare/series` (or a `series` field) is needed later.
14. **`DELETE /v1/me` with a request body.** Some proxies and clients drop DELETE bodies. Alternative: `POST /v1/me/deletion`. Kept per contract.
15. **Version rule opt-out.** Users cannot make a one-off ingredient tweak without creating a version. The blueprint wants versioning to be invisible; confirm this is acceptable.
16. **`calibratedAt` for pH meters** and onboarding `brews` answers exist in the blueprint schema but not in the contract.

**Product rules assumed here (confirm)**
17. Save as Recipe on a batch without a recipe links that batch to the new v1, while its snapshot stays null.
18. Finishing a batch auto-closes open bottles without a rating; finishing the last bottle does not finish the batch.
19. Logging on finished batches allowed for `note`, `taste`, `measure` only.
20. Reminder schedule: taste check at `targetF1Days` (or the stage's min typical days), daily burps through the F2 window, a chill reminder at the F2 max; default time 09:00 local.
21. Batch code prefixes `WK` (water kefir) and `CU` (custom); only `KB` is given.
22. `Entitlements.source` values `default | subscription | beta | grant`; closed beta grants Pro to everyone [Blueprint].
23. Account deletion: no grace period, executed within 24 h; GDPR/UK obligations not researched.
24. Rate-limit values and export retention (7 days) are initial guesses.

## Contract gaps found by the frontend (5 Nov 2026)

These were found while building the jarlore-fe screens against this contract. The spec has not been changed. Each gap is listed with the frontend workaround in use today. See `jarlore-fe/docs/architecture/FRONTEND_ARCHITECTURE.md`.

| # | Gap | Frontend impact today | Proposed change |
|---|---|---|---|
| F1 | There are no starting ingredients for a blank batch per fermentation type | The mock-only `getBatchTemplate(type)` supplies them | Add `defaultVolumeMl`, `defaultIngredients` and `defaultParams` to `FermentationType` |
| F2 | A recipe version can't be read by its own id (only `/recipes/{id}/versions/{no}`) | "Brew v4" links carry a `versionId` | Add `GET /v1/recipe-versions/{versionId}`, or have the frontend pass `{recipeId, versionNo}` |
| F3 | `diff-preview` doesn't say whether the draft will create a new recipe version | The UI explains the rule in prose only | Add `willCreateVersion` and `nextVersionNo` to the `diff-preview` response |
| F4 | Custom batches use the stage `f1`, while the type's stage is `ferment` (also listed above) | The frontend falls back to the type's first stage | Add `ferment` to `BatchStage`, or document the mapping |
| F5 | `GET /v1/reminders` has no `batchId` filter | Not needed yet (`Batch.nextReminder` is enough) | Add the filter when a reminders screen needs it |
