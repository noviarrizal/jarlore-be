-- +goose Up
-- Jarlore core schema. Owner: the migration role (DIRECT_DATABASE_URL).
-- The API connects as a login role that is a member of jarlore_app (see 00002).
-- Every user-owned row carries user_id; child tables reference parents through
-- composite foreign keys that include user_id, so a row can never point at
-- another user's data, whatever the application does.

-- ---------------------------------------------------------------- reference data

CREATE TABLE fermentation_types (
  id                  text PRIMARY KEY,
  name                text NOT NULL,
  code_prefix         text NOT NULL CHECK (code_prefix ~ '^[A-Z]{2}$'),
  stages              jsonb NOT NULL,
  default_volume_ml   integer NOT NULL CHECK (default_volume_ml > 0),
  default_ingredients jsonb NOT NULL DEFAULT '[]',
  default_params      jsonb NOT NULL DEFAULT '{}',
  sort_order          smallint NOT NULL
);

-- ---------------------------------------------------------------- identity

CREATE TABLE profiles (
  id           uuid PRIMARY KEY,                -- the verified JWT sub
  email        text,
  display_name text CHECK (char_length(display_name) <= 80),
  unit_system  text NOT NULL DEFAULT 'metric' CHECK (unit_system IN ('metric', 'us')),
  temp_unit    text NOT NULL DEFAULT 'C' CHECK (temp_unit IN ('C', 'F')),
  timezone     text NOT NULL DEFAULT 'UTC' CHECK (char_length(timezone) BETWEEN 1 AND 64),
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------- measurements

CREATE TABLE measurement_definitions (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id         uuid REFERENCES profiles (id) ON DELETE CASCADE,   -- null = system definition
  key             text NOT NULL CHECK (key ~ '^[a-z0-9_]{2,40}$'),
  label           text NOT NULL CHECK (char_length(label) BETWEEN 1 AND 60),
  kind            text NOT NULL DEFAULT 'numeric' CHECK (kind IN ('numeric', 'scale', 'boolean', 'text')),
  -- Physical quantity for unit conversion: values are stored in the canonical unit.
  quantity        text CHECK (quantity IN ('temperature', 'volume', 'mass')),
  unit            text NOT NULL DEFAULT '' CHECK (char_length(unit) <= 16),
  decimals        smallint NOT NULL DEFAULT 0 CHECK (decimals BETWEEN 0 AND 4),
  min_value       numeric,
  max_value       numeric,
  requires_method boolean NOT NULL DEFAULT false,
  archived_at     timestamptz,
  created_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE NULLS NOT DISTINCT (user_id, key),
  CHECK (min_value IS NULL OR max_value IS NULL OR min_value <= max_value),
  CHECK (kind IN ('numeric', 'scale') OR (quantity IS NULL AND min_value IS NULL AND max_value IS NULL)),
  CHECK (kind <> 'scale' OR (min_value IS NOT NULL AND max_value IS NOT NULL))
);

-- ---------------------------------------------------------------- recipes

CREATE TABLE recipes (
  id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id              uuid NOT NULL REFERENCES profiles (id) ON DELETE CASCADE,
  -- batch: a base (F1 / single-stage) recipe. bottle: an F2 bottle recipe.
  scope                text NOT NULL CHECK (scope IN ('batch', 'bottle')),
  fermentation_type_id text NOT NULL REFERENCES fermentation_types (id),
  name                 text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),
  -- Version numbers are allocated by incrementing this column, which row-locks the recipe.
  latest_version_no    integer NOT NULL DEFAULT 0 CHECK (latest_version_no >= 0),
  archived_at          timestamptz,
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now(),
  UNIQUE (id, user_id),
  UNIQUE (id, user_id, scope, fermentation_type_id)
);
CREATE INDEX recipes_user_created_idx ON recipes (user_id, created_at DESC, id DESC);

CREATE TABLE recipe_versions (
  id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id              uuid NOT NULL,
  recipe_id            uuid NOT NULL,
  scope                text NOT NULL,
  fermentation_type_id text NOT NULL,
  version_no           integer NOT NULL CHECK (version_no >= 1),
  parent_version_id    uuid,
  change_note          text CHECK (char_length(change_note) <= 1000),
  source_batch_id      uuid,
  source_bottle_id     uuid,
  base_volume_ml       integer NOT NULL CHECK (base_volume_ml > 0),
  params               jsonb NOT NULL DEFAULT '{}',      -- recipe-level params (targetF1Days)
  default_bottles      jsonb NOT NULL DEFAULT '[]',      -- batch scope: suggested bottles
  bottle_spec          jsonb,                            -- bottle scope: flavorName, additions, primingSugarG, headspaceCm
  changed_fields       text[] NOT NULL DEFAULT '{}',     -- diff keys vs parent, cached at creation
  created_at           timestamptz NOT NULL DEFAULT now(),
  UNIQUE (recipe_id, version_no),
  UNIQUE (id, user_id),
  UNIQUE (id, recipe_id, user_id),
  UNIQUE (id, user_id, scope),
  UNIQUE (id, user_id, scope, fermentation_type_id),
  FOREIGN KEY (recipe_id, user_id, scope, fermentation_type_id)
    REFERENCES recipes (id, user_id, scope, fermentation_type_id) ON DELETE CASCADE,
  -- A parent version always belongs to the same recipe.
  FOREIGN KEY (parent_version_id, recipe_id, user_id) REFERENCES recipe_versions (id, recipe_id, user_id),
  CHECK ((scope = 'bottle') = (bottle_spec IS NOT NULL)),
  CHECK (parent_version_id IS NULL OR parent_version_id <> id)
);

CREATE TABLE recipe_ingredients (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id           uuid NOT NULL,
  recipe_version_id uuid NOT NULL,
  position          smallint NOT NULL CHECK (position >= 0),
  name              text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
  role              text NOT NULL CHECK (role IN ('water', 'tea', 'sugar', 'starter', 'flavor', 'other')),
  amount            numeric NOT NULL CHECK (amount >= 0),
  unit              text NOT NULL CHECK (char_length(unit) BETWEEN 1 AND 16),
  UNIQUE (recipe_version_id, position),
  FOREIGN KEY (recipe_version_id, user_id) REFERENCES recipe_versions (id, user_id) ON DELETE CASCADE
);

-- ---------------------------------------------------------------- batches (F1)

CREATE TABLE batch_code_counters (
  user_id uuid NOT NULL REFERENCES profiles (id) ON DELETE CASCADE,
  prefix  text NOT NULL,
  last_no integer NOT NULL CHECK (last_no >= 0),
  PRIMARY KEY (user_id, prefix)
);

CREATE TABLE batches (
  id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id              uuid NOT NULL REFERENCES profiles (id) ON DELETE CASCADE,
  code                 text NOT NULL,
  name                 text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),
  fermentation_type_id text NOT NULL REFERENCES fermentation_types (id),
  status               text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'finished', 'discarded')),
  stage                text NOT NULL CHECK (stage IN ('f1', 'f2', 'done')),
  started_at           timestamptz NOT NULL,
  bottled_at           timestamptz,
  finished_at          timestamptz,
  volume_ml            integer NOT NULL CHECK (volume_ml BETWEEN 1 AND 200000),
  vessel               text CHECK (char_length(vessel) <= 80),
  params               jsonb NOT NULL DEFAULT '{}',
  recipe_scope         text NOT NULL DEFAULT 'batch' CHECK (recipe_scope = 'batch'),
  recipe_version_id    uuid,
  recipe_snapshot      jsonb,
  parent_batch_id      uuid,
  hypothesis           text CHECK (char_length(hypothesis) <= 1000),
  changed_fields       text[] NOT NULL DEFAULT '{}',
  is_reference         boolean NOT NULL DEFAULT false,
  rating               jsonb,
  discard_reason       text CHECK (char_length(discard_reason) <= 500),
  archived_at          timestamptz,
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, code),
  UNIQUE (id, user_id),
  -- The pinned version is a batch-scope version of the same user and fermentation type.
  FOREIGN KEY (recipe_version_id, user_id, recipe_scope, fermentation_type_id)
    REFERENCES recipe_versions (id, user_id, scope, fermentation_type_id),
  FOREIGN KEY (parent_batch_id, user_id) REFERENCES batches (id, user_id),
  CHECK (parent_batch_id IS NULL OR parent_batch_id <> id),
  CHECK (finished_at IS NULL OR finished_at >= started_at),
  CHECK (bottled_at IS NULL OR bottled_at >= started_at),
  CHECK ((status = 'finished') = (stage = 'done')),
  CHECK ((status = 'active') = (finished_at IS NULL)),
  CHECK (status <> 'finished' OR rating IS NOT NULL),
  CHECK (NOT is_reference OR status = 'finished'),
  CHECK (archived_at IS NULL OR status <> 'active'),
  CHECK (recipe_snapshot IS NULL OR recipe_version_id IS NOT NULL),
  CHECK (rating IS NULL OR ((rating ->> 'overall')::int BETWEEN 1 AND 5))
);
CREATE INDEX batches_user_started_idx ON batches (user_id, started_at DESC, id DESC);
CREATE INDEX batches_user_updated_idx ON batches (user_id, updated_at DESC, id DESC);
CREATE INDEX batches_user_status_idx ON batches (user_id, status, stage);
CREATE INDEX batches_recipe_version_idx ON batches (recipe_version_id) WHERE recipe_version_id IS NOT NULL;

-- recipe_versions.source_batch_id points at the batch a version was saved from or created by.
-- The batch and its version are inserted in one transaction, so the check is deferred.
ALTER TABLE recipe_versions
  ADD FOREIGN KEY (source_batch_id, user_id) REFERENCES batches (id, user_id) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE batch_ingredients (
  id       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id  uuid NOT NULL,
  batch_id uuid NOT NULL,
  position smallint NOT NULL CHECK (position >= 0),
  name     text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
  role     text NOT NULL CHECK (role IN ('water', 'tea', 'sugar', 'starter', 'flavor', 'other')),
  amount   numeric NOT NULL CHECK (amount >= 0),
  unit     text NOT NULL CHECK (char_length(unit) BETWEEN 1 AND 16),
  UNIQUE (batch_id, position),
  FOREIGN KEY (batch_id, user_id) REFERENCES batches (id, user_id) ON DELETE CASCADE
);

-- ---------------------------------------------------------------- bottles (F2 experiments)

CREATE TABLE bottles (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id           uuid NOT NULL,
  batch_id          uuid NOT NULL,
  label             text NOT NULL CHECK (char_length(label) BETWEEN 1 AND 8),
  flavor_name       text NOT NULL CHECK (char_length(flavor_name) BETWEEN 1 AND 80),
  volume_ml         integer NOT NULL CHECK (volume_ml BETWEEN 1 AND 20000),
  additions         jsonb NOT NULL DEFAULT '[]',
  priming_sugar_g   numeric CHECK (priming_sugar_g BETWEEN 0 AND 1000),
  headspace_cm      numeric CHECK (headspace_cm BETWEEN 0 AND 50),
  status            text NOT NULL DEFAULT 'conditioning'
                      CHECK (status IN ('conditioning', 'chilled', 'finished', 'discarded')),
  started_at        timestamptz NOT NULL,
  chilled_at        timestamptz,
  finished_at       timestamptz,
  recipe_scope      text NOT NULL DEFAULT 'bottle' CHECK (recipe_scope = 'bottle'),
  recipe_version_id uuid,
  recipe_snapshot   jsonb,
  parent_bottle_id  uuid,
  hypothesis        text CHECK (char_length(hypothesis) <= 1000),
  changed_fields    text[] NOT NULL DEFAULT '{}',
  is_reference      boolean NOT NULL DEFAULT false,
  rating            jsonb,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE (batch_id, label),
  UNIQUE (id, user_id),
  UNIQUE (id, batch_id, user_id),
  FOREIGN KEY (batch_id, user_id) REFERENCES batches (id, user_id) ON DELETE CASCADE,
  FOREIGN KEY (recipe_version_id, user_id, recipe_scope) REFERENCES recipe_versions (id, user_id, scope),
  FOREIGN KEY (parent_bottle_id, user_id) REFERENCES bottles (id, user_id),
  CHECK (parent_bottle_id IS NULL OR parent_bottle_id <> id),
  CHECK (finished_at IS NULL OR finished_at >= started_at),
  CHECK ((status IN ('finished', 'discarded')) = (finished_at IS NOT NULL)),
  CHECK (NOT is_reference OR status = 'finished'),
  CHECK (recipe_snapshot IS NULL OR recipe_version_id IS NOT NULL),
  CHECK (rating IS NULL OR ((rating ->> 'overall')::int BETWEEN 1 AND 5))
);
CREATE INDEX bottles_batch_idx ON bottles (batch_id, label);
CREATE INDEX bottles_recipe_version_idx ON bottles (recipe_version_id) WHERE recipe_version_id IS NOT NULL;

-- recipe_versions(scope = bottle) points at the bottle it was saved from.
ALTER TABLE recipe_versions
  ADD FOREIGN KEY (source_bottle_id, user_id) REFERENCES bottles (id, user_id) DEFERRABLE INITIALLY DEFERRED;

-- ---------------------------------------------------------------- journal

CREATE TABLE batch_logs (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id         uuid NOT NULL,
  batch_id        uuid NOT NULL,
  bottle_id       uuid,
  kind            text NOT NULL CHECK (kind IN ('note', 'taste', 'burp', 'measure', 'stage_change', 'feed', 'issue')),
  occurred_at     timestamptz NOT NULL,
  note            text CHECK (char_length(note) <= 4000),
  deleted_at      timestamptz,
  created_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (id, user_id),
  UNIQUE (id, batch_id, user_id),
  FOREIGN KEY (batch_id, user_id) REFERENCES batches (id, user_id) ON DELETE CASCADE,
  -- A bottle log always names a bottle of the same batch.
  FOREIGN KEY (bottle_id, batch_id, user_id) REFERENCES bottles (id, batch_id, user_id) ON DELETE CASCADE
);
CREATE INDEX batch_logs_batch_idx ON batch_logs (batch_id, occurred_at DESC, id DESC) WHERE deleted_at IS NULL;
CREATE INDEX batch_logs_bottle_idx ON batch_logs (bottle_id, occurred_at DESC) WHERE bottle_id IS NOT NULL AND deleted_at IS NULL;

CREATE TABLE batch_measurements (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id         uuid NOT NULL,
  log_id          uuid NOT NULL,
  batch_id        uuid NOT NULL,
  definition_id   uuid NOT NULL REFERENCES measurement_definitions (id),
  position        smallint NOT NULL CHECK (position >= 0),
  value_num       numeric,
  value_bool      boolean,
  value_text      text CHECK (char_length(value_text) <= 500),
  unit            text NOT NULL DEFAULT '' CHECK (char_length(unit) <= 16),    -- as entered
  value_canonical numeric,                                                     -- numeric kinds, canonical unit
  method          text CHECK (method IN ('strip', 'meter', 'refractometer', 'hydrometer', 'thermometer', 'sensor', 'taste', 'estimate', 'other')),
  UNIQUE (log_id, position),
  -- The measurement's batch is its log's batch.
  FOREIGN KEY (log_id, batch_id, user_id) REFERENCES batch_logs (id, batch_id, user_id) ON DELETE CASCADE,
  CHECK (num_nonnulls(value_num, value_bool, value_text) = 1),
  CHECK (value_num IS NULL OR value_canonical IS NOT NULL)
);
CREATE INDEX batch_measurements_batch_idx ON batch_measurements (batch_id);

-- ---------------------------------------------------------------- reminders

CREATE TABLE reminders (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id      uuid NOT NULL,
  batch_id     uuid NOT NULL,
  bottle_id    uuid,
  kind         text NOT NULL CHECK (kind IN ('taste_check', 'bottle', 'burp', 'chill', 'custom')),
  due_at       timestamptz NOT NULL,
  status       text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'done', 'snoozed', 'dismissed')),
  label        text NOT NULL CHECK (char_length(label) BETWEEN 1 AND 120),
  completed_at timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (batch_id, user_id) REFERENCES batches (id, user_id) ON DELETE CASCADE,
  FOREIGN KEY (bottle_id, batch_id, user_id) REFERENCES bottles (id, batch_id, user_id) ON DELETE CASCADE,
  CHECK ((status IN ('done', 'dismissed')) = (completed_at IS NOT NULL))
);
CREATE INDEX reminders_open_idx ON reminders (user_id, due_at) WHERE status IN ('pending', 'snoozed');
CREATE INDEX reminders_batch_idx ON reminders (batch_id, due_at) WHERE status IN ('pending', 'snoozed');

-- ---------------------------------------------------------------- billing (written by webhook processing only)

CREATE TABLE subscriptions (
  id                       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id                  uuid NOT NULL REFERENCES profiles (id) ON DELETE CASCADE,
  provider                 text NOT NULL CHECK (provider IN ('paddle', 'creem', 'polar')),
  provider_subscription_id text NOT NULL,
  provider_customer_id     text,
  plan                     text NOT NULL CHECK (plan IN ('pro')),
  status                   text NOT NULL CHECK (status IN ('trialing', 'active', 'past_due', 'canceled', 'paused')),
  current_period_end       timestamptz,
  created_at               timestamptz NOT NULL DEFAULT now(),
  updated_at               timestamptz NOT NULL DEFAULT now(),
  UNIQUE (provider, provider_subscription_id)
);
CREATE INDEX subscriptions_user_idx ON subscriptions (user_id);

CREATE TABLE billing_events (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  provider          text NOT NULL,
  provider_event_id text NOT NULL,
  user_id           uuid REFERENCES profiles (id) ON DELETE SET NULL,
  payload           jsonb NOT NULL,
  received_at       timestamptz NOT NULL DEFAULT now(),
  processed_at      timestamptz,
  attempts          integer NOT NULL DEFAULT 0,
  last_error        text,
  UNIQUE (provider, provider_event_id)
);

-- ---------------------------------------------------------------- idempotency

CREATE TABLE idempotency_keys (
  user_id      uuid NOT NULL REFERENCES profiles (id) ON DELETE CASCADE,
  key          text NOT NULL CHECK (char_length(key) BETWEEN 8 AND 128),
  operation    text NOT NULL,
  request_hash text NOT NULL,
  status_code  integer NOT NULL,
  response     bytea NOT NULL,                 -- exact bytes, replayed verbatim
  created_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, key)
);

-- No Down section on purpose: rolling back would drop user data. Fix forward with a new migration.
