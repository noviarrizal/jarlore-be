package app_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"jarlore/internal/app"
	"jarlore/internal/apperr"
	"jarlore/internal/auth"
	"jarlore/internal/domain"
	"jarlore/internal/testdb"
)

var (
	alice = auth.User{ID: "11111111-1111-4111-8111-111111111111", Email: ptr("alice@example.com")}
	bob   = auth.User{ID: "22222222-2222-4222-8222-222222222222", Email: ptr("bob@example.com")}
)

func ptr[T any](v T) *T { return &v }

// clock is a controllable "now" so the scenario can span days.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func setup(t *testing.T) (*app.Service, *clock, context.Context, *testdb.DB) {
	t.Helper()
	d := testdb.New(t)
	c := &clock{t: time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)}
	svc := app.New(d.Store, app.Options{CursorSecret: []byte("test-secret"), Now: c.now})
	ctx := context.Background()
	for _, u := range []auth.User{alice, bob} {
		if err := svc.EnsureProfile(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	return svc, c, ctx, d
}

// decode returns a function so it can take a use case's two results directly:
// decode[app.Batch](t)(svc.CreateBatch(...)).
func decode[T any](t *testing.T) func(app.Result, error) T {
	return func(res app.Result, err error) T {
		t.Helper()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var v T
		if err := json.Unmarshal(res.Body, &v); err != nil {
			t.Fatalf("decode %T: %v", v, err)
		}
		return v
	}
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	e, ok := apperr.As(err)
	if !ok || e.Code != code {
		t.Fatalf("want error code %q, got %v", code, err)
	}
}

func rating(overall int, note string) *domain.Rating {
	return &domain.Rating{Overall: overall, Fizz: ptr(overall), OffNotes: []string{}, WouldRepeat: ptr(overall >= 4), Note: &note}
}

func ginger(form string, amount float64, unit string) []domain.Addition {
	return []domain.Addition{{Name: "Ginger", Form: form, Amount: &amount, Unit: &unit}}
}

func kombuchaDraft() app.CreateBatchInput {
	return app.CreateBatchInput{
		FermentationTypeID: "kombucha", Name: ptr("Ginger kombucha"), VolumeMl: 4200,
		Ingredients: []domain.IngredientInput{
			{Name: "Filtered water", Role: "water", Amount: 3.8, Unit: "L"},
			{Name: "Black tea", Role: "tea", Amount: 12, Unit: "g"},
			{Name: "Cane sugar", Role: "sugar", Amount: 228, Unit: "g"},
			{Name: "Starter tea", Role: "starter", Amount: 400, Unit: "ml"},
		},
		Params: domain.BatchParams{TargetF1Days: ptr(9.0), RoomTempC: ptr(23.0)},
	}
}

func findRow(rows []domain.Row, key string) *domain.Row {
	for i := range rows {
		if rows[i].Key == key {
			return &rows[i]
		}
	}
	return nil
}

// TestAcceptanceScenario is the main end-to-end scenario from the brief, on real
// persisted data: F1 -> four F2 bottles -> rate -> save bottle A as an F2 recipe
// -> brew a new bottle from it with a change -> diff -> finish -> compare, and
// history stays unchanged throughout.
func TestAcceptanceScenario(t *testing.T) {
	svc, clk, ctx, _ := setup(t)

	// 2-3. A 4.2 L ginger kombucha F1 with its ingredients.
	kb := decode[app.Batch](t)(svc.CreateBatch(ctx, alice, "create-kb-0001", kombuchaDraft()))
	if kb.Code != "KB-001" || kb.Stage != "f1" || len(kb.Ingredients) != 4 || kb.VolumeMl != 4200 {
		t.Fatalf("created batch = %+v", kb)
	}
	if kb.NextReminder == nil || kb.NextReminder.Kind != "taste_check" {
		t.Fatalf("a taste check is scheduled at the target F1 length, got %+v", kb.NextReminder)
	}

	// 6-7. F1 observations with pH (method required) and temperature in °F.
	clk.add(7 * 24 * time.Hour)
	_, err := svc.CreateLogs(ctx, alice, kb.ID, "log-f1-ph-0001", app.CreateLogInput{Kind: "measure", Measurements: []domain.Measurement{
		{DefinitionKey: "ph", Value: domain.NumValue(3.2), Unit: "pH"},
	}})
	wantCode(t, err, "validation_failed") // pH without a method is refused
	logs := decode[map[string][]app.LogEntry](t)(svc.CreateLogs(ctx, alice, kb.ID, "log-f1-ph-0002", app.CreateLogInput{
		Kind: "taste", Note: ptr("Tart, nearly there"), Measurements: []domain.Measurement{
			{DefinitionKey: "ph", Value: domain.NumValue(3.1), Unit: "pH", Method: ptr("strip")},
			{DefinitionKey: "temp", Value: domain.NumValue(73.4), Unit: "°F", Method: ptr("thermometer")},
		}}))
	if len(logs["data"]) != 1 || len(logs["data"][0].Measurements) != 2 {
		t.Fatalf("log = %+v", logs)
	}

	// 4-5. Split into four F2 bottles, each its own experiment.
	clk.add(2 * 24 * time.Hour)
	bottles := []app.BottleInput{
		{Label: "A", FlavorName: "Ginger juice", VolumeMl: 500, Additions: ginger("juice", 15, "ml"), PrimingSugarG: ptr(4.0), HeadspaceCm: ptr(2.0)},
		{Label: "B", FlavorName: "Ginger puree", VolumeMl: 500, Additions: ginger("puree", 30, "g"), PrimingSugarG: ptr(4.0), HeadspaceCm: ptr(2.0)},
		{Label: "C", FlavorName: "Ginger chunks", VolumeMl: 500, Additions: ginger("chunks", 15, "g"), PrimingSugarG: ptr(4.0), HeadspaceCm: ptr(2.0)},
		{Label: "D", FlavorName: "Raspberry lime", VolumeMl: 500, Additions: []domain.Addition{
			{Name: "Raspberry", Form: "whole", Amount: ptr(40.0), Unit: ptr("g")}, {Name: "Lime", Form: "juice", Amount: ptr(10.0), Unit: ptr("ml")},
		}, PrimingSugarG: ptr(2.0), HeadspaceCm: ptr(2.0)},
	}
	kb = decode[app.Batch](t)(svc.CreateBottles(ctx, alice, kb.ID, "bottle-kb-0001", app.CreateBottlesInput{Bottles: bottles}))
	if kb.Stage != "f2" || len(kb.Bottles) != 4 {
		t.Fatalf("after bottling: stage %s, %d bottles", kb.Stage, len(kb.Bottles))
	}
	byLabel := map[string]app.Bottle{}
	for _, b := range kb.Bottles {
		byLabel[b.Label] = b
	}

	// Each bottle has its own observations and readings.
	clk.add(24 * time.Hour)
	all := []string{byLabel["A"].ID, byLabel["B"].ID, byLabel["C"].ID, byLabel["D"].ID}
	burps := decode[map[string][]app.LogEntry](t)(svc.CreateLogs(ctx, alice, kb.ID, "burp-all-0001", app.CreateLogInput{Kind: "burp", BottleIDs: all}))
	if len(burps["data"]) != 4 {
		t.Fatalf("burp all creates one entry per bottle, got %d", len(burps["data"]))
	}
	for label, ph := range map[string]float64{"A": 3.3, "B": 3.2, "C": 3.4, "D": 3.0} {
		_, err := svc.CreateLogs(ctx, alice, kb.ID, "ph-"+label+"-0001", app.CreateLogInput{Kind: "measure", BottleID: ptr(byLabel[label].ID), Measurements: []domain.Measurement{
			{DefinitionKey: "ph", Value: domain.NumValue(ph), Unit: "pH", Method: ptr("meter")},
		}})
		if err != nil {
			t.Fatal(err)
		}
	}

	// 8-9. Finish and rate each bottle; A is the best.
	clk.add(2 * 24 * time.Hour)
	scores := map[string]int{"A": 5, "B": 4, "C": 3, "D": 4}
	for label, score := range scores {
		bo := decode[app.Bottle](t)(svc.FinishBottle(ctx, alice, byLabel[label].ID, "", app.FinishInput{Rating: rating(score, "bottle "+label)}))
		if bo.Status != "finished" || bo.Rating.Overall != score || len(bo.LastMeasurements) != 1 {
			t.Fatalf("finished bottle %s = %+v", label, bo)
		}
	}
	_, err = svc.FinishBottle(ctx, alice, byLabel["A"].ID, "", app.FinishInput{Rating: rating(1, "changed my mind")})
	wantCode(t, err, "invalid_state_transition") // ratings are history
	best := decode[app.Bottle](t)(svc.UpdateBottle(ctx, alice, byLabel["A"].ID, app.UpdateBottleInput{IsReference: ptr(true)}))
	if !best.IsReference {
		t.Fatal("bottle A is marked as the best")
	}
	_, err = svc.UpdateBottle(ctx, alice, byLabel["A"].ID, app.UpdateBottleInput{Additions: &[]domain.Addition{}})
	wantCode(t, err, "invalid_state_transition") // a finished experiment is frozen
	kb = decode[app.Batch](t)(svc.FinishBatch(ctx, alice, kb.ID, "", app.FinishInput{Rating: rating(4, "Good batch")}))
	if kb.Status != "finished" || kb.Stage != "done" {
		t.Fatalf("batch finished = %+v", kb)
	}

	// 10. Save bottle A as an F2 recipe.
	recipe := decode[app.Recipe](t)(svc.SaveBottleAsRecipe(ctx, alice, byLabel["A"].ID, "save-a-0001", app.SaveAsRecipeInput{Name: ptr("Ginger juice bottle")}))
	if recipe.Scope != "bottle" || recipe.LatestVersion.VersionNo != 1 || recipe.LatestVersion.Bottle == nil ||
		recipe.LatestVersion.Bottle.Additions[0].Form != "juice" || *recipe.LatestVersion.SourceBottleID != byLabel["A"].ID {
		t.Fatalf("bottle recipe = %+v", recipe)
	}
	v1 := recipe.LatestVersion
	_, err = svc.SaveBottleAsRecipe(ctx, alice, byLabel["A"].ID, "save-a-0002", app.SaveAsRecipeInput{RecipeID: &recipe.ID})
	wantCode(t, err, "no_changes")

	// 11-13. Brew again (base recipe follows the batch) and bottle a new experiment
	// from the F2 recipe with a different form and amount; the diff shows exactly that.
	clk.add(24 * time.Hour)
	draft := decode[app.BrewAgainDraft](t)(func() (app.Result, error) {
		d, err := svc.BrewAgainDraft(ctx, alice, kb.ID)
		b, _ := json.Marshal(d)
		return app.Result{Body: b}, err
	}())
	draft.Draft.Hypothesis = ptr("Same base; try puree in the bottle")
	kb2 := decode[app.Batch](t)(svc.CreateBatch(ctx, alice, "create-kb-0002", draft.Draft))
	if kb2.Code != "KB-002" || kb2.ParentBatchID == nil || *kb2.ParentBatchID != kb.ID || len(kb2.ChangedFields) != 0 {
		t.Fatalf("brew again = %+v", kb2)
	}
	clk.add(9 * 24 * time.Hour)
	newBottle := app.BottleInput{
		Label: "A", FlavorName: "Ginger puree", VolumeMl: 500, Additions: ginger("puree", 30, "g"), PrimingSugarG: ptr(4.0), HeadspaceCm: ptr(2.0),
		Source: &app.SourceRef{Type: "recipe_version", ID: v1.ID}, Hypothesis: ptr("Puree gives more fizz?"),
	}
	preview, err := svc.PreviewBottle(ctx, alice, app.BottlePreviewInput{Base: app.SourceRef{Type: "recipe_version", ID: v1.ID}, Draft: newBottle})
	if err != nil {
		t.Fatal(err)
	}
	form, amount, sugar := findRow(preview.Rows, "additions.ginger.form"), findRow(preview.Rows, "additions.ginger.amount"), findRow(preview.Rows, "additions.primingSugarG")
	if form == nil || !form.Differs || form.Values[0] != "juice" || form.Values[1] != "puree" ||
		amount == nil || !amount.Differs || amount.Values[0] != "15 ml" || amount.Values[1] != "30 g" ||
		sugar == nil || sugar.Differs || !*preview.WillCreateVersion || *preview.NextVersionNo != 2 {
		t.Fatalf("preview = %+v", preview)
	}
	kb2 = decode[app.Batch](t)(svc.CreateBottles(ctx, alice, kb2.ID, "bottle-kb-0002", app.CreateBottlesInput{Bottles: []app.BottleInput{newBottle}}))
	exp := kb2.Bottles[0]
	if exp.RecipeVersion == nil || exp.RecipeVersion.VersionNo != 2 || exp.Hypothesis == nil || len(exp.ChangedFields) == 0 {
		t.Fatalf("new experiment = %+v", exp)
	}

	// 14. Finish the new experiment.
	clk.add(3 * 24 * time.Hour)
	decode[app.Bottle](t)(svc.FinishBottle(ctx, alice, exp.ID, "", app.FinishInput{Rating: rating(4, "More body, less fizz")}))

	// 15. Compare both experiments across batches.
	cmp, err := svc.Compare(ctx, alice, app.CompareInput{Items: []app.ItemRef{{Type: "bottle", ID: byLabel["A"].ID}, {Type: "bottle", ID: exp.ID}}, OnlyDifferences: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"additions.ginger.form", "additions.ginger.amount", "result.overall", "recipe.version"} {
		if r := findRow(cmp.Rows, key); r == nil || !r.Differs {
			t.Errorf("compare should show %s: %+v", key, r)
		}
	}
	if r := findRow(cmp.Rows, "additions.primingSugarG"); r != nil {
		t.Errorf("unchanged priming sugar must not show with onlyDifferences: %+v", r)
	}
	if cmp.Items[0].Title != "KB-001 A" || !cmp.Items[0].IsReference {
		t.Errorf("compare items = %+v", cmp.Items)
	}

	// 16. History is unchanged: v1 still says juice, bottle A still has its data and score.
	v1Again, err := svc.GetRecipeVersion(ctx, alice, v1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v1Again.Bottle.Additions[0].Form != "juice" || *v1Again.Bottle.Additions[0].Amount != 15 {
		t.Errorf("recipe v1 changed: %+v", v1Again.Bottle)
	}
	old, err := svc.GetBatch(ctx, alice, kb.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range old.Bottles {
		if b.Label == "A" && (b.Additions[0].Form != "juice" || b.Rating.Overall != 5 || !b.IsReference || b.RecipeVersion == nil || b.RecipeVersion.VersionNo != 1) {
			t.Errorf("bottle A changed: %+v", b)
		}
	}
	versions, err := svc.ListRecipeVersions(ctx, alice, recipe.ID, "", 20)
	if err != nil || len(versions.Data) != 2 || versions.Data[0].VersionNo != 2 || *versions.Data[0].ParentVersionID != v1.ID {
		t.Fatalf("versions = %+v, %v", versions, err)
	}
}

// The version rule: a recipe-level change creates n+1 pinned to the new batch;
// conditions and scaling don't.
func TestBrewAgainVersionRule(t *testing.T) {
	svc, clk, ctx, _ := setup(t)
	kb := decode[app.Batch](t)(svc.CreateBatch(ctx, alice, "", kombuchaDraft()))
	clk.add(10 * 24 * time.Hour)
	decode[app.Batch](t)(svc.FinishBatch(ctx, alice, kb.ID, "", app.FinishInput{Rating: rating(4, "")}))
	recipe := decode[app.Recipe](t)(svc.SaveBatchAsRecipe(ctx, alice, kb.ID, "", app.SaveAsRecipeInput{Name: ptr("Black tea base")}))
	if recipe.Scope != "batch" || recipe.LatestVersion.VersionNo != 1 || len(recipe.LatestVersion.Ingredients) != 4 {
		t.Fatalf("recipe = %+v", recipe)
	}
	kb, _ = svc.GetBatch(ctx, alice, kb.ID)
	if kb.RecipeVersion == nil || kb.RecipeVersion.VersionNo != 1 {
		t.Fatalf("a batch brewed without a recipe is linked to v1: %+v", kb.RecipeVersion)
	}

	// Warmer room and a doubled batch: same recipe, no new version.
	same := kombuchaDraft()
	same.VolumeMl = 8400
	for i := range same.Ingredients {
		same.Ingredients[i].Amount *= 2
	}
	same.Params.RoomTempC = ptr(26.0)
	same.Source = &app.SourceRef{Type: "batch", ID: kb.ID}
	clk.add(24 * time.Hour)
	b2 := decode[app.Batch](t)(svc.CreateBatch(ctx, alice, "", same))
	if b2.RecipeVersion.VersionNo != 1 {
		t.Fatalf("conditions and scale don't version: got v%d", b2.RecipeVersion.VersionNo)
	}

	// More sugar: v2, parent v1, change note = hypothesis.
	more := kombuchaDraft()
	more.Ingredients[2].Amount = 260
	more.Hypothesis = ptr("More sugar for more fizz")
	more.Source = &app.SourceRef{Type: "batch", ID: kb.ID}
	preview, err := svc.PreviewBatch(ctx, alice, app.BatchPreviewInput{Base: app.SourceRef{Type: "batch", ID: kb.ID}, Draft: more})
	if err != nil || !*preview.WillCreateVersion || *preview.NextVersionNo != 2 {
		t.Fatalf("preview = %+v %v", preview, err)
	}
	if r := findRow(preview.Rows, "ingredients.sugar.cane_sugar"); r == nil || r.Values[0] != 228.0 || r.Values[1] != 260.0 || *r.Direction[1] != "up" {
		t.Fatalf("sugar row = %+v", r)
	}
	b3 := decode[app.Batch](t)(svc.CreateBatch(ctx, alice, "", more))
	if b3.RecipeVersion.VersionNo != 2 {
		t.Fatalf("recipe change creates v2, got v%d", b3.RecipeVersion.VersionNo)
	}
	v2, err := svc.GetRecipeVersionByNo(ctx, alice, recipe.ID, 2)
	if err != nil || *v2.ChangeNote != "More sugar for more fizz" || *v2.SourceBatchID != b3.ID || v2.ChangedFields[0] != "ingredients.sugar.cane_sugar" {
		t.Fatalf("v2 = %+v %v", v2, err)
	}
	snap, err := svc.Snapshot(ctx, alice, b3.ID)
	if err != nil || snap.RecipeVersion.VersionNo != 2 {
		t.Fatalf("snapshot = %+v %v", snap, err)
	}
	diff, err := svc.DiffRecipeVersions(ctx, alice, recipe.ID, 1, 2)
	if err != nil || diff.ChangeCount != 1 {
		t.Fatalf("v1→v2 diff = %+v %v", diff, err)
	}
}

func TestConcurrentSavesGetDistinctVersionNumbers(t *testing.T) {
	svc, clk, ctx, _ := setup(t)
	kb := decode[app.Batch](t)(svc.CreateBatch(ctx, alice, "", kombuchaDraft()))
	recipe := decode[app.Recipe](t)(svc.SaveBatchAsRecipe(ctx, alice, kb.ID, "", app.SaveAsRecipeInput{Name: ptr("Base")}))
	ids := []string{}
	for i := 0; i < 6; i++ {
		d := kombuchaDraft()
		d.Ingredients[2].Amount = float64(240 + i*10)
		clk.add(time.Hour)
		b := decode[app.Batch](t)(svc.CreateBatch(ctx, alice, "", d))
		ids = append(ids, b.ID)
	}
	errs := make(chan error, len(ids))
	for _, id := range ids {
		go func(id string) {
			_, err := svc.SaveBatchAsRecipe(ctx, alice, id, "", app.SaveAsRecipeInput{RecipeID: &recipe.ID})
			errs <- err
		}(id)
	}
	for range ids {
		if err := <-errs; err != nil {
			t.Errorf("concurrent save: %v", err)
		}
	}
	page, err := svc.ListRecipeVersions(ctx, alice, recipe.ID, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for _, v := range page.Data {
		if seen[v.VersionNo] {
			t.Fatalf("duplicate version number %d", v.VersionNo)
		}
		seen[v.VersionNo] = true
	}
	if len(seen) != 7 || !seen[7] {
		t.Fatalf("want versions 1..7 without gaps, got %v", seen)
	}
}

func TestIdempotentLogReplay(t *testing.T) {
	svc, _, ctx, _ := setup(t)
	kb := decode[app.Batch](t)(svc.CreateBatch(ctx, alice, "", kombuchaDraft()))
	in := app.CreateLogInput{Kind: "note", Note: ptr("Smells bready")}
	first, err := svc.CreateLogs(ctx, alice, kb.ID, "same-key-0001", in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := svc.CreateLogs(ctx, alice, kb.ID, "same-key-0001", in)
	if err != nil || !again.Replayed || string(again.Body) != string(first.Body) {
		t.Fatalf("replay: %v replayed=%v", err, again.Replayed)
	}
	_, err = svc.CreateLogs(ctx, alice, kb.ID, "same-key-0001", app.CreateLogInput{Kind: "note", Note: ptr("different")})
	wantCode(t, err, "idempotency_key_reused")
	page, _ := svc.ListLogs(ctx, alice, kb.ID, nil, "", 20)
	if len(page.Data) != 1 {
		t.Fatalf("the replay must not create a second entry, got %d", len(page.Data))
	}
	_, err = svc.CreateLogs(ctx, alice, kb.ID, "", in)
	wantCode(t, err, "validation_failed")
}

func TestMeasurementKindsAndUnits(t *testing.T) {
	svc, _, ctx, _ := setup(t)
	kb := decode[app.Batch](t)(svc.CreateBatch(ctx, alice, "", kombuchaDraft()))
	for _, d := range []app.CreateDefinitionInput{
		{Key: "pellicle", Label: "New pellicle", Kind: ptr("boolean")},
		{Key: "aroma", Label: "Aroma", Kind: ptr("text")},
		{Key: "fizz_check", Label: "Fizz check", Kind: ptr("scale"), Min: ptr(0.0), Max: ptr(3.0)},
	} {
		if _, err := svc.CreateMeasurementDefinition(ctx, alice, d); err != nil {
			t.Fatal(err)
		}
	}
	_, err := svc.CreateMeasurementDefinition(ctx, alice, app.CreateDefinitionInput{Key: "fourth", Label: "Fourth"})
	wantCode(t, err, "entitlement_required") // free plan: 3 custom definitions

	yes := true
	text := "vinegar"
	_, err = svc.CreateLogs(ctx, alice, kb.ID, "kinds-0001", app.CreateLogInput{Kind: "measure", Measurements: []domain.Measurement{
		{DefinitionKey: "pellicle", Value: domain.Value{Bool: &yes}},
		{DefinitionKey: "aroma", Value: domain.Value{Text: &text}},
		{DefinitionKey: "fizz_check", Value: domain.NumValue(2)},
		{DefinitionKey: "temp", Value: domain.NumValue(295), Unit: "K"},
	}})
	wantCode(t, err, "validation_failed") // kelvin is not a supported temperature unit

	_, err = svc.CreateLogs(ctx, alice, kb.ID, "kinds-0002", app.CreateLogInput{Kind: "measure", Measurements: []domain.Measurement{
		{DefinitionKey: "pellicle", Value: domain.Value{Bool: &yes}},
		{DefinitionKey: "aroma", Value: domain.Value{Text: &text}},
		{DefinitionKey: "fizz_check", Value: domain.NumValue(2)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.CreateLogs(ctx, alice, kb.ID, "kinds-0003", app.CreateLogInput{Kind: "measure", Measurements: []domain.Measurement{
		{DefinitionKey: "pellicle", Value: domain.NumValue(1)},
	}})
	wantCode(t, err, "validation_failed") // a yes/no reading needs true or false

	// Bob's definitions are his: Alice can't record a reading with Bob's key.
	if _, err := svc.CreateMeasurementDefinition(ctx, bob, app.CreateDefinitionInput{Key: "bobs_only", Label: "Bob"}); err != nil {
		t.Fatal(err)
	}
	_, err = svc.CreateLogs(ctx, alice, kb.ID, "kinds-0004", app.CreateLogInput{Kind: "measure", Measurements: []domain.Measurement{
		{DefinitionKey: "bobs_only", Value: domain.NumValue(1)},
	}})
	wantCode(t, err, "validation_failed")
}

func TestUserIsolation(t *testing.T) {
	svc, clk, ctx, _ := setup(t)
	kb := decode[app.Batch](t)(svc.CreateBatch(ctx, alice, "", kombuchaDraft()))
	kb = decode[app.Batch](t)(svc.CreateBottles(ctx, alice, kb.ID, "", app.CreateBottlesInput{Bottles: []app.BottleInput{
		{Label: "A", FlavorName: "Ginger", VolumeMl: 500, Additions: ginger("juice", 15, "ml")},
	}}))
	clk.add(48 * time.Hour)
	decode[app.Bottle](t)(svc.FinishBottle(ctx, alice, kb.Bottles[0].ID, "", app.FinishInput{Rating: rating(5, "")}))
	recipe := decode[app.Recipe](t)(svc.SaveBottleAsRecipe(ctx, alice, kb.Bottles[0].ID, "", app.SaveAsRecipeInput{Name: ptr("Alice's ginger")}))
	bobBatch := decode[app.Batch](t)(svc.CreateBatch(ctx, bob, "", kombuchaDraft()))

	notFound := func(name string, err error) {
		t.Helper()
		if e, ok := apperr.As(err); !ok || e.Status != 404 {
			t.Errorf("%s: want 404, got %v", name, err)
		}
	}
	// Bob can't read Alice's records…
	_, err := svc.GetBatch(ctx, bob, kb.ID)
	notFound("read batch", err)
	_, err = svc.GetRecipe(ctx, bob, recipe.ID)
	notFound("read recipe", err)
	_, err = svc.GetRecipeVersion(ctx, bob, recipe.LatestVersion.ID)
	notFound("read version", err)
	_, err = svc.ListLogs(ctx, bob, kb.ID, nil, "", 20)
	notFound("read logs", err)
	_, err = svc.Compare(ctx, bob, app.CompareInput{Items: []app.ItemRef{{Type: "batch", ID: bobBatch.ID}, {Type: "batch", ID: kb.ID}}})
	notFound("compare with another user's batch", err)
	list, _ := svc.ListBatches(ctx, bob, app.BatchQuery{})
	if len(list.Data) != 1 || list.Data[0].ID != bobBatch.ID {
		t.Errorf("Bob's list shows only his batch: %+v", list.Data)
	}
	// …or modify them…
	_, err = svc.UpdateBatch(ctx, bob, kb.ID, app.UpdateBatchInput{Name: ptr("mine now")})
	notFound("rename batch", err)
	_, err = svc.FinishBatch(ctx, bob, kb.ID, "", app.FinishInput{Rating: rating(1, "")})
	notFound("finish batch", err)
	_, err = svc.UpdateRecipe(ctx, bob, recipe.ID, app.UpdateRecipeInput{Archived: ptr(true)})
	notFound("archive recipe", err)
	_, err = svc.CreateLogs(ctx, bob, kb.ID, "bob-log-0001", app.CreateLogInput{Kind: "note", Note: ptr("hi")})
	notFound("log on batch", err)
	// …or attach their own data to Alice's, or Alice's to theirs.
	_, err = svc.CreateLogs(ctx, bob, bobBatch.ID, "bob-log-0002", app.CreateLogInput{Kind: "note", BottleID: ptr(kb.Bottles[0].ID)})
	notFound("log on another user's bottle", err)
	_, err = svc.CreateBottles(ctx, bob, bobBatch.ID, "", app.CreateBottlesInput{Bottles: []app.BottleInput{
		{Label: "A", FlavorName: "Stolen", VolumeMl: 500, Source: &app.SourceRef{Type: "recipe_version", ID: recipe.LatestVersion.ID}},
	}})
	notFound("bottle from another user's recipe version", err)
	_, err = svc.CreateBatch(ctx, bob, "", func() app.CreateBatchInput {
		d := kombuchaDraft()
		d.Source = &app.SourceRef{Type: "batch", ID: kb.ID}
		return d
	}())
	notFound("brew again from another user's batch", err)
	_, err = svc.SaveBottleAsRecipe(ctx, bob, kb.Bottles[0].ID, "", app.SaveAsRecipeInput{Name: ptr("x")})
	notFound("save another user's bottle", err)
	_, err = svc.SaveBatchAsRecipe(ctx, bob, bobBatch.ID, "", app.SaveAsRecipeInput{RecipeID: &recipe.ID})
	notFound("save into another user's recipe", err)

	// Idempotency keys are per user: Bob reusing Alice's key gets his own result.
	_, err = svc.CreateLogs(ctx, alice, kb.ID, "shared-key-0001", app.CreateLogInput{Kind: "note", Note: ptr("a")})
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.CreateLogs(ctx, bob, bobBatch.ID, "shared-key-0001", app.CreateLogInput{Kind: "note", Note: ptr("a")})
	if err != nil || res.Replayed {
		t.Fatalf("Bob's request with the same key must run for Bob: %v replayed=%v", err, res.Replayed)
	}

	// Plan: Bob can't grant himself Pro through the API (there is no path) — the
	// database test proves the runtime role can't write subscriptions either.
	e, _ := svc.Entitlements(ctx, bob)
	if e.Plan != "free" || e.Limits.CompareItems != 2 {
		t.Errorf("entitlements = %+v", e)
	}
	kb2 := decode[app.Batch](t)(svc.CreateBatch(ctx, alice, "", kombuchaDraft()))
	_, err = svc.Compare(ctx, alice, app.CompareInput{Items: []app.ItemRef{{Type: "batch", ID: kb.ID}, {Type: "bottle", ID: kb.Bottles[0].ID}, {Type: "batch", ID: kb2.ID}}})
	wantCode(t, err, "entitlement_required") // three items on the free plan
}

func TestCursorPagination(t *testing.T) {
	svc, clk, ctx, _ := setup(t)
	for i := 0; i < 5; i++ {
		clk.add(time.Hour)
		decode[app.Batch](t)(svc.CreateBatch(ctx, alice, "", kombuchaDraft()))
	}
	seen := map[string]bool{}
	cur := ""
	for page := 0; page < 5; page++ {
		p, err := svc.ListBatches(ctx, alice, app.BatchQuery{Limit: 2, Cursor: cur})
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range p.Data {
			if seen[b.ID] {
				t.Fatalf("batch %s on two pages", b.Code)
			}
			seen[b.ID] = true
		}
		if p.NextCursor == nil {
			break
		}
		cur = *p.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("paged through %d of 5 batches", len(seen))
	}
	// A cursor is bound to its query.
	p, _ := svc.ListBatches(ctx, alice, app.BatchQuery{Limit: 2})
	_, err := svc.ListBatches(ctx, alice, app.BatchQuery{Limit: 2, Cursor: *p.NextCursor, Sort: "started_asc"})
	wantCode(t, err, "invalid_cursor")
	_, err = svc.ListBatches(ctx, alice, app.BatchQuery{Cursor: "tampered." + "x"})
	wantCode(t, err, "invalid_cursor")
}
