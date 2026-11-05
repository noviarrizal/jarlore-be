# Sequences

Participants: **U** user, **FE** Next.js frontend, **AP** auth provider (proposed Supabase Auth), **API** Jarlore API, **DB** PostgreSQL, **J** job runner (same codebase as the API), **S** object storage, **M** Resend, **MoR** billing provider. Every API-to-DB write in a request runs in one transaction unless noted.

## 1. Authentication

Magic link / OTP or Google OAuth happens between the frontend and the auth provider. The API only verifies the resulting JWT.

```mermaid
sequenceDiagram
    autonumber
    actor U
    participant FE
    participant AP as Auth provider
    participant M as Resend (SMTP)
    participant API
    participant DB

    alt Email magic link or OTP
        U->>FE: Enter email
        FE->>AP: signInWithOtp(email)
        AP->>M: Send link or code via custom SMTP
        M-->>U: Email
        U->>FE: Open link or type code
        FE->>AP: Verify OTP or exchange code
    else Google
        U->>FE: Continue with Google
        FE->>AP: OAuth redirect (PKCE)
        AP-->>FE: Callback with code
        FE->>AP: Exchange code
    end
    AP-->>FE: Access JWT (short-lived) + refresh token

    FE->>API: GET /v1/me (Authorization Bearer JWT)
    API->>API: Look up kid in cached JWKS
    opt Unknown kid or cache expired
        API->>AP: GET JWKS
        AP-->>API: Public keys
    end
    API->>API: Verify signature, iss, aud, exp, sub
    alt Invalid
        API-->>FE: 401 problem+json code unauthenticated
    else Valid
        API->>DB: INSERT profiles (id = sub, email) ON CONFLICT DO NOTHING
        API->>DB: SELECT profile WHERE id = sub
        API-->>FE: 200 Me
    end
    Note over FE,AP: FE refreshes the session with the auth provider before expiry and retries once on 401
```

## 2. Create F1 batch

```mermaid
sequenceDiagram
    autonumber
    actor U
    participant FE
    participant API
    participant DB

    U->>FE: New batch, choose Kombucha, edit defaults, optional backdate
    FE->>API: POST /v1/batches {fermentationTypeId, volumeMl, ingredients, params, startedAt}
    API->>API: Validate body, type exists, startedAt not in the future
    API->>DB: BEGIN
    API->>DB: UPSERT batch_code_counters (user, KB) RETURNING last_no
    API->>DB: INSERT batches (code KB-015, status active, stage f1, snapshot null)
    API->>DB: INSERT batch_ingredients (amount_per_l computed)
    API->>DB: INSERT reminder taste_check at startedAt + targetF1Days or typicalDays min
    API->>DB: COMMIT
    API-->>FE: 201 Batch
    FE-->>U: Batch card in Today
```

## 3. Split F1 into F2 bottles

```mermaid
sequenceDiagram
    autonumber
    actor U
    participant FE
    participant API
    participant DB

    U->>FE: Bottle it (F2), 4 bottles with flavours and additions
    FE->>API: POST /v1/batches/{id}/bottles {bottles, bottledAt} + Idempotency-Key
    API->>DB: SELECT batch WHERE id AND user_id FOR UPDATE
    alt Not found or other user
        API-->>FE: 404 not_found
    else Not active, or type has no f2 stage
        API-->>FE: 409 invalid_state_transition
    else OK
        API->>API: Validate labels unique within batch, bottledAt >= startedAt
        API->>DB: INSERT bottles (status conditioning, startedAt = bottledAt)
        opt Batch stage was f1
            API->>DB: UPDATE batches SET stage f2, bottled_at
            API->>DB: INSERT batch_logs kind stage_change f1 to f2
            API->>DB: Dismiss pending taste_check for F1
        end
        API->>DB: INSERT daily burp reminders for the F2 typicalDays window, plus one chill reminder
        API->>DB: Store idempotency response
        API-->>FE: 201 Batch with bottles
    end
```

## 4. Record bottle observation (optimistic and idempotent)

```mermaid
sequenceDiagram
    autonumber
    actor U
    participant FE
    participant API
    participant DB

    U->>FE: Burp all (or pH 3.4 strip on bottle A)
    FE->>FE: key = uuid(), add optimistic entry, show it immediately
    FE->>API: POST /v1/batches/{id}/logs {kind burp, bottleIds, occurredAt} Idempotency-Key key
    API->>DB: BEGIN
    API->>DB: INSERT idempotency_keys (user, key, hash) ON CONFLICT DO NOTHING
    alt Key existed with same hash and stored response
        API->>DB: ROLLBACK
        API-->>FE: 201 stored body, Idempotent-Replayed true
    else Key existed with different hash
        API-->>FE: 422 idempotency_key_reused
    else Key in progress
        API-->>FE: 409 idempotency_in_progress
    else New key
        API->>DB: Check batch and bottles owned, open, in this batch
        API->>DB: Resolve definition keys, require method for ph, check min and max
        API->>DB: INSERT batch_logs (one per bottle) and batch_measurements
        API->>DB: UPDATE batches.updated_at, store response on the key
        API->>DB: COMMIT
        API-->>FE: 201 {data: LogEntry[]}
    end
    alt Network error or timeout
        FE->>API: Retry with the same Idempotency-Key
        API-->>FE: 201 (replayed, no duplicate)
    else Validation error
        FE-->>U: Mark entry Not saved, retry
    end
```

## 5. Finish and rate an experiment

```mermaid
sequenceDiagram
    autonumber
    actor U
    participant FE
    participant API
    participant DB

    U->>FE: How did bottle A turn out? fizz 4, overall 5
    FE->>API: POST /v1/bottles/{id}/finish {rating}
    API->>DB: Lock bottle, check status conditioning or chilled
    API->>DB: UPDATE bottle status finished, rating, finished_at
    API->>DB: Dismiss pending reminders for this bottle
    API-->>FE: 200 Bottle

    U->>FE: Finish batch, overall 4
    FE->>API: POST /v1/batches/{id}/finish {rating, finishedAt}
    API->>DB: Lock batch, check status active, finishedAt >= startedAt
    API->>DB: UPDATE batch status finished, stage done, rating
    API->>DB: Close open bottles (finished without rating)
    API->>DB: INSERT stage_change log, dismiss pending reminders
    API-->>FE: 200 Batch
    FE-->>U: Overall >= 4 or wouldRepeat, show Save as recipe banner
```

## 6. Save the result as a recipe

```mermaid
sequenceDiagram
    autonumber
    actor U
    participant FE
    participant API
    participant DB

    U->>FE: Save as recipe "Ginger fizz"
    FE->>API: POST /v1/batches/{id}/save-as-recipe {name, changeNote}
    API->>DB: Load batch with ingredients, params, bottles (owned, not discarded)
    alt No recipeId
        API->>DB: INSERT recipes
        API->>DB: INSERT recipe_versions v1 (ingredients, targetF1Days, defaultBottles from bottles, sourceBatchId)
        API->>DB: UPDATE recipes.current_version_id
        opt Batch has no recipe version
            API->>DB: UPDATE batches.recipe_version_id = v1 (snapshot stays null)
        end
    else recipeId given
        API->>DB: SELECT recipe FOR UPDATE (owned, same type)
        API->>API: Diff batch recipe-level fields and defaultBottles vs latest version
        alt No differences
            API-->>FE: 409 no_changes
        else Differences
            API->>DB: INSERT recipe_versions vN+1 (parent = latest, sourceBatchId)
            API->>DB: UPDATE recipes.current_version_id
        end
    end
    API->>DB: INSERT audit_events recipe.version_created
    API-->>FE: 201 Recipe (latestVersion = new version)
```

## 7. Brew Again (draft, diff-preview, create)

```mermaid
sequenceDiagram
    autonumber
    actor U
    participant FE
    participant API
    participant DB

    U->>FE: Brew again on KB-014
    FE->>API: GET /v1/batches/{id}/brew-again
    API->>DB: Load batch, its recipe version, fermentation type
    API-->>FE: {draft CreateBatchRequest with source batch, base, recipeVersion v3}
    loop On each edit (debounced 300 ms)
        U->>FE: Change sugar 53 to 60 g/L, write hypothesis
        FE->>API: POST /v1/batches/diff-preview {base: batch KB-014, draft}
        API->>API: Normalize both sides, run diff engine (no writes)
        API-->>FE: {rows, changeCount 1}
        FE-->>U: Changes vs KB-014 - sugar 53 to 60 g/L
    end
    U->>FE: Start batch
    FE->>API: POST /v1/batches {draft, source batch KB-014, hypothesis} + Idempotency-Key
    API->>DB: BEGIN
    API->>DB: Load source batch and its version v3 (owned, same type)
    API->>API: Compare draft recipe-level fields vs v3
    alt Recipe-level change (ingredient per litre or targetF1Days)
        API->>DB: SELECT recipe FOR UPDATE, max(version_no)
        API->>DB: INSERT recipe_versions v4 (parent v3, changeNote = hypothesis, sourceBatchId = new batch)
        API->>DB: UPDATE recipes.current_version_id = v4
        API->>DB: INSERT batch pinned to v4, snapshot = v4, parentBatchId KB-014
    else Only batch-level change (room temp, volume, vessel, starter source)
        API->>DB: INSERT batch pinned to v3, snapshot = v3, parentBatchId KB-014
    end
    API->>DB: INSERT batch_ingredients, code, reminders
    API->>DB: COMMIT
    API-->>FE: 201 Batch (recipeVersion v4 or v3)
    FE-->>U: Recipe v4 - 1 change
```

## 8. Compare experiments with the entitlement check

```mermaid
sequenceDiagram
    autonumber
    actor U
    participant FE
    participant API
    participant DB

    U->>FE: Select bottles A, B (KB-014) and batch KB-011, Compare
    FE->>API: POST /v1/compare {items (3), onlyDifferences true}
    API->>DB: Resolve entitlements (subscriptions, beta flag, grants)
    alt Free plan and items > 2
        API-->>FE: 402 entitlement_required (limit compareItems 2)
        FE-->>U: Upgrade sheet, selection kept
    else Allowed
        API->>DB: Load all items WHERE user_id (any miss = 404)
        API->>DB: Load ingredients, params, bottles, measurements (non-deleted), ratings
        API->>API: Build normalized snapshots, diff engine, filter differences
        API-->>FE: 200 CompareResult {items, rows}
    end
```

## 9. Export user data

```mermaid
sequenceDiagram
    autonumber
    actor U
    participant FE
    participant API
    participant DB
    participant J as Job runner
    participant S as Object storage
    participant M as Resend

    U->>FE: Export everything
    FE->>API: POST /v1/exports
    API->>DB: Rate limit check (3 per 24 h), INSERT exports queued, INSERT jobs build_export
    API-->>FE: 202 {id, status queued}
    J->>DB: Claim job FOR UPDATE SKIP LOCKED
    J->>DB: Read all user rows in a REPEATABLE READ snapshot
    J->>S: Stream ZIP (CSV per table, jarlore-export.json nested, schema.md)
    J->>DB: UPDATE exports ready, storage_path, expires_at = now + 7 days
    J->>M: Email: your export is ready
    loop Poll every 2 s while queued or running
        FE->>API: GET /v1/exports/{id}
        API-->>FE: {status}
    end
    FE->>API: GET /v1/exports/{id}
    API->>S: Create signed URL (15 min)
    API-->>FE: {status ready, downloadUrl}
    FE-->>U: Download starts
```

## 10. Subscription checkout and webhook synchronisation

```mermaid
sequenceDiagram
    autonumber
    actor U
    participant FE
    participant API
    participant DB
    participant MoR as MoR (Paddle)
    participant J as Job runner

    U->>FE: Upgrade, yearly
    FE->>API: POST /v1/billing/checkout {priceId}
    API->>API: priceId in allow-list, no active subscription
    API->>MoR: Create checkout or transaction (customData userId)
    MoR-->>API: checkoutUrl
    API-->>FE: {checkoutUrl}
    FE->>MoR: Open overlay or redirect
    U->>MoR: Pay
    MoR->>API: POST /webhooks/billing/paddle (signed raw body)
    API->>API: Verify signature and timestamp tolerance
    alt Invalid signature
        API-->>MoR: 401
    else Valid
        API->>DB: INSERT billing_events (provider, event id) ON CONFLICT DO NOTHING
        alt Duplicate
            API-->>MoR: 200 (already received)
        else New
            API->>DB: INSERT jobs process_billing_event
            API-->>MoR: 200
        end
    end
    J->>DB: Claim job, map provider status to subscriptions (upsert by provider subscription id)
    J->>DB: UPDATE billing_events processed_at, INSERT audit_events
    loop Up to 10 s
        FE->>API: GET /v1/entitlements
        API-->>FE: {plan pro, limits, source subscription}
    end
    FE-->>U: Return to the pending action (e.g. compare 3 items)
```

Out-of-order events are handled by comparing the provider's event timestamp (or subscription `updated_at`) with the stored one and ignoring older updates [Assumption].
