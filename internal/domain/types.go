// Package domain holds Jarlore's pure business rules: value objects, unit
// normalisation, the diff engine and the recipe version rule. It has no I/O.
package domain

import (
	"bytes"
	"encoding/json"
	"errors"
)

// Ingredient roles, addition forms and other vocabularies from the API contract.
var (
	IngredientRoles   = []string{"water", "tea", "sugar", "starter", "flavor", "other"}
	AdditionForms     = []string{"juice", "puree", "chunks", "whole", "syrup", "dried", "other"}
	MeasureMethods    = []string{"strip", "meter", "refractometer", "hydrometer", "thermometer", "sensor", "taste", "estimate", "other"}
	UserLogKinds      = []string{"note", "taste", "burp", "measure", "feed", "issue"}
	ClosedBatchKinds  = []string{"note", "taste", "measure"}
	MeasurementKinds  = []string{"numeric", "scale", "boolean", "text"}
	BottleUserStatus  = []string{"conditioning", "chilled", "discarded"}
	FermentationTypes = []string{"kombucha", "water_kefir", "custom"}
)

// IngredientInput is an ingredient as the client sends it.
type IngredientInput struct {
	Name   string  `json:"name"`
	Role   string  `json:"role"`
	Amount float64 `json:"amount"`
	Unit   string  `json:"unit"`
}

// Ingredient is a stored ingredient line.
type Ingredient struct {
	ID string `json:"id"`
	IngredientInput
}

// Addition is something added to one F2 bottle (ginger juice, raspberries).
type Addition struct {
	Name   string   `json:"name"`
	Form   string   `json:"form"`
	Amount *float64 `json:"amount"`
	Unit   *string  `json:"unit"`
}

// BatchParams are the batch parameters. targetF1Days is recipe-level; roomTempC
// and starterSource are conditions of this particular batch.
type BatchParams struct {
	TargetF1Days  *float64 `json:"targetF1Days,omitempty"`
	RoomTempC     *float64 `json:"roomTempC,omitempty"`
	StarterSource *string  `json:"starterSource,omitempty"`
}

// RecipeParams returns only the recipe-level part of the params.
func (p BatchParams) RecipeParams() BatchParams {
	return BatchParams{TargetF1Days: p.TargetF1Days}
}

// Rating is the result of a finished batch or bottle.
type Rating struct {
	Overall     int      `json:"overall"`
	Fizz        *int     `json:"fizz"`
	SweetTart   *int     `json:"sweetTart"`
	Flavor      *int     `json:"flavor"`
	OffNotes    []string `json:"offNotes"`
	WouldRepeat *bool    `json:"wouldRepeat"`
	Note        *string  `json:"note"`
}

// Value is a measurement value: a number, a boolean or a text, depending on the
// definition's kind. Exactly one field is set.
type Value struct {
	Num  *float64
	Bool *bool
	Text *string
}

func NumValue(v float64) Value { return Value{Num: &v} }

func (v Value) MarshalJSON() ([]byte, error) {
	switch {
	case v.Num != nil:
		return json.Marshal(*v.Num)
	case v.Bool != nil:
		return json.Marshal(*v.Bool)
	case v.Text != nil:
		return json.Marshal(*v.Text)
	}
	return []byte("null"), nil
}

func (v *Value) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		return errors.New("value is required")
	}
	switch b[0] {
	case '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		v.Text = &s
	case 't', 'f':
		var x bool
		if err := json.Unmarshal(b, &x); err != nil {
			return err
		}
		v.Bool = &x
	default:
		var f float64
		if err := json.Unmarshal(b, &f); err != nil {
			return err
		}
		v.Num = &f
	}
	return nil
}

// Measurement is one reading attached to a log entry.
type Measurement struct {
	DefinitionKey string  `json:"definitionKey"`
	Value         Value   `json:"value"`
	Unit          string  `json:"unit"`
	Method        *string `json:"method"`
}

// MeasurementDef is what the domain needs to know about a measurement definition.
type MeasurementDef struct {
	Key            string
	Label          string
	Kind           string // numeric | scale | boolean | text
	Quantity       string // temperature | volume | mass | ""
	Unit           string // canonical unit
	Decimals       int
	Min, Max       *float64
	RequiresMethod bool
}

// BottleSpec is the recipe-level configuration of one F2 bottle.
type BottleSpec struct {
	FlavorName    string     `json:"flavorName"`
	VolumeMl      int        `json:"volumeMl"`
	Additions     []Addition `json:"additions"`
	PrimingSugarG *float64   `json:"primingSugarG"`
	HeadspaceCm   *float64   `json:"headspaceCm"`
}

// DefaultBottle is a suggested bottle stored on a batch-scope recipe version.
type DefaultBottle struct {
	FlavorName    string     `json:"flavorName"`
	Additions     []Addition `json:"additions"`
	PrimingSugarG *float64   `json:"primingSugarG"`
}

func Contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
