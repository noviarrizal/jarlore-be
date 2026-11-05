# Security

Scope: the Jarlore API and its data. Labels: **[Blueprint]**, **[Assumption]**, **[Decision to confirm]**.

## 1. Threat model in one table

| Asset | Main threats | Primary controls |
|---|---|---|
| Brew data (batches, notes, measurements) | Cross-tenant read/write (IDOR), bulk scraping | Ownership on every query, composite FKs, 404 for others' data, rate limits |
| Recipe history | Tampering with immutable versions/snapshots | No update path, DB triggers, transactional version rule |
| Plan / entitlements | Forged upgrades | Subscriptions only from signature-verified, de-duplicated webhooks |
| Account | Token theft, unwanted deletion | Short-lived JWT, two-step deletion with emailed token |
| Calendar feed | Token leakage reveals reminders | Unguessable rotatable token, minimal event content, no token logging |
| Secrets | Leakage via repo or logs | Env-only secrets, `.env*` ignored, log scrubbing |

## 2. Authentication

- The frontend signs in with the auth provider (proposed Supabase Auth: email magic link/OTP and Google [Blueprint]; **[Decision to confirm]**, ADR-003).
- The API accepts only `Authorization: Bearer <JWT>`. It verifies:
  - signature against the provider's JWKS (asymmetric keys; cache keys, refetch on unknown `kid`, at most once per minute);
  - `iss = AUTH_ISSUER`, `aud = AUTH_AUDIENCE`, `exp`, `nbf`/`iat` with ≤ 60 s skew;
  - `sub` is a UUID; `role` is `authenticated` (Supabase), anonymous sessions are rejected.
- **[Assumption]** If the provider project still uses a shared HS256 secret, migrate it to asymmetric signing keys before launch so the API never holds a token-minting secret.
- No API sessions or cookies. CORS allows `FRONTEND_ORIGIN` only, methods used by the contract, headers `Authorization, Content-Type, Idempotency-Key`.
- Profile is created lazily on the first valid request (`INSERT ... ON CONFLICT DO NOTHING`).
- Internal endpoints (`/internal/jobs/tick`) require `Authorization: Bearer <CRON_SECRET>` compared in constant time, and are not exposed in the public OpenAPI.

## 3. Authorization

- **Single rule:** the user id comes from the verified `sub`, and every query on user-owned data includes `user_id = :sub`. Request bodies never carry `userId`.
- **404, not 403**, for resources that exist but belong to someone else, so ids cannot be probed [Contract]. 403 is used only for visible-but-read-only resources (system measurement definitions).
- Referenced ids inside bodies (`source.id`, `bottleIds`, `recipeId`, compare items, `batchId` on reminders) are resolved with the same ownership filter; any miss is 404 for the whole request.
- Implementation guard **[Assumption]**: the data-access layer exposes repository functions that require a `userId` argument; raw SQL without it fails code review. An integration test suite runs every endpoint as user B against user A's ids and expects 404.
- Entitlements are checked server-side in the use case, not inferred from the frontend.

## 4. Input validation

- Validate every request against the OpenAPI schema (types, enums, lengths, ranges, `additionalProperties: false`), then apply domain rules (DOMAIN_MODEL.md §7).
- Domain checks include: `startedAt` not in the future (≤ 5 min skew), `finishedAt >= startedAt`, bottle label uniqueness, measurement within definition `min`/`max`, `ph` requires `method`, timezone is a valid IANA zone, `priceId` in the allow-list.
- Free text (`note`, `hypothesis`, `name`) is stored as-is and length-limited. The API never renders HTML; the frontend must escape output. ICS output escapes text per RFC 5545.
- Body size limit 64 KB; JSON depth limit; reject duplicate JSON keys if the parser allows [Assumption].
- All SQL is parameterized. Sort and filter values map to allow-listed columns.

## 5. Data isolation

1. **Composite foreign keys** `(parent_id, user_id)` on every child table [Blueprint]. Even a buggy query cannot attach a log to another user's batch.
2. **Denormalized `user_id`** on every child row with an index leading on `user_id` [Blueprint].
3. **Optional defense-in-depth RLS [Decision to confirm].** The API connects as a non-owner role without `BYPASSRLS`. Each request transaction runs `SELECT set_config('app.user_id', :sub, true)`, and policies check `user_id = current_setting('app.user_id')::uuid`. Jobs that span users (reminders, webhooks, deletion) use a separate role with `BYPASSRLS`. Trade-off: one more moving part and every query must run inside a transaction, which works with transaction-mode poolers. Recommended before public launch, not required for the first build.
4. **Immutability triggers** on `recipe_versions`, `recipe_ingredients` and the batch snapshot [Blueprint], as a second line behind the API.
5. Object storage paths are prefixed `{user_id}/`; the bucket is private; downloads only through short-lived signed URLs.

## 6. Rate limits and abuse

See API_CONTRACT.md §9 for values. Additionally:
- Per-IP limits on unauthenticated routes (`.ics`, webhooks get a generous limit and body size cap).
- Exports and account-deletion emails are capped to stop email abuse.
- Compare and diff-preview are CPU-bound; limits protect the single instance.

## 7. Webhook verification

- Route: `POST /webhooks/billing/{provider}` for `paddle`, `creem`, `polar`.
- Verify the provider signature over the **raw** request body before JSON parsing, with the provider secret (`*_WEBHOOK_SECRET`), constant-time comparison and a timestamp tolerance (5 minutes) where the provider signs a timestamp. Exact header names and schemes are provider-specific and must be taken from each provider's current docs when the adapter is written.
- Insert into `billing_events (provider, provider_event_id)` with a unique constraint. Duplicates return 200 and are not reprocessed [Blueprint].
- Process asynchronously; map users only through ids the API created (`customData.userId` / stored customer id), never through email matching alone.
- Never trust plan data from the frontend; `/v1/entitlements` reads only `subscriptions`.

## 8. Secrets management

- Secrets live in the host's secret store and are injected as environment variables. Names are listed in `.env.example`; values are never committed (`.gitignore` excludes `.env*` except `.env.example`).
- Highly privileged secrets: `SUPABASE_SERVICE_ROLE_KEY` (used only by the account-deletion job), MoR API keys, webhook secrets, storage keys, `DATABASE_URL`. None of them reach the frontend.
- Rotation: webhook and API keys rotated on staff/device change or suspected leak; `CRON_SECRET` and `CURSOR_SIGNING_SECRET` support overlap by accepting two values during rotation [Assumption].
- Logs and Sentry scrub `Authorization`, `Idempotency-Key`, cookies, calendar tokens in paths, signed URLs and request bodies of log notes.

## 9. Account deletion

1. `DELETE /v1/me` without a token: create a single-use token (random, stored hashed, 30-minute expiry), email it, return 202 `confirmation_sent`.
2. `DELETE /v1/me` with the token: verify, set `profiles.deletion_requested_at`, enqueue `delete_account`, return 202 `scheduled`. Further API calls by this user return 401/410 [Assumption].
3. Job: cancel any active subscription at the MoR; delete storage objects under `{user_id}/`; delete all user rows (cascade); delete the auth identity through the provider's admin API; write an anonymized audit record.
4. **[Assumption]** No grace period in the MVP; deletion runs within 24 hours. Billing records the MoR must retain stay with the MoR. GDPR/UK specifics were not researched in the blueprint and remain open.
5. Exports remain free before deletion; the confirmation email suggests exporting first.

## 10. Audit log

Append-only `audit_events (id, user_id, actor, action, target_type, target_id, request_id, ip_hash, metadata, created_at)` **[Assumption]**. Logged actions:

| Action | Why |
|---|---|
| `account.deletion_requested`, `account.deletion_confirmed`, `account.deleted` | Accountability for irreversible actions |
| `export.created`, `export.downloaded` (signed URL issued) | Data access |
| `calendar.token_rotated` | Feed revocation |
| `recipe.version_created` (with source: brew_again or save_as_recipe) | History integrity |
| `batch.finished`, `batch.discarded` | Lifecycle |
| `billing.subscription_changed`, `billing.webhook_rejected` | Money and entitlement |
| `auth.cross_tenant_miss` (sampled) | Detect id probing |

Retention 1 year **[Assumption]**. On account deletion, rows keep the action but replace `user_id` with a salted hash.

## 11. Food-safety constraints

From blueprint §16. The API is a record-keeping tool.
- No response field, enum value, reminder label, email or error message uses "safe", "ready", "done" (as a drinkability verdict), "passed", "OK to drink" or similar. Reminder labels come from a fixed vocabulary: "Taste check", "Burp", "Planned F2 end", "Move to fridge", custom user text.
  - Note: `BatchStage = 'done'` is a workflow stage in the contract, not shown to users as a verdict. The frontend must not render it as "Done/Ready to drink".
- The API never evaluates measurements as pass/fail, never colours or flags pH, never predicts safety, and has no AI features.
- `typicalDays` and any reference ranges are references with provenance, not targets.
- `ph` measurements require a `method` (strip, meter, ...) [Blueprint safeguard]; calibration date support is planned (`calibrated_at`).
- `issue` log entries are stored verbatim with no classification.
- User-created templates or measurements can never be labelled "validated".
