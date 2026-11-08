-- name: InsertBottle :one
INSERT INTO bottles (
  id, user_id, batch_id, label, flavor_name, volume_ml, additions, priming_sugar_g, headspace_cm,
  started_at, recipe_version_id, recipe_snapshot, parent_bottle_id, hypothesis, changed_fields
) VALUES (
  @id, @user_id, @batch_id, @label, @flavor_name, @volume_ml, @additions, sqlc.narg(priming_sugar_g), sqlc.narg(headspace_cm),
  @started_at, sqlc.narg(recipe_version_id), sqlc.narg(recipe_snapshot), sqlc.narg(parent_bottle_id),
  sqlc.narg(hypothesis), @changed_fields
)
RETURNING *;

-- name: ListBottlesByBatchIDs :many
SELECT * FROM bottles
WHERE user_id = @user_id AND batch_id = ANY(@batch_ids::uuid[])
ORDER BY batch_id, started_at, label;

-- name: GetBottle :one
SELECT * FROM bottles WHERE id = @id AND user_id = @user_id;

-- name: GetBottleForUpdate :one
SELECT * FROM bottles WHERE id = @id AND user_id = @user_id FOR UPDATE;

-- name: GetBottlesByIDs :many
SELECT * FROM bottles WHERE user_id = @user_id AND id = ANY(@ids::uuid[]);

-- name: UpdateBottle :one
UPDATE bottles SET
  label           = coalesce(sqlc.narg(label), label),
  flavor_name     = coalesce(sqlc.narg(flavor_name), flavor_name),
  volume_ml       = coalesce(sqlc.narg(volume_ml), volume_ml),
  additions       = coalesce(sqlc.narg(additions), additions),
  priming_sugar_g = CASE WHEN @set_priming_sugar::bool THEN sqlc.narg(priming_sugar_g) ELSE priming_sugar_g END,
  headspace_cm    = CASE WHEN @set_headspace::bool THEN sqlc.narg(headspace_cm) ELSE headspace_cm END,
  is_reference    = coalesce(sqlc.narg(is_reference), is_reference),
  status          = coalesce(sqlc.narg(status), status),
  chilled_at      = CASE WHEN sqlc.narg(status) = 'chilled' AND chilled_at IS NULL THEN @now ELSE chilled_at END,
  finished_at     = CASE WHEN sqlc.narg(status) = 'discarded' THEN @now ELSE finished_at END
WHERE id = @id AND user_id = @user_id
RETURNING *;

-- name: ClearBottleReference :exec
UPDATE bottles SET is_reference = false
WHERE batch_id = @batch_id AND user_id = @user_id AND is_reference AND id <> @keep_id;

-- name: FinishBottle :one
UPDATE bottles SET status = 'finished', finished_at = coalesce(finished_at, @finished_at), rating = @rating
WHERE id = @id AND user_id = @user_id
RETURNING *;

-- name: CloseOpenBottles :exec
-- When a batch finishes or is discarded every open bottle closes with it.
UPDATE bottles SET status = @status, finished_at = @finished_at
WHERE batch_id = @batch_id AND user_id = @user_id AND status IN ('conditioning', 'chilled');

-- name: PinBottleToVersion :exec
UPDATE bottles SET recipe_version_id = @recipe_version_id
WHERE id = @id AND user_id = @user_id AND recipe_version_id IS NULL;

-- name: ListBottlesByRecipe :many
SELECT bo.* FROM bottles bo
JOIN recipe_versions v ON v.id = bo.recipe_version_id AND v.user_id = bo.user_id
WHERE bo.user_id = @user_id AND v.recipe_id = @recipe_id;
