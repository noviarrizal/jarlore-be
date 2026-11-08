-- name: EnsureProfile :one
-- Creates the profile on the first authenticated request and re-syncs the email claim.
INSERT INTO profiles (id, email)
VALUES (@id, sqlc.narg(email))
ON CONFLICT (id) DO UPDATE SET email = excluded.email, updated_at = now()
  WHERE profiles.email IS DISTINCT FROM excluded.email
RETURNING *;

-- name: GetProfile :one
SELECT * FROM profiles WHERE id = @id;

-- name: UpdateProfile :one
UPDATE profiles SET
  display_name = CASE WHEN @set_display_name::bool THEN sqlc.narg(display_name) ELSE display_name END,
  unit_system  = coalesce(sqlc.narg(unit_system), unit_system),
  temp_unit    = coalesce(sqlc.narg(temp_unit), temp_unit),
  timezone     = coalesce(sqlc.narg(timezone), timezone),
  updated_at   = now()
WHERE id = @id
RETURNING *;

-- name: GetActiveSubscription :one
-- Entitlement source: an active, trialing or past_due subscription, with three days'
-- grace after the period ends.
SELECT * FROM subscriptions
WHERE user_id = @user_id
  AND status IN ('trialing', 'active', 'past_due')
  AND (current_period_end IS NULL OR current_period_end + interval '3 days' > now())
ORDER BY current_period_end DESC NULLS FIRST
LIMIT 1;

-- name: ListFermentationTypes :many
SELECT * FROM fermentation_types ORDER BY sort_order;

-- name: GetFermentationType :one
SELECT * FROM fermentation_types WHERE id = @id;

-- name: ListMeasurementDefinitions :many
-- System definitions plus the user's own (RLS also limits this to those rows).
SELECT * FROM measurement_definitions
WHERE (user_id IS NULL OR user_id = @user_id) AND archived_at IS NULL
ORDER BY user_id NULLS FIRST, created_at, key;

-- name: ListAllMeasurementDefinitions :many
-- Including archived ones: old readings keep their labels.
SELECT * FROM measurement_definitions
WHERE user_id IS NULL OR user_id = @user_id
ORDER BY user_id NULLS FIRST, created_at, key;

-- name: InsertMeasurementDefinition :one
INSERT INTO measurement_definitions (user_id, key, label, kind, quantity, unit, decimals, min_value, max_value)
VALUES (@user_id, @key, @label, @kind, sqlc.narg(quantity), @unit, @decimals, sqlc.narg(min_value), sqlc.narg(max_value))
RETURNING *;

-- name: CountCustomDefinitions :one
SELECT count(*) FROM measurement_definitions WHERE user_id = @user_id AND archived_at IS NULL;

-- name: GetIdempotencyKey :one
SELECT * FROM idempotency_keys
WHERE user_id = @user_id AND key = @key AND created_at > now() - interval '24 hours';

-- name: InsertIdempotencyKey :exec
INSERT INTO idempotency_keys (user_id, key, operation, request_hash, status_code, response)
VALUES (@user_id, @key, @operation, @request_hash, @status_code, @response);

-- name: DeleteExpiredIdempotencyKey :exec
DELETE FROM idempotency_keys
WHERE user_id = @user_id AND key = @key AND created_at <= now() - interval '24 hours';
