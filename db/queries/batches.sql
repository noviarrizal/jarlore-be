-- Every query names user_id explicitly, in addition to row level security.

-- name: NextBatchCodeNo :one
INSERT INTO batch_code_counters (user_id, prefix, last_no)
VALUES (@user_id, @prefix, 1)
ON CONFLICT (user_id, prefix) DO UPDATE SET last_no = batch_code_counters.last_no + 1
RETURNING last_no;

-- name: InsertBatch :one
INSERT INTO batches (
  id, user_id, code, name, fermentation_type_id, status, stage, started_at, volume_ml, vessel,
  params, recipe_version_id, recipe_snapshot, parent_batch_id, hypothesis, changed_fields
) VALUES (
  @id, @user_id, @code, @name, @fermentation_type_id, 'active', 'f1', @started_at, @volume_ml, sqlc.narg(vessel),
  @params, sqlc.narg(recipe_version_id), sqlc.narg(recipe_snapshot), sqlc.narg(parent_batch_id),
  sqlc.narg(hypothesis), @changed_fields
)
RETURNING *;

-- name: InsertBatchIngredients :batchexec
INSERT INTO batch_ingredients (user_id, batch_id, position, name, role, amount, unit)
VALUES (@user_id, @batch_id, @position, @name, @role, @amount, @unit);

-- name: GetBatch :one
SELECT * FROM batches WHERE id = @id AND user_id = @user_id;

-- name: GetBatchForUpdate :one
SELECT * FROM batches WHERE id = @id AND user_id = @user_id FOR UPDATE;

-- name: GetBatchesByIDs :many
SELECT * FROM batches WHERE user_id = @user_id AND id = ANY(@ids::uuid[]);

-- name: ListBatchIngredients :many
SELECT * FROM batch_ingredients
WHERE user_id = @user_id AND batch_id = ANY(@batch_ids::uuid[])
ORDER BY batch_id, position;

-- name: CountBottlesByBatch :many
SELECT batch_id, count(*)::int AS n FROM bottles
WHERE user_id = @user_id AND batch_id = ANY(@batch_ids::uuid[])
GROUP BY batch_id;

-- Batch lists. One query per sort order keeps each one index-friendly and readable.
-- Filters are optional; the cursor is (sort key, id) of the last row of the previous page.

-- name: ListBatchesStartedDesc :many
SELECT * FROM batches
WHERE batches.user_id = @user_id
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
  AND (sqlc.narg(stage)::text IS NULL OR stage = sqlc.narg(stage))
  AND (sqlc.narg(type)::text IS NULL OR fermentation_type_id = sqlc.narg(type))
  AND (sqlc.narg(recipe_id)::uuid IS NULL OR recipe_version_id IN (SELECT v.id FROM recipe_versions v WHERE v.recipe_id = sqlc.narg(recipe_id) AND v.user_id = batches.user_id))
  AND (sqlc.narg(q)::text IS NULL OR name ILIKE '%' || sqlc.narg(q) || '%' OR code ILIKE '%' || sqlc.narg(q) || '%' OR hypothesis ILIKE '%' || sqlc.narg(q) || '%')
  AND ((@archived::bool AND archived_at IS NOT NULL) OR (NOT @archived::bool AND archived_at IS NULL))
  AND (sqlc.narg(after_time)::timestamptz IS NULL OR (started_at, id) < (sqlc.narg(after_time), sqlc.narg(after_id)::uuid))
ORDER BY started_at DESC, id DESC
LIMIT @lim;

-- name: ListBatchesStartedAsc :many
SELECT * FROM batches
WHERE batches.user_id = @user_id
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
  AND (sqlc.narg(stage)::text IS NULL OR stage = sqlc.narg(stage))
  AND (sqlc.narg(type)::text IS NULL OR fermentation_type_id = sqlc.narg(type))
  AND (sqlc.narg(recipe_id)::uuid IS NULL OR recipe_version_id IN (SELECT v.id FROM recipe_versions v WHERE v.recipe_id = sqlc.narg(recipe_id) AND v.user_id = batches.user_id))
  AND (sqlc.narg(q)::text IS NULL OR name ILIKE '%' || sqlc.narg(q) || '%' OR code ILIKE '%' || sqlc.narg(q) || '%' OR hypothesis ILIKE '%' || sqlc.narg(q) || '%')
  AND ((@archived::bool AND archived_at IS NOT NULL) OR (NOT @archived::bool AND archived_at IS NULL))
  AND (sqlc.narg(after_time)::timestamptz IS NULL OR (started_at, id) > (sqlc.narg(after_time), sqlc.narg(after_id)::uuid))
ORDER BY started_at ASC, id ASC
LIMIT @lim;

-- name: ListBatchesUpdatedDesc :many
SELECT * FROM batches
WHERE batches.user_id = @user_id
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
  AND (sqlc.narg(stage)::text IS NULL OR stage = sqlc.narg(stage))
  AND (sqlc.narg(type)::text IS NULL OR fermentation_type_id = sqlc.narg(type))
  AND (sqlc.narg(recipe_id)::uuid IS NULL OR recipe_version_id IN (SELECT v.id FROM recipe_versions v WHERE v.recipe_id = sqlc.narg(recipe_id) AND v.user_id = batches.user_id))
  AND (sqlc.narg(q)::text IS NULL OR name ILIKE '%' || sqlc.narg(q) || '%' OR code ILIKE '%' || sqlc.narg(q) || '%' OR hypothesis ILIKE '%' || sqlc.narg(q) || '%')
  AND ((@archived::bool AND archived_at IS NOT NULL) OR (NOT @archived::bool AND archived_at IS NULL))
  AND (sqlc.narg(after_time)::timestamptz IS NULL OR (updated_at, id) < (sqlc.narg(after_time), sqlc.narg(after_id)::uuid))
ORDER BY updated_at DESC, id DESC
LIMIT @lim;

-- name: ListBatchesRatingDesc :many
-- Unrated batches sort last (score 0).
SELECT * FROM batches
WHERE batches.user_id = @user_id
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
  AND (sqlc.narg(stage)::text IS NULL OR stage = sqlc.narg(stage))
  AND (sqlc.narg(type)::text IS NULL OR fermentation_type_id = sqlc.narg(type))
  AND (sqlc.narg(recipe_id)::uuid IS NULL OR recipe_version_id IN (SELECT v.id FROM recipe_versions v WHERE v.recipe_id = sqlc.narg(recipe_id) AND v.user_id = batches.user_id))
  AND (sqlc.narg(q)::text IS NULL OR name ILIKE '%' || sqlc.narg(q) || '%' OR code ILIKE '%' || sqlc.narg(q) || '%' OR hypothesis ILIKE '%' || sqlc.narg(q) || '%')
  AND ((@archived::bool AND archived_at IS NOT NULL) OR (NOT @archived::bool AND archived_at IS NULL))
  AND (sqlc.narg(after_score)::int IS NULL
       OR (coalesce((rating ->> 'overall')::int, 0), id) < (sqlc.narg(after_score)::int, sqlc.narg(after_id)::uuid))
ORDER BY coalesce((rating ->> 'overall')::int, 0) DESC, id DESC
LIMIT @lim;

-- name: ListActiveBatches :many
SELECT * FROM batches
WHERE user_id = @user_id AND status = 'active' AND archived_at IS NULL
ORDER BY started_at DESC, id DESC
LIMIT 100;

-- name: ListRecentlyFinished :many
SELECT * FROM batches
WHERE user_id = @user_id AND status = 'finished' AND archived_at IS NULL AND finished_at > @since
ORDER BY finished_at DESC, id DESC
LIMIT 5;

-- name: UpdateBatchDetails :one
UPDATE batches SET
  name         = coalesce(sqlc.narg(name), name),
  vessel       = CASE WHEN @set_vessel::bool THEN sqlc.narg(vessel) ELSE vessel END,
  is_reference = coalesce(sqlc.narg(is_reference), is_reference),
  params       = CASE WHEN @set_room_temp::bool THEN
                   CASE WHEN sqlc.narg(room_temp_c)::numeric IS NULL THEN params - 'roomTempC'
                        ELSE jsonb_set(params, '{roomTempC}', to_jsonb(sqlc.narg(room_temp_c)::numeric)) END
                 ELSE params END
WHERE id = @id AND user_id = @user_id
RETURNING *;

-- name: MoveBatchToF2 :exec
UPDATE batches SET stage = 'f2', bottled_at = @bottled_at
WHERE id = @id AND user_id = @user_id AND status = 'active' AND stage = 'f1';

-- name: TouchBatch :exec
UPDATE batches SET updated_at = now() WHERE id = @id AND user_id = @user_id;

-- name: FinishBatch :one
UPDATE batches SET status = 'finished', stage = 'done', finished_at = @finished_at, rating = @rating
WHERE id = @id AND user_id = @user_id AND status = 'active'
RETURNING *;

-- name: DiscardBatch :one
UPDATE batches SET status = 'discarded', finished_at = @finished_at, discard_reason = @reason
WHERE id = @id AND user_id = @user_id AND status = 'active'
RETURNING *;

-- name: SetBatchArchived :one
UPDATE batches SET archived_at = sqlc.narg(archived_at)
WHERE id = @id AND user_id = @user_id
RETURNING *;

-- name: PinBatchToVersion :exec
UPDATE batches SET recipe_version_id = @recipe_version_id
WHERE id = @id AND user_id = @user_id AND recipe_version_id IS NULL;

-- name: FindReferenceBatch :one
-- "Last good batch" for comparison (DOMAIN_MODEL.md 8.6). mode 1: marked reference,
-- mode 2: overall >= 4. Scope by recipe or by fermentation type.
SELECT * FROM batches b
WHERE b.user_id = @user_id AND b.id <> @exclude_id AND b.status = 'finished' AND b.started_at < @before
  AND (sqlc.narg(recipe_id)::uuid IS NULL OR b.recipe_version_id IN (SELECT v.id FROM recipe_versions v WHERE v.recipe_id = sqlc.narg(recipe_id) AND v.user_id = @user_id))
  AND b.fermentation_type_id = @fermentation_type_id
  AND ((@marked::bool AND b.is_reference) OR (NOT @marked::bool AND (b.rating ->> 'overall')::int >= 4))
ORDER BY b.started_at DESC, b.id DESC
LIMIT 1;
