package domain

import (
	"reflect"
	"testing"
)

func f(v float64) *float64 { return &v }
func s(v string) *string   { return &v }
func i(v int) *int         { return &v }
func b(v bool) *bool       { return &v }

func rowByKey(t *testing.T, rows []Row, key string) Row {
	t.Helper()
	for _, r := range rows {
		if r.Key == key {
			return r
		}
	}
	t.Fatalf("row %q not found in %v", key, keys(rows))
	return Row{}
}

func keys(rows []Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Key
	}
	return out
}

func gingerBottle(form string, amount float64, unit string) *BottleSpec {
	return &BottleSpec{
		FlavorName:    "Ginger",
		VolumeMl:      500,
		Additions:     []Addition{{Name: "Ginger", Form: form, Amount: f(amount), Unit: s(unit)}},
		PrimingSugarG: f(4),
		HeadspaceCm:   f(2),
	}
}

// The example from the brief: juice 15 ml -> puree 30 g.
func TestDiffBottleFormAndAmount(t *testing.T) {
	rows := Diff([]Subject{
		{Bottle: gingerBottle("juice", 15, "ml")},
		{Bottle: gingerBottle("puree", 30, "g")},
	}, Options{Sections: ConfigSections})

	form := rowByKey(t, rows, "additions.ginger.form")
	if !form.Differs || form.Values[0] != "juice" || form.Values[1] != "puree" {
		t.Errorf("form row = %+v", form)
	}
	amount := rowByKey(t, rows, "additions.ginger.amount")
	if !amount.Differs || amount.Values[0] != "15 ml" || amount.Values[1] != "30 g" || amount.Unit != nil {
		t.Errorf("amount row = %+v", amount)
	}
	if amount.Direction[1] != nil {
		t.Errorf("text values must have no direction, got %v", *amount.Direction[1])
	}
	sugar := rowByKey(t, rows, "additions.primingSugarG")
	if sugar.Differs {
		t.Errorf("priming sugar is unchanged: %+v", sugar)
	}
	if got := RecipeChanges(Subject{Bottle: gingerBottle("juice", 15, "ml")}, Subject{Bottle: gingerBottle("puree", 30, "g")}); !reflect.DeepEqual(got, []string{"additions.ginger.form", "additions.ginger.amount"}) {
		t.Errorf("RecipeChanges = %v", got)
	}
}

func TestDiffEquivalentUnitsAreNotDifferences(t *testing.T) {
	rows := Diff([]Subject{
		{Bottle: gingerBottle("juice", 15, "ml")},
		{Bottle: gingerBottle("juice", 0.015, "L")},
	}, Options{Sections: ConfigSections})
	amount := rowByKey(t, rows, "additions.ginger.amount")
	if amount.Differs || amount.Values[0] != 15.0 || amount.Values[1] != 15.0 || *amount.Unit != "ml" {
		t.Errorf("15 ml and 0.015 L must be equal: %+v", amount)
	}

	batch := func(water float64, unit string) Subject {
		return Subject{VolumeMl: f(4200), Ingredients: []IngredientInput{{Name: "Filtered water", Role: "water", Amount: water, Unit: unit}}}
	}
	rows = Diff([]Subject{batch(1, "L"), batch(1000, "ml")}, Options{Sections: ConfigSections})
	if r := rowByKey(t, rows, "ingredients.water.filtered_water"); r.Differs {
		t.Errorf("1 L and 1000 ml must be equal: %+v", r)
	}
}

func TestDiffTemperatureInUserUnit(t *testing.T) {
	defs := []MeasurementDef{{Key: "temp", Label: "Temp", Kind: "numeric", Quantity: "temperature", Unit: "°C", Decimals: 1}}
	reading := func(c float64) map[string]Series {
		return map[string]Series{"temp": {First: &Reading{Num: f(c)}, Last: &Reading{Num: f(c)}}}
	}
	// 71.6 °F entered by one user is stored as 22 °C: identical readings.
	c, _, _ := ToCanonical(71.6, "°F")
	rows := Diff([]Subject{
		{Params: BatchParams{RoomTempC: f(22)}, Measurements: reading(22)},
		{Params: BatchParams{RoomTempC: f(c)}, Measurements: reading(c)},
	}, Options{Sections: AllSections, TempUnit: "F", Defs: defs})

	room := rowByKey(t, rows, "params.roomTempC")
	if room.Differs || room.Values[0] != 71.6 || *room.Unit != "°F" || room.Category != CatEnvironment {
		t.Errorf("room temperature row = %+v", room)
	}
	temp := rowByKey(t, rows, "measurements.temp.last")
	if temp.Differs || temp.Category != CatEnvironment {
		t.Errorf("temperature reading row = %+v", temp)
	}
}

func TestDiffScaledBatchIsNotARecipeChange(t *testing.T) {
	base := Subject{VolumeMl: f(4000), Ingredients: []IngredientInput{
		{Name: "Cane sugar", Role: "sugar", Amount: 228, Unit: "g"},
		{Name: "Black tea", Role: "tea", Amount: 16, Unit: "g"},
	}, Params: BatchParams{TargetF1Days: f(9)}}
	doubled := Subject{VolumeMl: f(8000), Ingredients: []IngredientInput{
		{Name: "cane  SUGAR", Role: "sugar", Amount: 456, Unit: "g"},
		{Name: "Black tea", Role: "tea", Amount: 0.032, Unit: "kg"},
	}, Params: BatchParams{TargetF1Days: f(9)}}

	if got := RecipeChanges(base, doubled); len(got) != 0 {
		t.Fatalf("doubling every amount is the same recipe, got changes %v", got)
	}
	rows := Diff([]Subject{base, doubled}, Options{Sections: ConfigSections})
	sugar := rowByKey(t, rows, "ingredients.sugar.cane_sugar")
	if sugar.Differs || *sugar.Unit != "g/L" || sugar.Values[0] != 57.0 {
		t.Errorf("different volumes are shown per litre: %+v", sugar)
	}
	size := rowByKey(t, rows, "process.volumeMl")
	if !size.Differs || size.Category != CatVariation || *size.Direction[1] != "up" {
		t.Errorf("batch size is a variation: %+v", size)
	}

	more := base
	more.Ingredients = []IngredientInput{
		{Name: "Cane sugar", Role: "sugar", Amount: 260, Unit: "g"},
		{Name: "Black tea", Role: "tea", Amount: 16, Unit: "g"},
	}
	if got := RecipeChanges(base, more); !reflect.DeepEqual(got, []string{"ingredients.sugar.cane_sugar"}) {
		t.Errorf("RecipeChanges = %v", got)
	}
	rows = Diff([]Subject{base, more}, Options{Sections: ConfigSections, OnlyDifferences: true})
	if len(rows) != 1 || rows[0].Values[0] != 228.0 || rows[0].Values[1] != 260.0 || *rows[0].Unit != "g" || *rows[0].Direction[1] != "up" {
		t.Errorf("same volume shows absolute amounts: %+v", rows)
	}
	if got := DescribeChange(rows[0]); got != "Cane sugar: 228 g → 260 g" {
		t.Errorf("DescribeChange = %q", got)
	}
}

func TestDiffConditionsAreNotRecipeChanges(t *testing.T) {
	base := Subject{VolumeMl: f(4000), Params: BatchParams{TargetF1Days: f(9), RoomTempC: f(22)}}
	warmer := Subject{VolumeMl: f(4000), Params: BatchParams{TargetF1Days: f(9), RoomTempC: f(25), StarterSource: s("KB-012")}}
	if got := RecipeChanges(base, warmer); len(got) != 0 {
		t.Errorf("room temperature and starter source are batch conditions, got %v", got)
	}
}

func TestDiffMissingValuesAndAddedRemovedIngredients(t *testing.T) {
	rows := Diff([]Subject{
		{VolumeMl: f(1000), Ingredients: []IngredientInput{{Name: "Ginger", Role: "flavor", Amount: 20, Unit: "g"}}},
		{VolumeMl: f(1000), Ingredients: []IngredientInput{{Name: "Hibiscus", Role: "flavor", Amount: 2, Unit: "bags"}}},
	}, Options{Sections: ConfigSections})
	removed := rowByKey(t, rows, "ingredients.flavor.ginger")
	if !removed.Differs || removed.Values[1] != nil || removed.Direction[1] != nil {
		t.Errorf("removed ingredient: %+v", removed)
	}
	added := rowByKey(t, rows, "ingredients.flavor.hibiscus")
	if !added.Differs || added.Values[0] != nil || added.Values[1] != "2 bags" {
		t.Errorf("added non-convertible ingredient: %+v", added)
	}
}

func TestDiffResultsAndOnlyDifferences(t *testing.T) {
	r1 := &Rating{Overall: 4, Fizz: i(3), OffNotes: []string{"Vinegary"}, WouldRepeat: b(true)}
	r2 := &Rating{Overall: 4, Fizz: i(5), OffNotes: []string{"vinegary "}, WouldRepeat: b(true)}
	rows := Diff([]Subject{{Rating: r1}, {Rating: r2}, {Rating: r1}}, Options{Sections: AllSections, OnlyDifferences: true})
	if !reflect.DeepEqual(keys(rows), []string{"result.fizz"}) {
		t.Fatalf("only fizz differs, got %v", keys(rows))
	}
	if *rows[0].Direction[1] != "up" || *rows[0].Direction[2] != "same" || rows[0].Direction[0] != nil {
		t.Errorf("directions = %v", rows[0].Direction)
	}
}

func TestDiffDifferenceBetweenNonBaseItems(t *testing.T) {
	rows := Diff([]Subject{
		{Rating: &Rating{Overall: 3}},
		{Rating: &Rating{Overall: 3, Fizz: i(2)}},
		{Rating: &Rating{Overall: 3, Fizz: i(4)}},
	}, Options{Sections: AllSections})
	if r := rowByKey(t, rows, "result.fizz"); !r.Differs {
		t.Errorf("fizz differs between items 2 and 3: %+v", r)
	}
}

func TestDiffMeasurementKinds(t *testing.T) {
	defs := []MeasurementDef{
		{Key: "ph", Label: "pH", Kind: "numeric", Unit: "pH", Decimals: 1},
		{Key: "pellicle", Label: "New pellicle", Kind: "boolean"},
		{Key: "aroma", Label: "Aroma", Kind: "text"},
	}
	yes, no := true, false
	rows := Diff([]Subject{
		{Measurements: map[string]Series{
			"ph":       {First: &Reading{Num: f(4.5)}, Last: &Reading{Num: f(3.14)}},
			"pellicle": {Last: &Reading{Bool: &yes}},
			"aroma":    {Last: &Reading{Text: s("vinegar")}},
		}},
		{Measurements: map[string]Series{
			"ph":       {First: &Reading{Num: f(4.5)}, Last: &Reading{Num: f(3.11)}},
			"pellicle": {Last: &Reading{Bool: &no}},
		}},
	}, Options{Sections: AllSections, Defs: defs})
	ph := rowByKey(t, rows, "measurements.ph.last")
	if ph.Differs || ph.Values[0] != 3.1 || ph.Category != CatMeasurement {
		t.Errorf("pH compares at the definition's precision: %+v", ph)
	}
	if r := rowByKey(t, rows, "measurements.pellicle.last"); !r.Differs || r.Values[0] != "Yes" || r.Values[1] != "No" || r.Unit != nil {
		t.Errorf("boolean row = %+v", r)
	}
	if r := rowByKey(t, rows, "measurements.aroma.last"); !r.Differs || r.Values[1] != nil {
		t.Errorf("text row = %+v", r)
	}
}

func TestDiffRowOrderIsDeterministic(t *testing.T) {
	sub := Subject{
		VolumeMl:    f(4000),
		Ingredients: []IngredientInput{{Name: "Water", Role: "water", Amount: 3.8, Unit: "L"}, {Name: "Tea", Role: "tea", Amount: 12, Unit: "g"}},
		Params:      BatchParams{TargetF1Days: f(9)},
		Rating:      &Rating{Overall: 4},
		F1Days:      f(9.2),
	}
	want := []string{"ingredients.water.water", "ingredients.tea.tea", "params.targetF1Days", "process.volumeMl", "process.f1Days", "result.overall"}
	for n := 0; n < 20; n++ {
		if got := keys(Diff([]Subject{sub, sub}, Options{Sections: AllSections})); !reflect.DeepEqual(got, want) {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	water := rowByKey(t, Diff([]Subject{sub, sub}, Options{Sections: AllSections}), "ingredients.water.water")
	if water.Values[0] != 3.8 || *water.Unit != "L" {
		t.Errorf("litres for large volumes: %+v", water)
	}
	if d := rowByKey(t, Diff([]Subject{sub, sub}, Options{Sections: AllSections}), "process.f1Days"); d.Values[0] != 9.0 {
		t.Errorf("long ferments in whole days: %+v", d)
	}
}

func TestUnits(t *testing.T) {
	cases := []struct {
		amount float64
		unit   string
		want   float64
		dim    Dimension
	}{
		{1, "L", 1000, DimVolume}, {1, "litre", 1000, DimVolume}, {250, "ml", 250, DimVolume},
		{1, "kg", 1000, DimMass}, {22, "°C", 22, DimTemperature}, {71.6, "°F", 22, DimTemperature}, {32, "F", 0, DimTemperature},
	}
	for _, c := range cases {
		got, dim, ok := ToCanonical(c.amount, c.unit)
		if !ok || dim != c.dim || Round(got, 6) != c.want {
			t.Errorf("ToCanonical(%v %q) = %v %v %v, want %v %v", c.amount, c.unit, got, dim, ok, c.want, c.dim)
		}
	}
	if _, _, ok := ToCanonical(2, "bags"); ok {
		t.Error("bags are not convertible")
	}
	if Round(CelsiusTo(22, "F"), 1) != 71.6 {
		t.Error("22 °C is 71.6 °F")
	}
	if Slug("  Cane   Sugar ") != "cane_sugar" {
		t.Error("slug")
	}
}
