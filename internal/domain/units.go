package domain

import (
	"math"
	"strings"
)

// Dimension is the physical quantity a unit measures.
type Dimension string

const (
	DimNone        Dimension = ""
	DimMass        Dimension = "mass"        // canonical g
	DimVolume      Dimension = "volume"      // canonical ml
	DimTemperature Dimension = "temperature" // canonical °C
)

type unitInfo struct {
	dim    Dimension
	factor float64 // multiply to get the canonical unit (mass, volume)
}

var units = map[string]unitInfo{
	"mg": {DimMass, 0.001}, "g": {DimMass, 1}, "kg": {DimMass, 1000},
	"oz": {DimMass, 28.349523125}, "lb": {DimMass, 453.59237},
	"ml": {DimVolume, 1}, "cl": {DimVolume, 10}, "dl": {DimVolume, 100}, "l": {DimVolume, 1000},
	"tsp": {DimVolume, 4.92892159375}, "tbsp": {DimVolume, 14.78676478125},
	"cup": {DimVolume, 236.5882365}, "floz": {DimVolume, 29.5735295625},
	"gal": {DimVolume, 3785.411784}, "qt": {DimVolume, 946.352946}, "pt": {DimVolume, 473.176473},
	"c": {DimTemperature, 1}, "f": {DimTemperature, 1},
}

var unitAliases = map[string]string{
	"gram": "g", "grams": "g", "gr": "g", "kilogram": "kg", "kilograms": "kg",
	"milliliter": "ml", "millilitre": "ml", "milliliters": "ml", "millilitres": "ml", "mls": "ml",
	"liter": "l", "litre": "l", "liters": "l", "litres": "l", "ltr": "l",
	"teaspoon": "tsp", "teaspoons": "tsp", "tablespoon": "tbsp", "tablespoons": "tbsp",
	"cups": "cup", "fl oz": "floz", "fl. oz": "floz", "fl.oz": "floz",
	"°c": "c", "ºc": "c", "celsius": "c", "°f": "f", "ºf": "f", "fahrenheit": "f",
	"ounce": "oz", "ounces": "oz", "pound": "lb", "pounds": "lb", "lbs": "lb",
}

// NormalizeUnit lower-cases and resolves aliases ("L" -> "l", "°C" -> "c").
// Unknown units (for example "bags") are returned lower-cased and trimmed.
func NormalizeUnit(u string) string {
	s := strings.ToLower(strings.TrimSpace(u))
	s = strings.Join(strings.Fields(s), " ")
	if a, ok := unitAliases[s]; ok {
		return a
	}
	return s
}

// UnitDimension reports the dimension of a unit, or DimNone when it isn't convertible.
func UnitDimension(u string) Dimension {
	return units[NormalizeUnit(u)].dim
}

// ToCanonical converts an amount to the canonical unit of its dimension
// (g, ml, °C). ok is false for units that can't be converted.
func ToCanonical(amount float64, unit string) (value float64, dim Dimension, ok bool) {
	n := NormalizeUnit(unit)
	info, found := units[n]
	if !found {
		return 0, DimNone, false
	}
	if info.dim == DimTemperature {
		if n == "f" {
			return (amount - 32) * 5 / 9, DimTemperature, true
		}
		return amount, DimTemperature, true
	}
	return amount * info.factor, info.dim, true
}

// CelsiusTo converts a canonical °C value to the user's preferred unit ("C" or "F").
func CelsiusTo(c float64, tempUnit string) float64 {
	if strings.EqualFold(tempUnit, "F") {
		return c*9/5 + 32
	}
	return c
}

// TempLabel is the display unit for temperatures.
func TempLabel(tempUnit string) string {
	if strings.EqualFold(tempUnit, "F") {
		return "°F"
	}
	return "°C"
}

// Round rounds half away from zero to the given number of decimals.
func Round(v float64, decimals int) float64 {
	p := math.Pow(10, float64(decimals))
	r := math.Round(v*p) / p
	if r == 0 {
		return 0 // no negative zero in JSON
	}
	return r
}

// Slug normalises a free-text name for matching: lower-case, trimmed, internal
// whitespace collapsed to "_". "Cane  Sugar" and "cane sugar" are the same ingredient.
func Slug(name string) string {
	return strings.Join(strings.Fields(strings.ToLower(name)), "_")
}
