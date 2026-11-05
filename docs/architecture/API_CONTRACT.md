# API Contract

The machine-readable contract is [`docs/api/openapi.yaml`](../api/openapi.yaml) (OpenAPI 3.1). This document explains the conventions. When the two disagree, fix one of them in the same change; the frontend codes against the OpenAPI shapes.

## 1. Principles

1. **Workflow-oriented, not table CRUD.** Writes are named after user actions (`/finish`, `/discard`, `/bottles`, `/save-as-recipe`, `/brew-again`). Each runs one transaction with all side effects (stage change log, reminders, versions).
2. **The server owns rules.** Clients send intent; the server validates, computes codes, versions, diffs, entitlements and derived fields.
3. **Return the updated aggregate.** State-changing batch endpoints return the full `Batch` so the frontend can replace its cache without a second request.
4. **Stable shapes.** Fields are always present; absent values are `null` (or `[]` for lists), never omitted, in responses.
5. **No safety verdicts.** No field, enum or message labels a ferment safe, ready or passed.

## 2. Format conventions

| Topic | Rule |
|---|---|
| Base path | `/v1`. Exceptions: public `GET /calendar/{token}.ics`, `POST /webhooks/billing/{provider}`, internal `/internal/*` (not in the public contract). |
| Content type | `application/json; charset=utf-8`. Errors: `application/problem+json`. |
| Field case | camelCase. Enum values are lowercase snake_case. |
| IDs | Opaque strings (UUIDs). Clients must not parse them. |
| Timestamps | ISO 8601 UTC with `Z`, e.g. `2026-11-05T08:00:00Z`. Day boundaries ("today") use the profile `timezone`. |
| Numbers | JSON numbers. Amounts keep the unit the user entered (`amount` + `unit`); canonical units are used inside diffs. |
| Unknown fields | Request bodies reject unknown properties with 422 `validation_failed` (prevents silent typos on PATCH). |
| Request size | 64 KB max body [Assumption]. |

## 3. Versioning

- URL major version (`/v1`). Additive changes (new optional request fields, new response fields, new endpoints, new enum values documented as such) are non-breaking.
- Breaking changes require `/v2` for the affected endpoints. A deprecated endpoint returns `Deprecation` and `Sunset` headers for at least 90 days [Assumption].
- **Frontend tolerance rule:** clients ignore unknown response fields and treat unknown enum values as "other".
- The OpenAPI `info.version` follows semver for the document itself.

## 4. Authentication

- `Authorization: Bearer <JWT>` on every `/v1` route. The JWT is issued by the auth provider (proposed: Supabase Auth; ADR-003).
- The API verifies signature against the provider JWKS (cached, refreshed on unknown `kid`), plus `iss`, `aud`, `exp` (with ≤ 60 s clock skew) and `sub`.
- The first authenticated request lazily creates the `profiles` row (`id = sub`, `email` from claims). Email is re-synced when the claim changes.
- No cookies are used by the API, so CSRF does not apply. CORS allows only `FRONTEND_ORIGIN`.
- 401 `unauthenticated` for a missing/invalid/expired token. The frontend refreshes the session and retries once.

## 5. Error model (RFC 9457)

```json
{
  "type": "https://docs.jarlore.app/errors/entitlement_required",
  "title": "Upgrade required",
  "status": 402,
  "detail": "The free plan compares up to 2 items.",
  "code": "entitlement_required",
  "errors": [{ "field": "items", "code": "too_many", "message": "Maximum 2 items on the free plan" }],
  "requestId": "req_01J..."
}
```

| Status | `code` values | When |
|---|---|---|
| 400 | `bad_request`, `invalid_cursor` | Malformed JSON, bad query parameter, cursor from another query |
| 401 | `unauthenticated` | Token missing/invalid/expired |
| 402 | `entitlement_required` | Plan limit (compare items, custom definitions, non-adjacent recipe diff) |
| 403 | `system_definition_readonly` | Changing a system measurement definition (visible to all, owned by none) |
| 404 | `not_found`, `snapshot_not_found` | Absent, or owned by another user (never 403) |
| 409 | `invalid_state_transition`, `recipe_version_immutable`, `no_changes`, `idempotency_in_progress`, `definition_in_use`, `duplicate_key`, `subscription_exists` | State conflicts |
| 422 | `validation_failed`, `field_immutable`, `idempotency_key_reused` | Semantic validation; `errors[]` lists fields |
| 429 | `rate_limited` | With `Retry-After` |
| 500/503 | `internal_error`, `unavailable` | No internal details leaked |

- `type` is a URI per `code` (documentation page; may 404 until written).
- `requestId` equals the `X-Request-Id` response header, which is also sent to Sentry and logs.
- Field paths in `errors[].field` use dot/bracket notation: `ingredients[2].amount`, `measurements[0].method`.

## 6. Pagination

- Cursor-based (ADR-007). Response: `Page<T> = { data: T[], nextCursor: string | null }`.
- `limit` default 20, max 100. `nextCursor = null` means the last page.
- The cursor is opaque (base64url of the sort key + id, signed with `CURSOR_SIGNING_SECRET`). It is bound to the filter and sort; reusing it with different parameters returns 400 `invalid_cursor`.
- No total counts (expensive and not needed by the UI). Counts that matter are explicit fields (`bottleCount`, `versionCount`, `batchCount`).

## 7. Filtering and sorting

| Endpoint | Filters | Sort |
|---|---|---|
| `GET /v1/batches` | `status`, `stage`, `type`, `recipeId`, `q` (name, code, hypothesis), `archived` (default false) | `started_desc` (default), `started_asc`, `rating_desc` (nulls last), `updated_desc` |
| `GET /v1/recipes` | `q` (name), `type`, `archived` | `createdAt` desc [Assumption] |
| `GET /v1/reminders` | `status`, `from`, `to` (on `dueAt`) | `dueAt` asc |
| `GET /v1/batches/{id}/logs` | `bottleId` | `occurredAt` desc, then `id` |

Ties are broken by `id` so cursors are stable. `updated_desc` uses `batches.updated_at`, which is bumped by any write to the batch aggregate (log, bottle, status) [Assumption].

## 8. Idempotency

- Header `Idempotency-Key` (8-128 chars, UUID recommended), scoped per user, stored 24 hours.
- **Required** on `POST /v1/batches/{id}/logs` (optimistic quick log). **Optional** on `POST /v1/batches`, `/bottles`, `/finish`, `/save-as-recipe`, `POST /v1/bottles/{id}/finish`.
- Behaviour:
  - First request: the key is inserted with the request hash in the same transaction as the write; the response status and body are stored.
  - Same key, same body: the stored response is returned with `Idempotent-Replayed: true`.
  - Same key, different body: 422 `idempotency_key_reused`.
  - Same key while the first request is still running: 409 `idempotency_in_progress`; the client retries after a short delay.
- Natural idempotency: `finish` on an already finished batch returns 409 `invalid_state_transition` unless replayed by key; `DELETE /v1/logs/{id}` is naturally idempotent (204 again).

## 9. Rate limits [Assumption: initial values, tune with real traffic]

| Scope | Limit |
|---|---|
| Authenticated, per user | 120 requests/min overall; 60 writes/min |
| `POST /v1/compare`, `/diff-preview` | 60/min per user |
| `POST /v1/exports` | 3 per 24 h per user |
| `DELETE /v1/me` (step 1) | 3 per hour per user |
| `POST /v1/calendar/rotate` | 10 per hour per user |
| `POST /v1/billing/checkout` | 10 per hour per user |
| Public `.ics` | 60 per hour per token and 300 per hour per IP |
| Unauthenticated requests | 30/min per IP |

Responses include `RateLimit-Policy` and `RateLimit` headers (IETF draft) and `Retry-After` on 429. In the MVP the counters live in Postgres or process memory (single instance); no Redis.

## 10. DTO conventions

- Shapes are defined in OpenAPI `components/schemas`. Key ones: `Batch`, `BatchSummary`, `Bottle`, `LogEntry`, `Measurement`, `Recipe`, `RecipeVersion`, `RecipeVersionRef`, `Reminder`, `DiffRow`, `CompareResult`, `Page<T>`, `Problem`.
- Request DTOs are separate from response DTOs (`CreateBatchRequest` vs `Batch`; `IngredientInput` has no `id`).
- `BatchSummary` = `Batch` without `ingredients`, `params`, `bottles`, plus `bottleCount`.
- `Batch.lastLog` and `nextReminder` are computed (latest non-deleted log; earliest pending/snoozed reminder).
- `Bottle.lastMeasurements` = latest value per `definitionKey` from that bottle's non-deleted logs.
- `Recipe.avgOverall`/`RecipeVersion.avgOverall` = mean of `rating.overall` over finished batches pinned to the recipe/version; `null` if none.
- Units: requests carry the user's unit; responses echo stored values. Diff rows are converted to the user's preferences.
- `GET /v1/batches/{id}/brew-again` drafts reuse `CreateBatchRequest`, so the form can submit the draft as-is.
- Frontend type generation from the OpenAPI file (for example `openapi-typescript`) is recommended so the FE never hand-writes these types.

## 11. Endpoint overview by workflow

| Workflow | Method and path | Purpose | Notes |
|---|---|---|---|
| **Identity** | `GET /v1/me` | Profile (lazy create) | |
| | `PATCH /v1/me` | displayName, unitSystem, tempUnit, timezone | |
| | `DELETE /v1/me` | Two-step async deletion | 202; confirmation token by email |
| **Reference** | `GET /v1/fermentation-types` | Seed templates | Cacheable (`Cache-Control: private, max-age=3600`) |
| **Today** | `GET /v1/today` | Active batches, due reminders, recently finished (7 days, max 5) | |
| **Log** | `GET /v1/batches` | List batches | `Page<BatchSummary>` |
| | `POST /v1/batches` | Create (blank, from version, Brew Again) | Version rule; optional idempotency |
| | `GET /v1/batches/{id}` | Batch aggregate | |
| | `PATCH /v1/batches/{id}` | name, vessel, isReference, params.roomTempC | Others → 422 `field_immutable` |
| | `GET /v1/batches/{id}/snapshot` | Frozen recipe snapshot + deviations | 404 `snapshot_not_found` if no recipe |
| | `POST /v1/batches/{id}/bottles` | Split into F2 bottles | Stage → f2, burp reminders |
| | `PATCH /v1/bottles/{id}` | Edit bottle, chill, discard | |
| | `GET /v1/batches/{id}/logs` | Journal | `Page<LogEntry>` |
| | `POST /v1/batches/{id}/logs` | Quick log; `bottleIds[]` for burp all | **Idempotency-Key required** |
| | `DELETE /v1/logs/{id}` | Soft delete | 204 |
| **Rate** | `POST /v1/bottles/{id}/finish` | Finish and rate a bottle | |
| | `POST /v1/batches/{id}/finish` | Finish and rate a batch | Closes open bottles |
| | `POST /v1/batches/{id}/discard` | Discard with reason | |
| | `POST /v1/batches/{id}/archive`, `/unarchive` | Visibility | Closed batches only |
| **Save as Recipe** | `POST /v1/batches/{id}/save-as-recipe` | v1 of a new recipe, or vN+1 with `recipeId` | 409 `no_changes` if identical |
| | `GET /v1/recipes`, `GET /v1/recipes/{id}` | Recipes | |
| | `GET /v1/recipes/{id}/versions`, `/versions/{versionNo}` | Immutable versions | |
| | `GET /v1/recipes/{id}/diff?from=&to=` | Version diff | Free: adjacent only |
| | `GET /v1/recipes/{id}/batches` | Batches of this recipe | |
| | `PATCH /v1/recipes/{id}` | name, archived | |
| **Brew Again** | `GET /v1/batches/{id}/brew-again` | Draft + base + version | |
| | `POST /v1/batches/diff-preview` | Live diff for the draft | Read-only |
| **Compare** | `POST /v1/compare` | 2..6 batches/bottles | Free 2, Pro 6 → 402 |
| | `GET /v1/batches/{id}/reference` | Last good batch | |
| **Measurements** | `GET/POST /v1/measurement-definitions`, `PATCH/DELETE /v1/measurement-definitions/{id}` | Custom definitions | Free 3; delete archives if used |
| **Reminders** | `GET/POST /v1/reminders`, `PATCH /v1/reminders/{id}` | List, create, done/snooze/dismiss | |
| | `GET /v1/calendar`, `POST /v1/calendar/rotate` | Feed URL | |
| | `GET /calendar/{token}.ics` | Public feed | No auth |
| **Data** | `POST /v1/exports`, `GET /v1/exports/{id}` | Async ZIP export | Signed URL |
| **Billing** | `GET /v1/entitlements` | Plan, limits, source | Poll ≤ 10 s after checkout [Blueprint] |
| | `POST /v1/billing/checkout` | Checkout URL | Allow-listed `priceId` |
| | `GET /v1/billing/portal` | Portal URL | 404 if never subscribed |
| | `POST /webhooks/billing/{provider}` | MoR webhooks | Signature, idempotent |

## 12. Endpoint-specific behaviour worth knowing

- **`POST /v1/batches/{id}/logs`**: `kind` ∈ `note, taste, burp, measure, feed, issue`. `bottleId` and `bottleIds` are mutually exclusive. With `bottleIds`, one entry per bottle is created and returned in `data[]` (same order). Bottles must belong to the batch and be open (`conditioning` or `chilled`) for `burp`. Logging on a finished batch is allowed for `note`, `taste` and `measure` (late tasting notes) [Assumption]; other kinds return 409.
- **`PATCH /v1/reminders/{id}`**: `{status:'snoozed', snoozeUntil}` sets `dueAt = snoozeUntil`; `{status:'done'|'dismissed'}` closes it; `dueAt`/`label` may be edited while pending or snoozed.
- **`DELETE /v1/measurement-definitions/{id}`**: returns `{id, outcome:'deleted'}` if unused, else archives and returns `{id, outcome:'archived'}`.
- **`GET /v1/billing/portal`**: 404 `not_found` when the user has no provider customer.
- **`POST /v1/billing/checkout`**: 409 `subscription_exists` if an active subscription exists (use the portal to change plan).
