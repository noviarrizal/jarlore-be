-- name: InsertReminder :one
INSERT INTO reminders (user_id, batch_id, bottle_id, kind, due_at, label)
VALUES (@user_id, @batch_id, sqlc.narg(bottle_id), @kind, @due_at, @label)
RETURNING *;

-- name: NextRemindersByBatchIDs :many
SELECT DISTINCT ON (batch_id) * FROM reminders
WHERE user_id = @user_id AND batch_id = ANY(@batch_ids::uuid[]) AND status IN ('pending', 'snoozed')
ORDER BY batch_id, due_at, id;

-- name: ListDueReminders :many
SELECT r.* FROM reminders r
JOIN batches b ON b.id = r.batch_id AND b.user_id = r.user_id
WHERE r.user_id = @user_id AND r.status IN ('pending', 'snoozed') AND r.due_at < @before AND b.status = 'active'
ORDER BY r.due_at, r.id
LIMIT 100;

-- name: ListReminders :many
SELECT * FROM reminders
WHERE user_id = @user_id
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
  AND (sqlc.narg(batch_id)::uuid IS NULL OR batch_id = sqlc.narg(batch_id))
  AND (sqlc.narg(from_time)::timestamptz IS NULL OR due_at >= sqlc.narg(from_time))
  AND (sqlc.narg(to_time)::timestamptz IS NULL OR due_at < sqlc.narg(to_time))
ORDER BY due_at, id
LIMIT 200;

-- name: GetReminder :one
SELECT * FROM reminders WHERE id = @id AND user_id = @user_id;

-- name: UpdateReminder :one
UPDATE reminders SET
  status       = coalesce(sqlc.narg(status), status),
  due_at       = coalesce(sqlc.narg(due_at), due_at),
  label        = coalesce(sqlc.narg(label), label),
  completed_at = CASE WHEN sqlc.narg(status) IN ('done', 'dismissed') THEN now() ELSE completed_at END,
  updated_at   = now()
WHERE id = @id AND user_id = @user_id
RETURNING *;

-- name: CompleteDueReminders :exec
-- A log entry of the matching kind completes reminders that are due by the end of the day.
UPDATE reminders SET status = 'done', completed_at = now(), updated_at = now()
WHERE user_id = @user_id AND batch_id = @batch_id AND status IN ('pending', 'snoozed')
  AND kind = ANY(@kinds::text[]) AND due_at < @before
  AND (sqlc.narg(bottle_id)::uuid IS NULL OR bottle_id = sqlc.narg(bottle_id) OR bottle_id IS NULL);

-- name: DismissOpenReminders :exec
UPDATE reminders SET status = 'dismissed', completed_at = now(), updated_at = now()
WHERE user_id = @user_id AND batch_id = @batch_id AND status IN ('pending', 'snoozed')
  AND (sqlc.narg(bottle_id)::uuid IS NULL OR bottle_id = sqlc.narg(bottle_id))
  AND (cardinality(@kinds::text[]) = 0 OR kind = ANY(@kinds::text[]));

-- name: HasOpenReminder :one
SELECT EXISTS (
  SELECT 1 FROM reminders
  WHERE user_id = @user_id AND batch_id = @batch_id AND status IN ('pending', 'snoozed')
    AND kind = @kind AND bottle_id IS NOT DISTINCT FROM sqlc.narg(bottle_id)
) AS exists;
