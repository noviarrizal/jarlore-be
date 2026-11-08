-- name: InsertRecipe :one
INSERT INTO recipes (user_id, scope, fermentation_type_id, name)
VALUES (@user_id, @scope, @fermentation_type_id, @name)
RETURNING *;

-- name: GetRecipe :one
SELECT * FROM recipes WHERE id = @id AND user_id = @user_id;

-- name: GetRecipesByIDs :many
SELECT * FROM recipes WHERE user_id = @user_id AND id = ANY(@ids::uuid[]);

-- name: ListRecipes :many
SELECT * FROM recipes
WHERE user_id = @user_id
  AND (sqlc.narg(scope)::text IS NULL OR scope = sqlc.narg(scope))
  AND (sqlc.narg(type)::text IS NULL OR fermentation_type_id = sqlc.narg(type))
  AND (sqlc.narg(q)::text IS NULL OR name ILIKE '%' || sqlc.narg(q) || '%')
  AND ((@archived::bool AND archived_at IS NOT NULL) OR (NOT @archived::bool AND archived_at IS NULL))
  AND (sqlc.narg(after_time)::timestamptz IS NULL OR (created_at, id) < (sqlc.narg(after_time), sqlc.narg(after_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT @lim;

-- name: UpdateRecipe :one
UPDATE recipes SET
  name        = coalesce(sqlc.narg(name), name),
  archived_at = CASE WHEN @set_archived::bool THEN sqlc.narg(archived_at) ELSE archived_at END,
  updated_at  = now()
WHERE id = @id AND user_id = @user_id
RETURNING *;

-- name: AllocateVersionNo :one
-- Increments the counter under a row lock: concurrent saves get distinct, gap-free numbers.
UPDATE recipes SET latest_version_no = latest_version_no + 1, updated_at = now()
WHERE id = @id AND user_id = @user_id
RETURNING latest_version_no;

-- name: InsertRecipeVersion :one
INSERT INTO recipe_versions (
  id, user_id, recipe_id, scope, fermentation_type_id, version_no, parent_version_id, change_note,
  source_batch_id, source_bottle_id, base_volume_ml, params, default_bottles, bottle_spec, changed_fields
) VALUES (
  @id, @user_id, @recipe_id, @scope, @fermentation_type_id, @version_no, sqlc.narg(parent_version_id), sqlc.narg(change_note),
  sqlc.narg(source_batch_id), sqlc.narg(source_bottle_id), @base_volume_ml, @params, @default_bottles,
  sqlc.narg(bottle_spec), @changed_fields
)
RETURNING *;

-- name: InsertRecipeIngredients :batchexec
INSERT INTO recipe_ingredients (user_id, recipe_version_id, position, name, role, amount, unit)
VALUES (@user_id, @recipe_version_id, @position, @name, @role, @amount, @unit);

-- name: GetRecipeVersion :one
SELECT * FROM recipe_versions WHERE id = @id AND user_id = @user_id;

-- name: GetRecipeVersionByNo :one
SELECT * FROM recipe_versions WHERE recipe_id = @recipe_id AND version_no = @version_no AND user_id = @user_id;

-- name: GetRecipeVersionsByIDs :many
SELECT * FROM recipe_versions WHERE user_id = @user_id AND id = ANY(@ids::uuid[]);

-- name: GetLatestVersions :many
SELECT v.* FROM recipe_versions v
JOIN recipes r ON r.id = v.recipe_id AND r.user_id = v.user_id AND v.version_no = r.latest_version_no
WHERE v.user_id = @user_id AND v.recipe_id = ANY(@recipe_ids::uuid[]);

-- name: ListRecipeVersions :many
SELECT * FROM recipe_versions
WHERE recipe_id = @recipe_id AND user_id = @user_id
  AND (sqlc.narg(before_no)::int IS NULL OR version_no < sqlc.narg(before_no))
ORDER BY version_no DESC
LIMIT @lim;

-- name: ListRecipeIngredients :many
SELECT * FROM recipe_ingredients
WHERE user_id = @user_id AND recipe_version_id = ANY(@version_ids::uuid[])
ORDER BY recipe_version_id, position;

-- name: VersionUseStats :many
-- Batches pinned to each version (batch scope) or bottles (bottle scope), with the
-- mean overall rating of the finished ones.
SELECT recipe_version_id::uuid AS version_id,
       count(DISTINCT batch_id)::int AS batch_count,
       coalesce(sum(overall), 0)::int AS overall_sum,
       count(overall)::int AS rated_count
FROM (
  SELECT b.recipe_version_id, b.id AS batch_id, (b.rating ->> 'overall')::int AS overall
  FROM batches b
  WHERE b.user_id = @user_id AND b.recipe_version_id = ANY(@version_ids::uuid[])
  UNION ALL
  SELECT bo.recipe_version_id, bo.batch_id, (bo.rating ->> 'overall')::int AS overall
  FROM bottles bo
  WHERE bo.user_id = @user_id AND bo.recipe_version_id = ANY(@version_ids::uuid[])
) uses
GROUP BY recipe_version_id;

-- name: ListRecipeBatches :many
-- Batches brewed from any version of a batch-scope recipe, or containing a bottle
-- brewed from a bottle-scope recipe.
SELECT b.* FROM batches b
WHERE b.user_id = @user_id
  AND (b.recipe_version_id IN (SELECT v.id FROM recipe_versions v WHERE v.recipe_id = @recipe_id AND v.user_id = @user_id)
       OR b.id IN (SELECT bo.batch_id FROM bottles bo JOIN recipe_versions v ON v.id = bo.recipe_version_id
                   WHERE v.recipe_id = @recipe_id AND bo.user_id = @user_id))
  AND (sqlc.narg(after_time)::timestamptz IS NULL OR (b.started_at, b.id) < (sqlc.narg(after_time), sqlc.narg(after_id)::uuid))
ORDER BY b.started_at DESC, b.id DESC
LIMIT @lim;
