-- +goose Up
-- Second line of defence behind the API: immutability triggers, row level security
-- and a restricted runtime role. Custom SQLSTATEs (JL0xx) are mapped to API error
-- codes in internal/db/errors.go.
--   JL001 recipe_version_immutable   JL002 snapshot_immutable
--   JL003 record_locked              JL004 append_only
--   JL005 invalid_reference

-- Rows may be removed only by the account-deletion job, which sets jarlore.purge = 'on'
-- for its transaction.

-- +goose StatementBegin
CREATE FUNCTION jarlore_purging() RETURNS boolean
LANGUAGE sql STABLE AS $$
  SELECT coalesce(current_setting('jarlore.purge', true), '') = 'on'
$$;
-- +goose StatementEnd

-- The authenticated user of the current transaction, set by the API with
-- set_config('jarlore.user_id', <sub>, true). Null outside a request transaction.
-- +goose StatementBegin
CREATE FUNCTION jarlore_user_id() RETURNS uuid
LANGUAGE sql STABLE AS $$
  SELECT nullif(current_setting('jarlore.user_id', true), '')::uuid
$$;
-- +goose StatementEnd

-- ---------------------------------------------------------------- immutable recipe history

-- +goose StatementBegin
CREATE FUNCTION recipe_history_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' AND jarlore_purging() THEN
    RETURN OLD;
  END IF;
  RAISE EXCEPTION '% rows are immutable', TG_TABLE_NAME USING ERRCODE = 'JL001';
END
$$;
-- +goose StatementEnd

CREATE TRIGGER recipe_versions_immutable BEFORE UPDATE OR DELETE ON recipe_versions
  FOR EACH ROW EXECUTE FUNCTION recipe_history_guard();
CREATE TRIGGER recipe_ingredients_immutable BEFORE UPDATE OR DELETE ON recipe_ingredients
  FOR EACH ROW EXECUTE FUNCTION recipe_history_guard();

-- ---------------------------------------------------------------- append-only records

-- +goose StatementBegin
CREATE FUNCTION append_only_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' AND jarlore_purging() THEN
    RETURN OLD;
  END IF;
  RAISE EXCEPTION '% rows are append-only', TG_TABLE_NAME USING ERRCODE = 'JL004';
END
$$;
-- +goose StatementEnd

CREATE TRIGGER batch_ingredients_append_only BEFORE UPDATE OR DELETE ON batch_ingredients
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();
CREATE TRIGGER batch_measurements_append_only BEFORE UPDATE OR DELETE ON batch_measurements
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

-- Log entries: the only change ever allowed is a soft delete (deleted_at null -> value).
-- +goose StatementBegin
CREATE FUNCTION batch_logs_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    IF jarlore_purging() THEN RETURN OLD; END IF;
    RAISE EXCEPTION 'batch_logs rows are append-only' USING ERRCODE = 'JL004';
  END IF;
  IF OLD.deleted_at IS NOT NULL
     OR NEW.deleted_at IS NULL
     OR (to_jsonb(NEW) - 'deleted_at') IS DISTINCT FROM (to_jsonb(OLD) - 'deleted_at') THEN
    RAISE EXCEPTION 'batch_logs rows only allow a soft delete' USING ERRCODE = 'JL004';
  END IF;
  RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER batch_logs_guard BEFORE UPDATE OR DELETE ON batch_logs
  FOR EACH ROW EXECUTE FUNCTION batch_logs_guard();

-- Measurements must use a system definition or one of the same user's definitions.
-- +goose StatementBegin
CREATE FUNCTION batch_measurements_definition_check() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM measurement_definitions d
    WHERE d.id = NEW.definition_id AND (d.user_id IS NULL OR d.user_id = NEW.user_id)
  ) THEN
    RAISE EXCEPTION 'measurement definition % is not available to this user', NEW.definition_id
      USING ERRCODE = 'JL005';
  END IF;
  RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER batch_measurements_definition_check BEFORE INSERT ON batch_measurements
  FOR EACH ROW EXECUTE FUNCTION batch_measurements_definition_check();

-- ---------------------------------------------------------------- batches

-- +goose StatementBegin
CREATE FUNCTION batches_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.user_id <> OLD.user_id OR NEW.code <> OLD.code
     OR NEW.fermentation_type_id <> OLD.fermentation_type_id
     OR NEW.parent_batch_id IS DISTINCT FROM OLD.parent_batch_id
     OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'batch identity fields are immutable' USING ERRCODE = 'JL003';
  END IF;

  -- The frozen snapshot is written once, at creation. The version pointer may only
  -- go from null to a value (Save as Recipe on a batch brewed without a recipe).
  IF NEW.recipe_snapshot IS DISTINCT FROM OLD.recipe_snapshot THEN
    RAISE EXCEPTION 'batch recipe snapshot is immutable' USING ERRCODE = 'JL002';
  END IF;
  IF OLD.recipe_version_id IS NOT NULL AND NEW.recipe_version_id IS DISTINCT FROM OLD.recipe_version_id THEN
    RAISE EXCEPTION 'batch recipe version is immutable once set' USING ERRCODE = 'JL002';
  END IF;

  -- Finished and discarded batches are history. Only name, vessel, isReference,
  -- params.roomTempC, archive state and the recipe pointer may change.
  IF OLD.status <> 'active' THEN
    IF NEW.status <> OLD.status OR NEW.stage <> OLD.stage
       OR NEW.started_at <> OLD.started_at
       OR NEW.bottled_at IS DISTINCT FROM OLD.bottled_at
       OR NEW.finished_at IS DISTINCT FROM OLD.finished_at
       OR NEW.volume_ml <> OLD.volume_ml
       OR (NEW.params - 'roomTempC') IS DISTINCT FROM (OLD.params - 'roomTempC')
       OR NEW.rating IS DISTINCT FROM OLD.rating
       OR NEW.hypothesis IS DISTINCT FROM OLD.hypothesis
       OR NEW.changed_fields IS DISTINCT FROM OLD.changed_fields
       OR NEW.discard_reason IS DISTINCT FROM OLD.discard_reason THEN
      RAISE EXCEPTION 'batch % is closed', OLD.code USING ERRCODE = 'JL003';
    END IF;
  END IF;

  NEW.updated_at := now();
  RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER batches_guard BEFORE UPDATE ON batches
  FOR EACH ROW EXECUTE FUNCTION batches_guard();

-- +goose StatementBegin
CREATE FUNCTION batches_no_delete() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF jarlore_purging() THEN RETURN OLD; END IF;
  RAISE EXCEPTION 'batches are archived or discarded, never deleted' USING ERRCODE = 'JL004';
END
$$;
-- +goose StatementEnd

CREATE TRIGGER batches_no_delete BEFORE DELETE ON batches
  FOR EACH ROW EXECUTE FUNCTION batches_no_delete();

-- ---------------------------------------------------------------- bottles

-- +goose StatementBegin
CREATE FUNCTION bottles_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  batch_started timestamptz;
BEGIN
  IF TG_OP = 'DELETE' THEN
    IF jarlore_purging() THEN RETURN OLD; END IF;
    RAISE EXCEPTION 'bottles are finished or discarded, never deleted' USING ERRCODE = 'JL004';
  END IF;

  IF TG_OP = 'INSERT' THEN
    SELECT started_at INTO batch_started FROM batches WHERE id = NEW.batch_id AND user_id = NEW.user_id;
    IF NEW.started_at < batch_started THEN
      RAISE EXCEPTION 'a bottle cannot start before its batch' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
  END IF;

  IF NEW.user_id <> OLD.user_id OR NEW.batch_id <> OLD.batch_id
     OR NEW.parent_bottle_id IS DISTINCT FROM OLD.parent_bottle_id
     OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'a bottle belongs to one batch for its whole life' USING ERRCODE = 'JL003';
  END IF;
  IF NEW.recipe_snapshot IS DISTINCT FROM OLD.recipe_snapshot THEN
    RAISE EXCEPTION 'bottle recipe snapshot is immutable' USING ERRCODE = 'JL002';
  END IF;
  IF OLD.recipe_version_id IS NOT NULL AND NEW.recipe_version_id IS DISTINCT FROM OLD.recipe_version_id THEN
    RAISE EXCEPTION 'bottle recipe version is immutable once set' USING ERRCODE = 'JL002';
  END IF;

  -- A finished or discarded bottle is a frozen experiment. Its configuration and
  -- dates never change. A bottle closed without a rating may be rated once later.
  IF OLD.status IN ('finished', 'discarded') THEN
    IF NEW.status <> OLD.status OR NEW.label <> OLD.label OR NEW.flavor_name <> OLD.flavor_name
       OR NEW.volume_ml <> OLD.volume_ml OR NEW.additions IS DISTINCT FROM OLD.additions
       OR NEW.priming_sugar_g IS DISTINCT FROM OLD.priming_sugar_g
       OR NEW.headspace_cm IS DISTINCT FROM OLD.headspace_cm
       OR NEW.started_at <> OLD.started_at
       OR NEW.chilled_at IS DISTINCT FROM OLD.chilled_at
       OR NEW.finished_at IS DISTINCT FROM OLD.finished_at
       OR NEW.hypothesis IS DISTINCT FROM OLD.hypothesis
       OR NEW.changed_fields IS DISTINCT FROM OLD.changed_fields
       OR (OLD.rating IS NOT NULL AND NEW.rating IS DISTINCT FROM OLD.rating) THEN
      RAISE EXCEPTION 'bottle % is closed', OLD.label USING ERRCODE = 'JL003';
    END IF;
  END IF;

  NEW.updated_at := now();
  RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER bottles_guard BEFORE INSERT OR UPDATE OR DELETE ON bottles
  FOR EACH ROW EXECUTE FUNCTION bottles_guard();

-- ---------------------------------------------------------------- row level security

-- The runtime role. The API's login role (created per environment, see
-- docs/deployment/NEON_SETUP.md) is granted membership in it. It owns nothing,
-- has no BYPASSRLS, and gets only the privileges listed here.
-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'jarlore_app') THEN
    CREATE ROLE jarlore_app NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
  END IF;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO jarlore_app;
GRANT EXECUTE ON FUNCTION jarlore_user_id(), jarlore_purging() TO jarlore_app;

GRANT SELECT ON fermentation_types TO jarlore_app;
GRANT SELECT, INSERT, UPDATE ON profiles TO jarlore_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON measurement_definitions TO jarlore_app;
GRANT SELECT, INSERT, UPDATE ON recipes TO jarlore_app;
GRANT SELECT, INSERT ON recipe_versions, recipe_ingredients TO jarlore_app;
GRANT SELECT, INSERT, UPDATE ON batch_code_counters TO jarlore_app;
GRANT SELECT, INSERT, UPDATE ON batches TO jarlore_app;
GRANT SELECT, INSERT ON batch_ingredients TO jarlore_app;
GRANT SELECT, INSERT, UPDATE ON bottles TO jarlore_app;
GRANT SELECT, INSERT ON batch_logs TO jarlore_app;
GRANT UPDATE (deleted_at) ON batch_logs TO jarlore_app;
GRANT SELECT, INSERT ON batch_measurements TO jarlore_app;
GRANT SELECT, INSERT, UPDATE ON reminders TO jarlore_app;
GRANT SELECT, INSERT, DELETE ON idempotency_keys TO jarlore_app;
-- Plan state is read-only for the API's request path: it can't grant itself Pro.
GRANT SELECT ON subscriptions TO jarlore_app;
-- billing_events: no privileges for the runtime role.

ALTER TABLE profiles ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_profile ON profiles
  USING (id = jarlore_user_id()) WITH CHECK (id = jarlore_user_id());

ALTER TABLE measurement_definitions ENABLE ROW LEVEL SECURITY;
CREATE POLICY read_system_or_own ON measurement_definitions FOR SELECT
  USING (user_id IS NULL OR user_id = jarlore_user_id());
CREATE POLICY insert_own ON measurement_definitions FOR INSERT
  WITH CHECK (user_id = jarlore_user_id());
CREATE POLICY update_own ON measurement_definitions FOR UPDATE
  USING (user_id = jarlore_user_id()) WITH CHECK (user_id = jarlore_user_id());
CREATE POLICY delete_own ON measurement_definitions FOR DELETE
  USING (user_id = jarlore_user_id());

-- +goose StatementBegin
DO $$
DECLARE
  t text;
BEGIN
  FOREACH t IN ARRAY ARRAY[
    'recipes', 'recipe_versions', 'recipe_ingredients', 'batch_code_counters', 'batches',
    'batch_ingredients', 'bottles', 'batch_logs', 'batch_measurements', 'reminders',
    'idempotency_keys', 'subscriptions'
  ] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format(
      'CREATE POLICY own_rows ON %I USING (user_id = jarlore_user_id()) WITH CHECK (user_id = jarlore_user_id())', t);
  END LOOP;
END
$$;
-- +goose StatementEnd

ALTER TABLE billing_events ENABLE ROW LEVEL SECURITY;
