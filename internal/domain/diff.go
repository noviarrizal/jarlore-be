package domain

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Diff groups (contract enum) and categories. The group says where a row sits in
// the table; the category says what kind of difference it is:
//
//	recipe       a recipe-level choice (ingredients per litre, target F1 length, bottle additions)
//	variation    how this batch or bottle was actually run (size, vessel, durations, headspace)
//	environment  conditions observed around it (room temperature, starter source, temperature readings)
//	measurement  other readings (pH, Brix, gravity, custom)
//	outcome      the rating
const (
	GroupIngredients  = "ingredients"
	GroupParams       = "params"
	GroupAdditions    = "additions"
	GroupProcess      = "process"
	GroupMeasurements = "measurements"
	GroupResult       = "result"

	CatRecipe      = "recipe"
	CatVariation   = "variation"
	CatEnvironment = "environment"
	CatMeasurement = "measurement"
	CatOutcome     = "outcome"
)

var groupOrder = map[string]int{
	GroupIngredients: 0, GroupParams: 1, GroupAdditions: 2, GroupProcess: 3, GroupMeasurements: 4, GroupResult: 5,
}

// Row is one line of a diff: the same attribute across every compared item.
// Values hold float64, string or nil. Direction[0] is always nil.
type Row struct {
	Key       string    `json:"key"`
	Label     string    `json:"label"`
	Group     string    `json:"group"`
	Category  string    `json:"category"`
	Values    []any     `json:"values"`
	Unit      *string   `json:"unit"`
	Differs   bool      `json:"differs"`
	Direction []*string `json:"direction"`
}

// Reading is one measurement value, numbers already in the canonical unit.
type Reading struct {
	Num  *float64
	Bool *bool
	Text *string
}

// Series is the first and last reading of one measurement on one item.
type Series struct {
	First, Last *Reading
}

// Subject is anything that can be compared: a batch, a bottle, a recipe version
// or a draft. Unset fields are simply absent from the item.
type Subject struct {
	RecipeLabel  *string // "Ginger fizz v3"
	VolumeMl     *float64
	Ingredients  []IngredientInput
	Params       BatchParams
	Vessel       *string
	Bottle       *BottleSpec
	F1Days       *float64
	F2Days       *float64
	BottleCount  *int
	Measurements map[string]Series
	Rating       *Rating
}

// Sections selects which parts of the items are compared.
type Sections struct {
	RecipeLabel  bool
	Ingredients  bool
	Params       bool
	Additions    bool
	Process      bool // batch or bottle size, vessel, headspace
	Durations    bool // F1/F2 length, bottle count
	Measurements bool
	Result       bool
}

var (
	// AllSections is used by Compare.
	AllSections = Sections{true, true, true, true, true, true, true, true}
	// ConfigSections is used by Brew Again previews: what the user is about to brew.
	ConfigSections = Sections{Ingredients: true, Params: true, Additions: true, Process: true}
	// RecipeSections is used by recipe version diffs.
	RecipeSections = Sections{Ingredients: true, Params: true, Additions: true}
)

// Options configure a diff.
type Options struct {
	Sections        Sections
	TempUnit        string // "C" or "F": temperatures are shown in the user's unit
	Defs            []MeasurementDef
	OnlyDifferences bool
	// PerLitre forces ingredient amounts per litre even when volumes match.
	// The version rule uses it so a scaled batch doesn't count as a recipe change.
	PerLitre bool
}

// Diff compares the subjects. Row order is deterministic: by group, then by
// first appearance (subject order, then ingredient or addition position).
func Diff(subjects []Subject, o Options) []Row {
	var rows []Row
	s := o.Sections
	if s.RecipeLabel {
		rows = appendRow(rows, "recipe.version", "Recipe", GroupProcess, CatRecipe, nil,
			pick(subjects, func(x Subject) any { return strOrNil(x.RecipeLabel) }))
	}
	if s.Ingredients {
		rows = append(rows, ingredientRows(subjects, o.PerLitre)...)
	}
	if s.Params {
		days := "days"
		rows = appendRow(rows, "params.targetF1Days", "Target F1 length", GroupParams, CatRecipe, &days,
			pick(subjects, func(x Subject) any { return numOrNil(x.Params.TargetF1Days, 1) }))
		tu := TempLabel(o.TempUnit)
		rows = appendRow(rows, "params.roomTempC", "Room temperature", GroupParams, CatEnvironment, &tu,
			pick(subjects, func(x Subject) any {
				if x.Params.RoomTempC == nil {
					return nil
				}
				return Round(CelsiusTo(*x.Params.RoomTempC, o.TempUnit), 1)
			}))
		rows = appendRow(rows, "params.starterSource", "Starter source", GroupParams, CatEnvironment, nil,
			pick(subjects, func(x Subject) any { return trimmedOrNil(x.Params.StarterSource) }))
	}
	anyBottle := false
	for _, x := range subjects {
		if x.Bottle != nil {
			anyBottle = true
		}
	}
	if s.Additions && anyBottle {
		rows = append(rows, additionRows(subjects)...)
	}
	if s.Process {
		ml := "ml"
		if anyBottle {
			rows = appendRow(rows, "process.bottleVolumeMl", "Bottle size", GroupProcess, CatVariation, &ml,
				pick(subjects, func(x Subject) any {
					if x.Bottle == nil {
						return nil
					}
					return float64(x.Bottle.VolumeMl)
				}))
			cm := "cm"
			rows = appendRow(rows, "process.headspaceCm", "Headspace", GroupProcess, CatVariation, &cm,
				pick(subjects, func(x Subject) any {
					if x.Bottle == nil {
						return nil
					}
					return numOrNil(x.Bottle.HeadspaceCm, 1)
				}))
		} else {
			rows = appendRow(rows, "process.volumeMl", "Batch size", GroupProcess, CatVariation, &ml,
				pick(subjects, func(x Subject) any { return numOrNil(x.VolumeMl, 0) }))
			rows = appendRow(rows, "process.vessel", "Vessel", GroupProcess, CatVariation, nil,
				pick(subjects, func(x Subject) any { return trimmedOrNil(x.Vessel) }))
		}
	}
	if s.Durations {
		days := "days"
		rows = appendRow(rows, "process.f1Days", "F1 length", GroupProcess, CatVariation, &days,
			pick(subjects, func(x Subject) any { return daysValue(x.F1Days) }))
		rows = appendRow(rows, "process.f2Days", "F2 length", GroupProcess, CatVariation, &days,
			pick(subjects, func(x Subject) any { return daysValue(x.F2Days) }))
		if !anyBottle {
			rows = appendRow(rows, "process.bottleCount", "Bottles", GroupProcess, CatVariation, nil,
				pick(subjects, func(x Subject) any {
					if x.BottleCount == nil {
						return nil
					}
					return float64(*x.BottleCount)
				}))
		}
	}
	if s.Measurements {
		rows = append(rows, measurementRows(subjects, o)...)
	}
	if s.Result {
		rows = append(rows, resultRows(subjects)...)
	}

	sort.SliceStable(rows, func(i, j int) bool { return groupOrder[rows[i].Group] < groupOrder[rows[j].Group] })
	if o.OnlyDifferences {
		kept := rows[:0]
		for _, r := range rows {
			if r.Differs {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	if rows == nil {
		rows = []Row{}
	}
	return rows
}

// ChangeCount counts rows that differ.
func ChangeCount(rows []Row) int {
	n := 0
	for _, r := range rows {
		if r.Differs {
			n++
		}
	}
	return n
}

// RecipeChanges returns the keys of recipe-level differences between a recipe
// version (or its snapshot) and a draft. Empty means "same recipe version".
// Ingredients are compared per litre, so scaling a batch never creates a version;
// bottle additions are compared per bottle.
func RecipeChanges(base, draft Subject) []string {
	rows := Diff([]Subject{base, draft}, Options{
		Sections: Sections{Ingredients: true, Params: true, Additions: true},
		PerLitre: true,
	})
	keys := []string{}
	for _, r := range rows {
		if r.Differs && r.Category == CatRecipe {
			keys = append(keys, r.Key)
		}
	}
	return keys
}

// ---------------------------------------------------------------- row helpers

func pick(subjects []Subject, f func(Subject) any) []any {
	out := make([]any, len(subjects))
	for i, x := range subjects {
		out[i] = f(x)
	}
	return out
}

// appendRow builds a row and drops it when no item has a value.
func appendRow(rows []Row, key, label, group, cat string, unit *string, values []any) []Row {
	if allNil(values) {
		return rows
	}
	return append(rows, makeRow(key, label, group, cat, unit, values))
}

func makeRow(key, label, group, cat string, unit *string, values []any) Row {
	dir := make([]*string, len(values))
	differs := false
	for i := 1; i < len(values); i++ {
		if !sameValue(values[i], values[0]) {
			differs = true
		}
		a, okA := values[0].(float64)
		b, okB := values[i].(float64)
		if okA && okB {
			d := "same"
			if b > a {
				d = "up"
			} else if b < a {
				d = "down"
			}
			dir[i] = &d
		}
	}
	// Two items may both differ from each other without differing from the base.
	if !differs {
		for i := 1; i < len(values); i++ {
			for j := i + 1; j < len(values); j++ {
				if !sameValue(values[i], values[j]) {
					differs = true
				}
			}
		}
	}
	return Row{Key: key, Label: label, Group: group, Category: cat, Values: values, Unit: unit, Differs: differs, Direction: dir}
}

func sameValue(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a == b
}

func allNil(values []any) bool {
	for _, v := range values {
		if v != nil {
			return false
		}
	}
	return true
}

func strOrNil(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func trimmedOrNil(s *string) any {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	return strings.TrimSpace(*s)
}

func numOrNil(f *float64, decimals int) any {
	if f == nil {
		return nil
	}
	return Round(*f, decimals)
}

// daysValue shows short ferments with one decimal (water kefir: 1.5 days) and
// longer ones in whole days, so timestamps don't produce "16.1 days".
func daysValue(d *float64) any {
	if d == nil {
		return nil
	}
	if *d < 3 {
		return Round(*d, 1)
	}
	return Round(*d, 0)
}

// formatAmount renders a non-convertible amount as entered: "2 bags", "15 ml".
func formatAmount(amount float64, unit string) string {
	return strings.TrimSpace(strconv.FormatFloat(Round(amount, 2), 'f', -1, 64) + " " + strings.TrimSpace(unit))
}

// ---------------------------------------------------------------- ingredients

type found struct {
	present bool
	amount  float64
	unit    string
	canon   float64
	dim     Dimension
	ok      bool
}

func ingredientRows(subjects []Subject, perLitre bool) []Row {
	type key struct{ role, slug string }
	var order []key
	labels := map[key]string{}
	for _, x := range subjects {
		for _, in := range x.Ingredients {
			k := key{in.Role, Slug(in.Name)}
			if _, seen := labels[k]; !seen {
				order = append(order, k)
				labels[k] = strings.TrimSpace(in.Name)
			}
		}
	}

	sameVolume := true
	var vol *float64
	for _, x := range subjects {
		if x.VolumeMl == nil {
			continue
		}
		if vol != nil && math.Abs(*vol-*x.VolumeMl) > 0.5 {
			sameVolume = false
		}
		vol = x.VolumeMl
	}
	usePerLitre := perLitre || !sameVolume

	rows := make([]Row, 0, len(order))
	for _, k := range order {
		items := make([]found, len(subjects))
		for i, x := range subjects {
			for _, in := range x.Ingredients {
				if (key{in.Role, Slug(in.Name)}) != k {
					continue
				}
				c, dim, ok := ToCanonical(in.Amount, in.Unit)
				if items[i].present {
					// The same ingredient listed twice: add it up when the units allow it.
					if items[i].ok && ok && items[i].dim == dim {
						items[i].canon += c
						items[i].amount += in.Amount
					}
					continue
				}
				items[i] = found{present: true, amount: in.Amount, unit: in.Unit, canon: c, dim: dim, ok: ok}
			}
		}

		convertible := true
		var dim Dimension
		for _, f := range items {
			if !f.present {
				continue
			}
			if !f.ok || f.dim == DimTemperature || (dim != DimNone && f.dim != dim) {
				convertible = false
				break
			}
			dim = f.dim
		}

		values := make([]any, len(subjects))
		var unit *string
		if convertible && usePerLitre {
			u := "g/L"
			if dim == DimVolume {
				u = "ml/L"
			}
			unit = &u
			for i, f := range items {
				if f.present && subjects[i].VolumeMl != nil && *subjects[i].VolumeMl > 0 {
					values[i] = Round(f.canon/(*subjects[i].VolumeMl/1000), 1)
				}
			}
		} else if convertible {
			u := "g"
			if dim == DimVolume {
				u = "ml"
			}
			// Large volumes read better in litres (water: 3.8 L, not 3800 ml).
			litres := dim == DimVolume
			for _, f := range items {
				if f.present && f.canon < 1000 {
					litres = false
				}
			}
			if litres {
				u = "L"
			}
			unit = &u
			for i, f := range items {
				if !f.present {
					continue
				}
				if litres {
					values[i] = Round(f.canon/1000, 2)
				} else {
					values[i] = Round(f.canon, 1)
				}
			}
		} else {
			for i, f := range items {
				if f.present {
					values[i] = formatAmount(f.amount, f.unit)
				}
			}
		}
		rows = append(rows, makeRow("ingredients."+k.role+"."+k.slug, labels[k], GroupIngredients, CatRecipe, unit, values))
	}
	return rows
}

// ---------------------------------------------------------------- bottle additions

func additionRows(subjects []Subject) []Row {
	var rows []Row
	rows = appendRow(rows, "additions.flavorName", "Flavour", GroupAdditions, CatVariation, nil,
		pick(subjects, func(x Subject) any {
			if x.Bottle == nil {
				return nil
			}
			return trimmedOrNil(&x.Bottle.FlavorName)
		}))

	var order []string
	labels := map[string]string{}
	for _, x := range subjects {
		if x.Bottle == nil {
			continue
		}
		for _, a := range x.Bottle.Additions {
			k := Slug(a.Name)
			if _, seen := labels[k]; !seen {
				order = append(order, k)
				labels[k] = strings.TrimSpace(a.Name)
			}
		}
	}
	for _, k := range order {
		adds := make([]*Addition, len(subjects))
		for i, x := range subjects {
			if x.Bottle == nil {
				continue
			}
			for j := range x.Bottle.Additions {
				if Slug(x.Bottle.Additions[j].Name) == k {
					adds[i] = &x.Bottle.Additions[j]
					break
				}
			}
		}
		forms := make([]any, len(subjects))
		for i, a := range adds {
			if a != nil {
				forms[i] = a.Form
			}
		}
		rows = append(rows, makeRow("additions."+k+".form", labels[k]+" form", GroupAdditions, CatRecipe, nil, forms))

		// Amounts in one dimension compare as numbers (15 ml = 0.015 L); mixed
		// dimensions (15 ml vs 30 g) compare as text.
		var dim Dimension
		convertible := true
		for _, a := range adds {
			if a == nil || a.Amount == nil {
				continue
			}
			u := ""
			if a.Unit != nil {
				u = *a.Unit
			}
			_, d, ok := ToCanonical(*a.Amount, u)
			if !ok || d == DimTemperature || (dim != DimNone && d != dim) {
				convertible = false
				break
			}
			dim = d
		}
		amounts := make([]any, len(subjects))
		var unit *string
		if convertible && dim != DimNone {
			u := "g"
			if dim == DimVolume {
				u = "ml"
			}
			unit = &u
		}
		for i, a := range adds {
			if a == nil || a.Amount == nil {
				continue
			}
			u := ""
			if a.Unit != nil {
				u = *a.Unit
			}
			if unit != nil {
				c, _, _ := ToCanonical(*a.Amount, u)
				amounts[i] = Round(c, 1)
			} else {
				amounts[i] = formatAmount(*a.Amount, u)
			}
		}
		rows = appendRow(rows, "additions."+k+".amount", labels[k]+" amount", GroupAdditions, CatRecipe, unit, amounts)
	}

	g := "g"
	rows = appendRow(rows, "additions.primingSugarG", "Priming sugar", GroupAdditions, CatRecipe, &g,
		pick(subjects, func(x Subject) any {
			if x.Bottle == nil {
				return nil
			}
			return numOrNil(x.Bottle.PrimingSugarG, 1)
		}))
	return rows
}

// ---------------------------------------------------------------- measurements

func measurementRows(subjects []Subject, o Options) []Row {
	var rows []Row
	for _, d := range o.Defs {
		has := false
		for _, x := range subjects {
			if _, ok := x.Measurements[d.Key]; ok {
				has = true
			}
		}
		if !has {
			continue
		}
		cat := CatMeasurement
		unit := d.Unit
		if d.Quantity == string(DimTemperature) {
			cat = CatEnvironment
			unit = TempLabel(o.TempUnit)
		}
		var unitPtr *string
		if (d.Kind == "numeric" || d.Kind == "scale") && unit != "" {
			unitPtr = &unit
		}
		show := func(r *Reading) any {
			switch {
			case r == nil:
				return nil
			case r.Num != nil:
				v := *r.Num
				if d.Quantity == string(DimTemperature) {
					v = CelsiusTo(v, o.TempUnit)
				}
				return Round(v, d.Decimals)
			case r.Bool != nil:
				if *r.Bool {
					return "Yes"
				}
				return "No"
			case r.Text != nil:
				return *r.Text
			}
			return nil
		}
		rows = appendRow(rows, "measurements."+d.Key+".last", d.Label, GroupMeasurements, cat, unitPtr,
			pick(subjects, func(x Subject) any { return show(x.Measurements[d.Key].Last) }))
		rows = appendRow(rows, "measurements."+d.Key+".first", d.Label+", first reading", GroupMeasurements, cat, unitPtr,
			pick(subjects, func(x Subject) any { return show(x.Measurements[d.Key].First) }))
	}
	return rows
}

// ---------------------------------------------------------------- result

func resultRows(subjects []Subject) []Row {
	var rows []Row
	five := "/5"
	score := func(f func(Rating) *int) []any {
		return pick(subjects, func(x Subject) any {
			if x.Rating == nil {
				return nil
			}
			if v := f(*x.Rating); v != nil {
				return float64(*v)
			}
			return nil
		})
	}
	rows = appendRow(rows, "result.overall", "Overall", GroupResult, CatOutcome, &five,
		score(func(r Rating) *int { return &r.Overall }))
	rows = appendRow(rows, "result.fizz", "Fizz", GroupResult, CatOutcome, &five, score(func(r Rating) *int { return r.Fizz }))
	rows = appendRow(rows, "result.sweetTart", "Sweet ↔ tart", GroupResult, CatOutcome, &five,
		score(func(r Rating) *int { return r.SweetTart }))
	rows = appendRow(rows, "result.flavor", "Flavour", GroupResult, CatOutcome, &five, score(func(r Rating) *int { return r.Flavor }))
	rows = appendRow(rows, "result.wouldRepeat", "Brew again?", GroupResult, CatOutcome, nil,
		pick(subjects, func(x Subject) any {
			if x.Rating == nil || x.Rating.WouldRepeat == nil {
				return nil
			}
			if *x.Rating.WouldRepeat {
				return "Yes"
			}
			return "No"
		}))
	rows = appendRow(rows, "result.offNotes", "Off-notes", GroupResult, CatOutcome, nil,
		pick(subjects, func(x Subject) any {
			if x.Rating == nil || len(x.Rating.OffNotes) == 0 {
				return nil
			}
			notes := make([]string, 0, len(x.Rating.OffNotes))
			for _, n := range x.Rating.OffNotes {
				if t := strings.TrimSpace(strings.ToLower(n)); t != "" {
					notes = append(notes, t)
				}
			}
			sort.Strings(notes)
			return strings.Join(notes, ", ")
		}))
	return rows
}

// DescribeChange renders a changed row as "Label: a → b" for change notes and logs.
func DescribeChange(r Row) string {
	f := func(v any) string {
		switch t := v.(type) {
		case nil:
			return "none"
		case float64:
			s := strconv.FormatFloat(t, 'f', -1, 64)
			if r.Unit != nil {
				s += " " + *r.Unit
			}
			return s
		default:
			return fmt.Sprint(t)
		}
	}
	if len(r.Values) < 2 {
		return r.Label
	}
	return fmt.Sprintf("%s: %s → %s", r.Label, f(r.Values[0]), f(r.Values[len(r.Values)-1]))
}
