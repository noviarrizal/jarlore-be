-- name: InsertLog :one
INSERT INTO batch_logs (user_id, batch_id, bottle_id, kind, occurred_at, note)
VALUES (@user_id, @batch_id, sqlc.narg(bottle_id), @kind, @occurred_at, sqlc.narg(note))
RETURNING *;

-- name: InsertMeasurement :exec
INSERT INTO batch_measurements (
  user_id, log_id, batch_id, definition_id, position, value_num, value_bool, value_text, unit, value_canonical, method
) VALUES (
  @user_id, @log_id, @batch_id, @definition_id, @position, sqlc.narg(value_num), sqlc.narg(value_bool),
  sqlc.narg(value_text), @unit, sqlc.narg(value_canonical), sqlc.narg(method)
);

-- name: GetLog :one
SELECT * FROM batch_logs WHERE id = @id AND user_id = @user_id;

-- name: SoftDeleteLog :exec
UPDATE batch_logs SET deleted_at = now()
WHERE id = @id AND user_id = @user_id AND deleted_at IS NULL;

-- name: ListLogs :many
SELECT * FROM batch_logs
WHERE user_id = @user_id AND batch_id = @batch_id AND deleted_at IS NULL
  AND (sqlc.narg(bottle_id)::uuid IS NULL OR bottle_id = sqlc.narg(bottle_id))
  AND (sqlc.narg(after_time)::timestamptz IS NULL OR (occurred_at, id) < (sqlc.narg(after_time), sqlc.narg(after_id)::uuid))
ORDER BY occurred_at DESC, id DESC
LIMIT @lim;

-- name: LastLogsByBatchIDs :many
SELECT DISTINCT ON (batch_id) * FROM batch_logs
WHERE user_id = @user_id AND batch_id = ANY(@batch_ids::uuid[]) AND deleted_at IS NULL
ORDER BY batch_id, occurred_at DESC, id DESC;

-- name: ListMeasurementsByLogIDs :many
SELECT m.*, d.key AS definition_key
FROM batch_measurements m
JOIN measurement_definitions d ON d.id = m.definition_id
WHERE m.user_id = @user_id AND m.log_id = ANY(@log_ids::uuid[])
ORDER BY m.log_id, m.position;

-- name: ListReadingsByBatchIDs :many
-- Every visible reading of the given batches, oldest first: the source for
-- first/last values in comparisons and for Bottle.lastMeasurements.
SELECT m.*, d.key AS definition_key, l.bottle_id, l.occurred_at
FROM batch_measurements m
JOIN batch_logs l ON l.id = m.log_id AND l.user_id = m.user_id
JOIN measurement_definitions d ON d.id = m.definition_id
WHERE m.user_id = @user_id AND m.batch_id = ANY(@batch_ids::uuid[]) AND l.deleted_at IS NULL
ORDER BY l.occurred_at, l.id, m.position;
