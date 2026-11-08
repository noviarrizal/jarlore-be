# Implementation roadmap

Updated 2026-11-07.

## Done

| Phase | Delivered |
|---|---|
| 0 Inspect and plan | Contract gaps F1–F5 resolved (ADR list), decisions recorded as ADR-002/003/004/011–014 |
| 1 Go and Neon foundation | Module layout, config validation, pgxpool tuned for Neon, goose migrations, sqlc, JWT/JWKS verification, health endpoints, graceful shutdown, Dockerfile, Cloud Run service file |
| 2 Domain and database | Schema with composite keys, immutability triggers, RLS, restricted runtime role, seed data; F1 batches and F2 bottle experiments |
| 3 API | Batches, bottles, logs and measurements (four kinds, units), ratings, recipes and immutable versions (batch and bottle scope), reminders (scheduled and completed by logs), Today, entitlements |
| 4 Reproducibility | Deterministic diff engine with categories, Brew Again draft and previews, version rule for batches and bottles, snapshots, compare across bottles and batches |
| 5 Frontend integration | Every screen reads the Go API (mocks removed); generated types; development sign-in; new flows: Bottle it, Finish & rate, Mark as best, Save as recipe (batch and bottle), new bottle from a recipe with live diff |
| 6 Deployment readiness | Neon and Cloud Run guides with costs; container definition. **Not deployed.** |

## P0 — before real users

1. **Neon Auth sign-in in the frontend** (ADR-003). Replace development sign-in with `@neon/auth` (email OTP; Google later), and forward the Neon Auth JWT to the API from the server (see `jarlore-fe/src/lib/auth/session.ts`). Needs a Neon project with Auth enabled.
2. **First deployment** (after the founder approves billing): Neon project in Singapore, API role, migrations, Secret Manager, Cloud Run, budget alert. Verify `docker build` (not possible on the development machine) and measure cold starts.
3. **Rate limits** (API_CONTRACT §9), at least per user on writes and on compare/preview.
4. **CI**: `go vet`, `go test` with a PostgreSQL service, `sqlc diff`, OpenAPI lint, frontend lint/typecheck/build.

## P1

- Account deletion and data export (GDPR-style rights; designed in SECURITY.md §9).
- Edit/undo for log entries and an ingredient correction path for active batches (versioning implications in DOMAIN_MODEL).
- Reminder delivery (email or push) via Cloud Scheduler → internal endpoint.
- Custom measurement definition editing/archiving; boolean and text readings in the quick-log sheet.
- Batch discard and archive in the UI (API exists).
- Recipe list performance: `recipes()` runs a few queries per recipe; batch them when lists grow.
- Unit system "us" for ingredient display.

## P2

- Billing (Merchant of Record) and webhooks; entitlements already read `subscriptions`.
- Compare curves (pH/temperature over time).
- Photos, calendar feed, offline queue for quick log.

## Next task (recommended)

**Neon Auth sign-in in jarlore-fe**, because nothing else blocks the first real user.

- Files: `jarlore-fe/src/lib/auth/session.ts`, `src/app/login/page.tsx`, `src/features/auth/*`, `src/proxy.ts`; config in `jarlore-be` only (`AUTH_ISSUER`, `AUTH_JWKS_URL`). First confirm on the real project: `sub` is a UUID, Neon Auth is available in Singapore, and how the Next.js server obtains the JWT.
- Acceptance: a new user signs in with an emailed code; the API creates their profile from the token; the session refreshes before expiry without losing a quick-log entry; signing out clears the cookie; development sign-in is unavailable in production builds; an expired token lands on `/login?expired=1`.
