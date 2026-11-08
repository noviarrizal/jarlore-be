package app

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"jarlore/internal/apperr"
	"jarlore/internal/domain"
	"jarlore/internal/httpx"
)

// Request bodies. Shapes follow docs/api/openapi.yaml; validation here is
// independent of any client-side validation.

type SourceRef struct {
	Type string `json:"type"` // recipe_version | batch | bottle
	ID   string `json:"id"`
}

type CreateBatchInput struct {
	FermentationTypeID string                   `json:"fermentationTypeId"`
	Name               *string                  `json:"name,omitempty"`
	StartedAt          *time.Time               `json:"startedAt,omitempty"`
	VolumeMl           int                      `json:"volumeMl"`
	Vessel             *string                  `json:"vessel"`
	Ingredients        []domain.IngredientInput `json:"ingredients"`
	Params             domain.BatchParams       `json:"params"`
	Source             *SourceRef               `json:"source,omitempty"`
	Hypothesis         *string                  `json:"hypothesis"`
}

type BottleInput struct {
	Label         string            `json:"label"`
	FlavorName    string            `json:"flavorName"`
	VolumeMl      int               `json:"volumeMl"`
	Additions     []domain.Addition `json:"additions"`
	PrimingSugarG *float64          `json:"primingSugarG"`
	HeadspaceCm   *float64          `json:"headspaceCm"`
	Source        *SourceRef        `json:"source,omitempty"`
	Hypothesis    *string           `json:"hypothesis,omitempty"`
}

func (b BottleInput) Spec() domain.BottleSpec {
	return domain.BottleSpec{FlavorName: strings.TrimSpace(b.FlavorName), VolumeMl: b.VolumeMl, Additions: b.Additions, PrimingSugarG: b.PrimingSugarG, HeadspaceCm: b.HeadspaceCm}
}

type CreateBottlesInput struct {
	Bottles   []BottleInput `json:"bottles"`
	BottledAt *time.Time    `json:"bottledAt,omitempty"`
}

type UpdateBottleInput struct {
	Label         *string                 `json:"label"`
	FlavorName    *string                 `json:"flavorName"`
	VolumeMl      *int                    `json:"volumeMl"`
	Additions     *[]domain.Addition      `json:"additions"`
	PrimingSugarG httpx.Optional[float64] `json:"primingSugarG"`
	HeadspaceCm   httpx.Optional[float64] `json:"headspaceCm"`
	Status        *string                 `json:"status"`
	IsReference   *bool                   `json:"isReference"`
}

type FinishInput struct {
	Rating     *domain.Rating `json:"rating"`
	FinishedAt *time.Time     `json:"finishedAt,omitempty"`
}

type DiscardInput struct {
	Reason string `json:"reason"`
}

type UpdateBatchInput struct {
	Name        *string                `json:"name"`
	Vessel      httpx.Optional[string] `json:"vessel"`
	IsReference *bool                  `json:"isReference"`
	Params      *struct {
		RoomTempC httpx.Optional[float64] `json:"roomTempC"`
	} `json:"params"`
}

type CreateLogInput struct {
	Kind         string               `json:"kind"`
	BottleID     *string              `json:"bottleId"`
	BottleIDs    []string             `json:"bottleIds"`
	OccurredAt   *time.Time           `json:"occurredAt,omitempty"`
	Note         *string              `json:"note"`
	Measurements []domain.Measurement `json:"measurements"`
}

type SaveAsRecipeInput struct {
	Name       *string `json:"name"`
	ChangeNote *string `json:"changeNote"`
	RecipeID   *string `json:"recipeId"`
}

type UpdateRecipeInput struct {
	Name     *string `json:"name"`
	Archived *bool   `json:"archived"`
}

type UpdateMeInput struct {
	DisplayName httpx.Optional[string] `json:"displayName"`
	UnitSystem  *string                `json:"unitSystem"`
	TempUnit    *string                `json:"tempUnit"`
	Timezone    *string                `json:"timezone"`
}

type CompareInput struct {
	Items           []ItemRef `json:"items"`
	OnlyDifferences bool      `json:"onlyDifferences"`
}

type BatchPreviewInput struct {
	Base  SourceRef        `json:"base"`
	Draft CreateBatchInput `json:"draft"`
}

type BottlePreviewInput struct {
	Base  SourceRef   `json:"base"`
	Draft BottleInput `json:"draft"`
}

type UpdateReminderInput struct {
	Status      *string    `json:"status"`
	SnoozeUntil *time.Time `json:"snoozeUntil"`
	DueAt       *time.Time `json:"dueAt"`
	Label       *string    `json:"label"`
}

type CreateDefinitionInput struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Kind     *string  `json:"kind"`
	Unit     string   `json:"unit"`
	Decimals int      `json:"decimals"`
	Min      *float64 `json:"min"`
	Max      *float64 `json:"max"`
}

// ---------------------------------------------------------------- validation helpers

const futureSkew = 5 * time.Minute

func textLen(f *apperr.Fields, field string, v *string, min, max int) {
	if v == nil {
		return
	}
	n := utf8.RuneCountInString(strings.TrimSpace(*v))
	if n < min {
		f.Add(field, "required", field+" can't be empty.")
	} else if n > max {
		f.Add(field, "too_long", fmt.Sprintf("%s must be at most %d characters.", field, max))
	}
}

func numRange(f *apperr.Fields, field string, v *float64, min, max float64) {
	if v == nil {
		return
	}
	if math.IsNaN(*v) || math.IsInf(*v, 0) || *v < min || *v > max {
		f.Add(field, "out_of_range", fmt.Sprintf("%s must be between %g and %g.", field, min, max))
	}
}

func notFuture(f *apperr.Fields, field string, t *time.Time, now time.Time) {
	if t != nil && t.After(now.Add(futureSkew)) {
		f.Add(field, "in_future", field+" can't be in the future.")
	}
}

func validIngredients(f *apperr.Fields, list []domain.IngredientInput) {
	if len(list) > 30 {
		f.Add("ingredients", "too_many", "A batch has at most 30 ingredients.")
	}
	for i, in := range list {
		p := fmt.Sprintf("ingredients[%d]", i)
		name := in.Name
		textLen(f, p+".name", &name, 1, 80)
		if !domain.Contains(domain.IngredientRoles, in.Role) {
			f.Add(p+".role", "invalid_enum", "Choose water, tea, sugar, starter, flavor or other.")
		}
		amt := in.Amount
		numRange(f, p+".amount", &amt, 0, 1e6)
		unit := in.Unit
		textLen(f, p+".unit", &unit, 1, 16)
	}
}

func validParams(f *apperr.Fields, p domain.BatchParams) {
	numRange(f, "params.targetF1Days", p.TargetF1Days, 0, 365)
	numRange(f, "params.roomTempC", p.RoomTempC, -10, 60)
	textLen(f, "params.starterSource", p.StarterSource, 0, 200)
}

func validAdditions(f *apperr.Fields, prefix string, list []domain.Addition) {
	if len(list) > 20 {
		f.Add(prefix, "too_many", "A bottle has at most 20 additions.")
	}
	for i, a := range list {
		p := fmt.Sprintf("%s[%d]", prefix, i)
		name := a.Name
		textLen(f, p+".name", &name, 1, 80)
		if !domain.Contains(domain.AdditionForms, a.Form) {
			f.Add(p+".form", "invalid_enum", "Choose juice, puree, chunks, whole, syrup, dried or other.")
		}
		numRange(f, p+".amount", a.Amount, 0, 1e5)
		textLen(f, p+".unit", a.Unit, 0, 16)
		if a.Amount != nil && (a.Unit == nil || strings.TrimSpace(*a.Unit) == "") {
			f.Add(p+".unit", "required", "Give the unit of the amount.")
		}
	}
}

func validBottle(f *apperr.Fields, p string, b BottleInput) {
	textLen(f, p+".label", &b.Label, 1, 8)
	textLen(f, p+".flavorName", &b.FlavorName, 1, 80)
	if b.VolumeMl < 1 || b.VolumeMl > 20000 {
		f.Add(p+".volumeMl", "out_of_range", "Bottle size must be between 1 and 20000 ml.")
	}
	validAdditions(f, p+".additions", b.Additions)
	numRange(f, p+".primingSugarG", b.PrimingSugarG, 0, 1000)
	numRange(f, p+".headspaceCm", b.HeadspaceCm, 0, 50)
	textLen(f, p+".hypothesis", b.Hypothesis, 0, 1000)
	validSource(f, p+".source", b.Source, "recipe_version", "bottle")
}

func validSource(f *apperr.Fields, field string, s *SourceRef, types ...string) {
	if s == nil {
		return
	}
	if !domain.Contains(types, s.Type) {
		f.Add(field+".type", "invalid_enum", "Source type must be one of "+strings.Join(types, ", ")+".")
	}
	if !isUUID(s.ID) {
		f.Add(field+".id", "invalid", "Source id is not valid.")
	}
}

func validRating(f *apperr.Fields, r *domain.Rating) {
	if r == nil {
		f.Add("rating", "required", "Give an overall score from 1 to 5.")
		return
	}
	score := func(field string, v *int) {
		if v != nil && (*v < 1 || *v > 5) {
			f.Add(field, "out_of_range", "Scores go from 1 to 5.")
		}
	}
	o := r.Overall
	score("rating.overall", &o)
	score("rating.fizz", r.Fizz)
	score("rating.sweetTart", r.SweetTart)
	score("rating.flavor", r.Flavor)
	if len(r.OffNotes) > 20 {
		f.Add("rating.offNotes", "too_many", "At most 20 off-notes.")
	}
	for i, n := range r.OffNotes {
		textLen(f, fmt.Sprintf("rating.offNotes[%d]", i), &n, 1, 40)
	}
	textLen(f, "rating.note", r.Note, 0, 2000)
	if r.OffNotes == nil {
		r.OffNotes = []string{}
	}
}

func (in *CreateBatchInput) validate(now time.Time) error {
	var f apperr.Fields
	if !domain.Contains(domain.FermentationTypes, in.FermentationTypeID) {
		f.Add("fermentationTypeId", "invalid_enum", "Choose kombucha, water_kefir or custom.")
	}
	textLen(&f, "name", in.Name, 1, 120)
	notFuture(&f, "startedAt", in.StartedAt, now)
	if in.VolumeMl < 1 || in.VolumeMl > 200000 {
		f.Add("volumeMl", "out_of_range", "Batch size must be between 1 ml and 200 L.")
	}
	textLen(&f, "vessel", in.Vessel, 0, 80)
	if in.Ingredients == nil {
		in.Ingredients = []domain.IngredientInput{}
	}
	validIngredients(&f, in.Ingredients)
	validParams(&f, in.Params)
	validSource(&f, "source", in.Source, "recipe_version", "batch")
	textLen(&f, "hypothesis", in.Hypothesis, 0, 1000)
	return f.Err()
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
	}
	return true
}

// IsUUID is exported for handlers validating path parameters.
func IsUUID(s string) bool { return isUUID(s) }

func trimPtr(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	if t == "" {
		return nil
	}
	return &t
}
