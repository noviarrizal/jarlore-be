package app

import (
	"context"
	"net/http"
	"strings"
	"time"

	"jarlore/internal/apperr"
	"jarlore/internal/auth"
	"jarlore/internal/db/sqlc"
	"jarlore/internal/domain"
)

func toMe(p sqlc.Profile) Me {
	return Me{ID: p.ID, Email: p.Email, DisplayName: p.DisplayName, UnitSystem: p.UnitSystem, TempUnit: p.TempUnit, Timezone: p.Timezone, CreatedAt: p.CreatedAt.UTC()}
}

func (s *Service) Me(ctx context.Context, u auth.User) (Me, error) {
	var out Me
	err := s.read(ctx, u, func(q *sqlc.Queries) error {
		p, err := q.GetProfile(ctx, u.ID)
		out = toMe(p)
		return err
	})
	return out, err
}

func (s *Service) UpdateMe(ctx context.Context, u auth.User, in UpdateMeInput) (Result, error) {
	var f apperr.Fields
	if in.DisplayName.Set {
		textLen(&f, "displayName", in.DisplayName.Value, 0, 80)
	}
	if in.UnitSystem != nil && *in.UnitSystem != "metric" && *in.UnitSystem != "us" {
		f.Add("unitSystem", "invalid_enum", "unitSystem is metric or us.")
	}
	if in.TempUnit != nil && *in.TempUnit != "C" && *in.TempUnit != "F" {
		f.Add("tempUnit", "invalid_enum", "tempUnit is C or F.")
	}
	if in.Timezone != nil {
		if _, err := time.LoadLocation(*in.Timezone); err != nil || *in.Timezone == "" || strings.EqualFold(*in.Timezone, "local") {
			f.Add("timezone", "invalid", "Use an IANA time zone such as Asia/Jakarta.")
		}
	}
	if !in.DisplayName.Set && in.UnitSystem == nil && in.TempUnit == nil && in.Timezone == nil {
		f.Add("body", "empty", "Send at least one field to change.")
	}
	if err := f.Err(); err != nil {
		return Result{}, err
	}
	return s.write(ctx, u, "updateMe", "", nil, http.StatusOK, func(q *sqlc.Queries) (any, error) {
		p, err := q.UpdateProfile(ctx, sqlc.UpdateProfileParams{
			ID: u.ID, SetDisplayName: in.DisplayName.Set, DisplayName: trimPtr(in.DisplayName.Value),
			UnitSystem: in.UnitSystem, TempUnit: in.TempUnit, Timezone: in.Timezone,
		})
		if err != nil {
			return nil, err
		}
		return toMe(p), nil
	})
}

func (s *Service) FermentationTypes(ctx context.Context, u auth.User) ([]FermentationType, error) {
	var out []FermentationType
	err := s.read(ctx, u, func(q *sqlc.Queries) error {
		rows, err := q.ListFermentationTypes(ctx)
		if err != nil {
			return err
		}
		for _, r := range rows {
			stages, err := parseStages(r.Stages)
			if err != nil {
				return err
			}
			t := FermentationType{ID: r.ID, Name: r.Name, Stages: stages, DefaultVolumeMl: int(r.DefaultVolumeMl)}
			if err := unmarshalInto(r.DefaultIngredients, &t.DefaultIngredients); err != nil {
				return err
			}
			if t.DefaultIngredients == nil {
				t.DefaultIngredients = []domain.IngredientInput{}
			}
			if err := unmarshalInto(r.DefaultParams, &t.DefaultParams); err != nil {
				return err
			}
			out = append(out, t)
		}
		return nil
	})
	return out, err
}

func (s *Service) MeasurementDefinitions(ctx context.Context, u auth.User) ([]MeasurementDefinition, error) {
	out := []MeasurementDefinition{}
	err := s.read(ctx, u, func(q *sqlc.Queries) error {
		rows, err := q.ListMeasurementDefinitions(ctx, &u.ID)
		if err != nil {
			return err
		}
		for _, r := range rows {
			out = append(out, toDefinition(r))
		}
		return nil
	})
	return out, err
}

func toDefinition(r sqlc.MeasurementDefinition) MeasurementDefinition {
	return MeasurementDefinition{ID: r.ID, Key: r.Key, Label: r.Label, Kind: r.Kind, Unit: r.Unit, Decimals: int(r.Decimals), Min: r.MinValue, Max: r.MaxValue, RequiresMethod: r.RequiresMethod, IsSystem: r.UserID == nil}
}

// CreateMeasurementDefinition adds a custom reading (numeric, scale, yes/no or
// text). The free plan keeps up to 3 active custom definitions.
func (s *Service) CreateMeasurementDefinition(ctx context.Context, u auth.User, in CreateDefinitionInput) (Result, error) {
	var f apperr.Fields
	kind := "numeric"
	if in.Kind != nil {
		kind = *in.Kind
	}
	if !domain.Contains(domain.MeasurementKinds, kind) {
		f.Add("kind", "invalid_enum", "kind is numeric, scale, boolean or text.")
	}
	if !keyRe(in.Key) {
		f.Add("key", "invalid", "Use 2–40 lower-case letters, digits or underscores.")
	}
	textLen(&f, "label", &in.Label, 1, 60)
	textLen(&f, "unit", &in.Unit, 0, 16)
	if in.Decimals < 0 || in.Decimals > 4 {
		f.Add("decimals", "out_of_range", "decimals is 0 to 4.")
	}
	if in.Min != nil && in.Max != nil && *in.Min > *in.Max {
		f.Add("min", "invalid", "min must not be above max.")
	}
	if kind == "scale" && (in.Min == nil || in.Max == nil) {
		f.Add("max", "required", "A scale needs min and max.")
	}
	if (kind == "boolean" || kind == "text") && (in.Min != nil || in.Max != nil) {
		f.Add("min", "invalid", "Only numbers have a range.")
	}
	if err := f.Err(); err != nil {
		return Result{}, err
	}
	return s.write(ctx, u, "createDefinition", "", nil, http.StatusCreated, func(q *sqlc.Queries) (any, error) {
		e, err := s.entitlements(ctx, q, u.ID)
		if err != nil {
			return nil, err
		}
		if e.Limits.CustomMeasurements != nil {
			n, err := q.CountCustomDefinitions(ctx, &u.ID)
			if err != nil {
				return nil, err
			}
			if n >= int64(*e.Limits.CustomMeasurements) {
				return nil, apperr.EntitlementRequired("The free plan keeps up to 3 custom measurements.")
			}
		}
		defs, err := loadDefs(ctx, q, u.ID)
		if err != nil {
			return nil, err
		}
		if _, taken := defs.byKey[in.Key]; taken {
			return nil, apperr.Conflict("duplicate_key", "A measurement with this key already exists.")
		}
		var quantity *string
		unit := strings.TrimSpace(in.Unit)
		if kind == "numeric" || kind == "scale" {
			if d := domain.UnitDimension(unit); d != domain.DimNone {
				quantity = ptr(string(d))
				unit = map[domain.Dimension]string{domain.DimMass: "g", domain.DimVolume: "ml", domain.DimTemperature: "°C"}[d]
			}
		} else {
			unit = ""
		}
		row, err := q.InsertMeasurementDefinition(ctx, sqlc.InsertMeasurementDefinitionParams{
			UserID: &u.ID, Key: in.Key, Label: strings.TrimSpace(in.Label), Kind: kind, Quantity: quantity, Unit: unit,
			Decimals: int16(in.Decimals), MinValue: in.Min, MaxValue: in.Max,
		})
		if err != nil {
			return nil, err
		}
		return toDefinition(row), nil
	})
}

func keyRe(k string) bool {
	if len(k) < 2 || len(k) > 40 {
		return false
	}
	for _, c := range k {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// Today is the dashboard for the user's local day.
func (s *Service) Today(ctx context.Context, u auth.User) (Today, error) {
	out := Today{Active: []BatchSummary{}, Due: []Reminder{}, RecentlyFinished: []BatchSummary{}}
	now := s.now()
	err := s.read(ctx, u, func(q *sqlc.Queries) error {
		p, err := q.GetProfile(ctx, u.ID)
		if err != nil {
			return err
		}
		active, err := q.ListActiveBatches(ctx, u.ID)
		if err != nil {
			return err
		}
		if out.Active, err = s.summaries(ctx, q, u.ID, active); err != nil {
			return err
		}
		due, err := q.ListDueReminders(ctx, sqlc.ListDueRemindersParams{UserID: u.ID, Before: endOfLocalDay(now, p.Timezone)})
		if err != nil {
			return err
		}
		for _, r := range due {
			out.Due = append(out.Due, toReminder(r))
		}
		recent, err := q.ListRecentlyFinished(ctx, sqlc.ListRecentlyFinishedParams{UserID: u.ID, Since: ptr(now.Add(-7 * 24 * time.Hour))})
		if err != nil {
			return err
		}
		out.RecentlyFinished, err = s.summaries(ctx, q, u.ID, recent)
		return err
	})
	return out, err
}

type ReminderQuery struct {
	Status, BatchID *string
	From, To        *time.Time
}

func (s *Service) ListReminders(ctx context.Context, u auth.User, in ReminderQuery) (Page[Reminder], error) {
	page := Page[Reminder]{Data: []Reminder{}}
	err := s.read(ctx, u, func(q *sqlc.Queries) error {
		rows, err := q.ListReminders(ctx, sqlc.ListRemindersParams{UserID: u.ID, Status: in.Status, BatchID: in.BatchID, FromTime: in.From, ToTime: in.To})
		for _, r := range rows {
			page.Data = append(page.Data, toReminder(r))
		}
		return err
	})
	return page, err
}

func (s *Service) UpdateReminder(ctx context.Context, u auth.User, id string, in UpdateReminderInput) (Result, error) {
	var f apperr.Fields
	if in.Status != nil && *in.Status != "done" && *in.Status != "snoozed" && *in.Status != "dismissed" {
		f.Add("status", "invalid_enum", "status is done, snoozed or dismissed.")
	}
	if in.Status != nil && *in.Status == "snoozed" && in.SnoozeUntil == nil {
		f.Add("snoozeUntil", "required", "Say until when to snooze.")
	}
	textLen(&f, "label", in.Label, 1, 120)
	if in.Status == nil && in.DueAt == nil && in.Label == nil {
		f.Add("body", "empty", "Send at least one field to change.")
	}
	if err := f.Err(); err != nil {
		return Result{}, err
	}
	return s.write(ctx, u, "updateReminder", "", nil, http.StatusOK, func(q *sqlc.Queries) (any, error) {
		cur, err := q.GetReminder(ctx, sqlc.GetReminderParams{ID: id, UserID: u.ID})
		if err != nil {
			return nil, notFoundIf(err, "Reminder")
		}
		if cur.Status == "done" || cur.Status == "dismissed" {
			return nil, apperr.InvalidState("This reminder is closed.")
		}
		due := in.DueAt
		if in.SnoozeUntil != nil {
			due = in.SnoozeUntil
		}
		r, err := q.UpdateReminder(ctx, sqlc.UpdateReminderParams{ID: id, UserID: u.ID, Status: in.Status, DueAt: due, Label: trimPtr(in.Label)})
		if err != nil {
			return nil, err
		}
		return toReminder(r), nil
	})
}
