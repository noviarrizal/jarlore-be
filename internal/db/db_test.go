package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"jarlore/internal/app"
	"jarlore/internal/auth"
	"jarlore/internal/db"
	"jarlore/internal/db/sqlc"
	"jarlore/internal/domain"
	"jarlore/internal/testdb"
)

// These tests talk to PostgreSQL directly, bypassing the application, to prove
// the database itself enforces ownership and history even if application code
// had a bug.

var (
	alice = auth.User{ID: "11111111-1111-4111-8111-111111111111"}
	bob   = auth.User{ID: "22222222-2222-4222-8222-222222222222"}
)

func ptr[T any](v T) *T { return &v }

type fixture struct {
	d                        *testdb.DB
	aliceBatch, aliceBatch2  app.Batch
	aliceBottle, otherBottle app.Bottle
	bobBatch                 app.Batch
	aliceVersion, bobVersion string
	bottleVersion            string
}

func seed(t *testing.T) fixture {
	t.Helper()
	d := testdb.New(t)
	now := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	svc := app.New(d.Store, app.Options{CursorSecret: []byte("x"), Now: func() time.Time { return now }})
	ctx := context.Background()
	must := func(res app.Result, err error) json.RawMessage {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return res.Body
	}
	var f fixture
	f.d = d
	draft := app.CreateBatchInput{FermentationTypeID: "kombucha", VolumeMl: 4000, Params: domain.BatchParams{},
		Ingredients: []domain.IngredientInput{{Name: "Cane sugar", Role: "sugar", Amount: 200, Unit: "g"}}}
	for _, u := range []auth.User{alice, bob} {
		if err := svc.EnsureProfile(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	_ = json.Unmarshal(must(svc.CreateBatch(ctx, alice, "", draft)), &f.aliceBatch)
	_ = json.Unmarshal(must(svc.CreateBatch(ctx, alice, "", draft)), &f.aliceBatch2)
	_ = json.Unmarshal(must(svc.CreateBatch(ctx, bob, "", draft)), &f.bobBatch)
	bottle := app.CreateBottlesInput{Bottles: []app.BottleInput{{Label: "A", FlavorName: "Ginger", VolumeMl: 500, Additions: []domain.Addition{}}}}
	var b app.Batch
	_ = json.Unmarshal(must(svc.CreateBottles(ctx, alice, f.aliceBatch.ID, "", bottle)), &b)
	f.aliceBottle = b.Bottles[0]
	_ = json.Unmarshal(must(svc.CreateBottles(ctx, alice, f.aliceBatch2.ID, "", bottle)), &b)
	f.otherBottle = b.Bottles[0]
	var r app.Recipe
	_ = json.Unmarshal(must(svc.SaveBatchAsRecipe(ctx, alice, f.aliceBatch.ID, "", app.SaveAsRecipeInput{Name: ptr("A")})), &r)
	f.aliceVersion = r.LatestVersion.ID
	_ = json.Unmarshal(must(svc.SaveBatchAsRecipe(ctx, bob, f.bobBatch.ID, "", app.SaveAsRecipeInput{Name: ptr("B")})), &r)
	f.bobVersion = r.LatestVersion.ID
	_ = json.Unmarshal(must(svc.SaveBottleAsRecipe(ctx, alice, f.aliceBottle.ID, "", app.SaveAsRecipeInput{Name: ptr("Bottle A")})), &r)
	f.bottleVersion = r.LatestVersion.ID
	return f
}

// asUser runs fn in a transaction of the runtime role with the given user set,
// exactly as the API does, and rolls back.
func asUser(t *testing.T, pool *pgxpool.Pool, userID string, fn func(tx pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if userID != "" {
		if _, err := tx.Exec(ctx, "SELECT set_config('jarlore.user_id', $1, true)", userID); err != nil {
			t.Fatal(err)
		}
	}
	return fn(tx)
}

func sqlstate(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

func wantState(t *testing.T, name string, err error, codes ...string) {
	t.Helper()
	got := sqlstate(err)
	for _, c := range codes {
		if got == c {
			return
		}
	}
	t.Errorf("%s: want SQLSTATE %v, got %q (%v)", name, codes, got, err)
}

func count(t *testing.T, tx pgx.Tx, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRowLevelSecurity(t *testing.T) {
	f := seed(t)
	ctx := context.Background()

	// Without a user, the runtime role sees nothing at all.
	_ = asUser(t, f.d.App, "", func(tx pgx.Tx) error {
		for _, table := range []string{"batches", "bottles", "recipes", "recipe_versions", "batch_logs", "reminders", "profiles"} {
			if n := count(t, tx, "SELECT count(*) FROM "+table); n != 0 {
				t.Errorf("%s: %d rows visible without a user", table, n)
			}
		}
		return nil
	})
	// Alice sees only her rows, even when asking for Bob's by id with no user filter.
	_ = asUser(t, f.d.App, alice.ID, func(tx pgx.Tx) error {
		if n := count(t, tx, "SELECT count(*) FROM batches WHERE id = $1", f.bobBatch.ID); n != 0 {
			t.Error("Alice can read Bob's batch through RLS")
		}
		if n := count(t, tx, "SELECT count(*) FROM recipe_versions WHERE id = $1", f.bobVersion); n != 0 {
			t.Error("Alice can read Bob's recipe version through RLS")
		}
		if n := count(t, tx, "SELECT count(*) FROM batches"); n != 2 {
			t.Errorf("Alice sees %d batches, want 2", n)
		}
		if n := count(t, tx, "SELECT count(*) FROM measurement_definitions"); n != 4 {
			t.Errorf("Alice sees %d definitions, want the 4 system ones", n)
		}
		return nil
	})
	// …and can't modify or create rows for Bob.
	_ = asUser(t, f.d.App, alice.ID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "UPDATE batches SET name = 'mine' WHERE id = $1", f.bobBatch.ID)
		if err != nil || tag.RowsAffected() != 0 {
			t.Errorf("update of Bob's batch affected %d rows (%v)", tag.RowsAffected(), err)
		}
		_, err = tx.Exec(ctx, `INSERT INTO batch_logs (user_id, batch_id, kind, occurred_at) VALUES ($1, $2, 'note', now())`, bob.ID, f.bobBatch.ID)
		wantState(t, "insert a log as Bob", err, "42501")
		return nil
	})
}

func TestTransactionUserDoesNotLeakAcrossPooledConnections(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(f.d.App.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1 // every transaction reuses the same physical connection
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store := &db.Store{Pool: pool}
	if err := store.UserReadTx(ctx, alice.ID, func(q *sqlc.Queries) error { return nil }); err != nil {
		t.Fatal(err)
	}
	// The same physical connection, next transaction, no user: nothing visible.
	for i := 0; i < 3; i++ {
		_ = asUser(t, pool, "", func(tx pgx.Tx) error {
			var uid *string
			if err := tx.QueryRow(ctx, "SELECT jarlore_user_id()::text").Scan(&uid); err != nil {
				t.Fatal(err)
			}
			if uid != nil {
				t.Fatalf("user %s leaked into another transaction", *uid)
			}
			return nil
		})
	}
}

func TestRuntimeRoleGrants(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	_ = asUser(t, f.d.App, alice.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO subscriptions (user_id, provider, provider_subscription_id, plan, status) VALUES ($1, 'paddle', 'sub_1', 'pro', 'active')`, alice.ID)
		wantState(t, "grant yourself Pro", err, "42501")
		return nil
	})
	_ = asUser(t, f.d.App, alice.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE recipe_versions SET change_note = 'edited' WHERE id = $1`, f.aliceVersion)
		wantState(t, "edit a recipe version", err, "42501")
		return nil
	})
	_ = asUser(t, f.d.App, alice.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE batch_logs SET note = 'rewritten' WHERE batch_id = $1`, f.aliceBatch.ID)
		wantState(t, "rewrite a log note", err, "42501")
		return nil
	})
	_ = asUser(t, f.d.App, alice.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM batches WHERE id = $1`, f.aliceBatch.ID)
		wantState(t, "delete a batch", err, "42501")
		return nil
	})
	_ = asUser(t, f.d.App, alice.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT * FROM billing_events`)
		wantState(t, "read billing events", err, "42501")
		return nil
	})
}

// Even the owner role (and any future buggy query) can't break history.
func TestIntegrityTriggers(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	exec := func(name, sql string, codes []string, args ...any) {
		t.Helper()
		tx, err := f.d.Owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		_, err = tx.Exec(ctx, sql, args...)
		wantState(t, name, err, codes...)
	}
	exec("update recipe version", `UPDATE recipe_versions SET change_note = 'x' WHERE id = $1`, []string{"JL001"}, f.aliceVersion)
	exec("delete recipe version", `DELETE FROM recipe_versions WHERE id = $1`, []string{"JL001"}, f.aliceVersion)
	exec("edit recipe ingredient", `UPDATE recipe_ingredients SET amount = 1 WHERE recipe_version_id = $1`, []string{"JL001"}, f.aliceVersion)
	exec("edit batch ingredient", `UPDATE batch_ingredients SET amount = 1 WHERE batch_id = $1`, []string{"JL004"}, f.aliceBatch.ID)
	exec("rewrite log", `UPDATE batch_logs SET note = 'x' WHERE batch_id = $1`, []string{"JL004"}, f.aliceBatch.ID)
	exec("delete batch", `DELETE FROM batches WHERE id = $1`, []string{"JL004"}, f.aliceBatch.ID)
	exec("unpin a batch from its version", `UPDATE batches SET recipe_version_id = NULL WHERE id = $1`, []string{"JL002"}, f.aliceBatch.ID)
	exec("rewrite a batch snapshot", `UPDATE batches SET recipe_snapshot = '{}' WHERE id = $1`, []string{"JL002", "23514"}, f.aliceBatch.ID)
	exec("move a bottle to another batch", `UPDATE bottles SET batch_id = $2 WHERE id = $1`, []string{"JL003", "23503"}, f.aliceBottle.ID, f.aliceBatch2.ID)

	// Closed records are frozen.
	if _, err := f.d.Owner.Exec(ctx, `UPDATE bottles SET status = 'finished', finished_at = now(), rating = '{"overall":4}' WHERE id = $1`, f.aliceBottle.ID); err != nil {
		t.Fatal(err)
	}
	exec("edit a finished bottle", `UPDATE bottles SET additions = '[]', flavor_name = 'Other' WHERE id = $1`, []string{"JL003"}, f.aliceBottle.ID)
	exec("re-rate a finished bottle", `UPDATE bottles SET rating = '{"overall":1}' WHERE id = $1`, []string{"JL003"}, f.aliceBottle.ID)
	if _, err := f.d.Owner.Exec(ctx, `UPDATE bottles SET is_reference = true WHERE id = $1`, f.aliceBottle.ID); err != nil {
		t.Errorf("marking a finished bottle as best must work: %v", err)
	}
}

func TestCompositeForeignKeys(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	exec := func(name, sql string, codes []string, args ...any) {
		t.Helper()
		tx, err := f.d.Owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		_, err = tx.Exec(ctx, sql, args...)
		wantState(t, name, err, codes...)
	}
	// A log can't name a bottle of a different batch, even of the same user.
	exec("log with another batch's bottle", `INSERT INTO batch_logs (user_id, batch_id, bottle_id, kind, occurred_at) VALUES ($1, $2, $3, 'note', now())`,
		[]string{"23503"}, alice.ID, f.aliceBatch2.ID, f.aliceBottle.ID)
	// Nothing of Alice's can point at Bob's rows.
	exec("Alice's log on Bob's batch", `INSERT INTO batch_logs (user_id, batch_id, kind, occurred_at) VALUES ($1, $2, 'note', now())`,
		[]string{"23503"}, alice.ID, f.bobBatch.ID)
	exec("Alice's batch on Bob's recipe version", `INSERT INTO batches (user_id, code, name, fermentation_type_id, stage, started_at, volume_ml, recipe_version_id) VALUES ($1, 'KB-900', 'x', 'kombucha', 'f1', now(), 1000, $2)`,
		[]string{"23503"}, alice.ID, f.bobVersion)
	exec("reminder on another batch's bottle", `INSERT INTO reminders (user_id, batch_id, bottle_id, kind, due_at, label) VALUES ($1, $2, $3, 'burp', now(), 'x')`,
		[]string{"23503"}, alice.ID, f.aliceBatch2.ID, f.aliceBottle.ID)
	// A batch can't be pinned to a bottle recipe, nor a bottle to a batch recipe.
	exec("batch on a bottle-scope version", `INSERT INTO batches (user_id, code, name, fermentation_type_id, stage, started_at, volume_ml, recipe_version_id) VALUES ($1, 'KB-901', 'x', 'kombucha', 'f1', now(), 1000, $2)`,
		[]string{"23503"}, alice.ID, f.bottleVersion)
	exec("bottle on a batch-scope version", `INSERT INTO bottles (user_id, batch_id, label, flavor_name, volume_ml, started_at, recipe_version_id) VALUES ($1, $2, 'Z', 'x', 500, now(), $3)`,
		[]string{"23503"}, alice.ID, f.aliceBatch.ID, f.aliceVersion)
	// A parent version always belongs to the same recipe.
	exec("parent version from another recipe", `INSERT INTO recipe_versions (user_id, recipe_id, scope, fermentation_type_id, version_no, parent_version_id, base_volume_ml, bottle_spec)
		SELECT user_id, recipe_id, scope, fermentation_type_id, 99, $2, 500, bottle_spec FROM recipe_versions WHERE id = $1`,
		[]string{"23503"}, f.bottleVersion, f.aliceVersion)
	// Bottle labels are unique within a batch.
	exec("duplicate bottle label", `INSERT INTO bottles (user_id, batch_id, label, flavor_name, volume_ml, started_at) VALUES ($1, $2, 'A', 'x', 500, now())`,
		[]string{"23505"}, alice.ID, f.aliceBatch.ID)
}
