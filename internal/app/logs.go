package app

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"jarlore/internal/apperr"
	"jarlore/internal/auth"
	"jarlore/internal/db/sqlc"
	"jarlore/internal/domain"
)

// CreateLogs records an observation on a batch, on one bottle, or on several
// bottles at once ("burp all": one entry per bottle). Measurements are stored as
// entered and in their canonical unit; they are never interpreted.
func (s *Service) CreateLogs(ctx context.Context, u auth.User, batchID, idemKey string, in CreateLogInput) (Result, error) {
	now := s.now()
	var f apperr.Fields
	if strings.TrimSpace(idemKey) == "" {
		return Result{}, apperr.Validation(apperr.FieldError{Field: "Idempotency-Key", Code: "required", Message: "Send an Idempotency-Key header with every log entry."})
	}
	if !domain.Contains(domain.UserLogKinds, in.Kind) {
		f.Add("kind", "invalid_enum", "kind must be note, taste, burp, measure, feed or issue.")
	}
	if in.BottleID != nil && len(in.BottleIDs) > 0 {
		f.Add("bottleIds", "exclusive", "Send bottleId or bottleIds, not both.")
	}
	if len(in.BottleIDs) > 50 {
		f.Add("bottleIds", "too_many", "At most 50 bottles.")
	}
	seen := map[string]bool{}
	for i, id := range in.BottleIDs {
		if !isUUID(id) || seen[id] {
			f.Add(fmt.Sprintf("bottleIds[%d]", i), "invalid", "Bottle ids must be valid and unique.")
		}
		seen[id] = true
	}
	if in.BottleID != nil && !isUUID(*in.BottleID) {
		f.Add("bottleId", "invalid", "bottleId is not valid.")
	}
	notFuture(&f, "occurredAt", in.OccurredAt, now)
	textLen(&f, "note", in.Note, 0, 4000)
	if len(in.Measurements) > 20 {
		f.Add("measurements", "too_many", "At most 20 measurements per entry.")
	}
	if in.Kind == "measure" && len(in.Measurements) == 0 {
		f.Add("measurements", "required", "Add at least one reading.")
	}
	if err := f.Err(); err != nil {
		return Result{}, err
	}
	return s.write(ctx, u, "createLogs:"+batchID, idemKey, in, http.StatusCreated, func(q *sqlc.Queries) (any, error) {
		batch, err := q.GetBatchForUpdate(ctx, sqlc.GetBatchForUpdateParams{ID: batchID, UserID: u.ID})
		if err != nil {
			return nil, notFoundIf(err, "Batch")
		}
		if batch.Status != "active" && !domain.Contains(domain.ClosedBatchKinds, in.Kind) {
			return nil, apperr.InvalidState("This batch is " + batch.Status + ". You can still add notes, tastings and readings.")
		}
		defs, err := loadDefs(ctx, q, u.ID)
		if err != nil {
			return nil, err
		}
		readings, err := validateMeasurements(defs, in.Measurements)
		if err != nil {
			return nil, err
		}

		// Resolve the bottles: each must belong to this batch.
		targets := []*string{nil}
		ids := in.BottleIDs
		if in.BottleID != nil {
			ids = []string{*in.BottleID}
		}
		if len(ids) > 0 {
			rows, err := q.GetBottlesByIDs(ctx, sqlc.GetBottlesByIDsParams{UserID: u.ID, Ids: ids})
			if err != nil {
				return nil, err
			}
			byID := map[string]sqlc.Bottle{}
			for _, b := range rows {
				byID[b.ID] = b
			}
			targets = targets[:0]
			for i, id := range ids {
				b, ok := byID[id]
				if !ok || b.BatchID != batchID {
					return nil, apperr.NotFound(fmt.Sprintf("Bottle %d", i+1))
				}
				if in.Kind == "burp" && b.Status != "conditioning" && b.Status != "chilled" {
					return nil, apperr.InvalidState("Bottle " + b.Label + " is " + b.Status + ".")
				}
				targets = append(targets, &b.ID)
			}
		}

		at := now
		if in.OccurredAt != nil {
			at = in.OccurredAt.UTC()
		}
		profile, err := q.GetProfile(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		dayEnd := endOfLocalDay(now, profile.Timezone)

		var created []sqlc.BatchLog
		for _, bottleID := range targets {
			l, err := q.InsertLog(ctx, sqlc.InsertLogParams{UserID: u.ID, BatchID: batchID, BottleID: bottleID, Kind: in.Kind, OccurredAt: at, Note: trimPtr(in.Note)})
			if err != nil {
				return nil, err
			}
			for i, r := range readings {
				r.UserID, r.LogID, r.BatchID, r.Position = u.ID, l.ID, batchID, int16(i)
				if err := q.InsertMeasurement(ctx, r); err != nil {
					return nil, err
				}
			}
			created = append(created, l)

			// A matching log completes today's reminders; burping schedules tomorrow's.
			switch in.Kind {
			case "taste":
				if err := q.CompleteDueReminders(ctx, sqlc.CompleteDueRemindersParams{UserID: u.ID, BatchID: batchID, Kinds: []string{"taste_check"}, Before: dayEnd}); err != nil {
					return nil, err
				}
			case "burp":
				if err := q.CompleteDueReminders(ctx, sqlc.CompleteDueRemindersParams{UserID: u.ID, BatchID: batchID, BottleID: bottleID, Kinds: []string{"burp"}, Before: dayEnd}); err != nil {
					return nil, err
				}
				if bottleID != nil {
					if err := s.scheduleNextBurp(ctx, q, u.ID, batch, *bottleID, profile.Timezone, now); err != nil {
						return nil, err
					}
				}
			}
		}
		if err := q.TouchBatch(ctx, sqlc.TouchBatchParams{ID: batchID, UserID: u.ID}); err != nil {
			return nil, err
		}
		entries, err := loadLogs(ctx, q, u.ID, created)
		if err != nil {
			return nil, err
		}
		return map[string]any{"data": entries}, nil
	})
}

// scheduleNextBurp adds tomorrow's burp reminder for a conditioning bottle while
// it is inside the type's typical F2 window. References only, never a verdict.
func (s *Service) scheduleNextBurp(ctx context.Context, q *sqlc.Queries, userID string, batch sqlc.Batch, bottleID, tz string, now time.Time) error {
	open, err := q.HasOpenReminder(ctx, sqlc.HasOpenReminderParams{UserID: userID, BatchID: batch.ID, Kind: "burp", BottleID: &bottleID})
	if err != nil || open {
		return err
	}
	bo, err := q.GetBottle(ctx, sqlc.GetBottleParams{ID: bottleID, UserID: userID})
	if err != nil || bo.Status != "conditioning" {
		return err
	}
	ft, err := q.GetFermentationType(ctx, batch.FermentationTypeID)
	if err != nil {
		return err
	}
	stages, err := parseStages(ft.Stages)
	if err != nil {
		return err
	}
	f2 := stageOf(stages, "f2")
	if f2 == nil {
		return nil
	}
	due := localMorning(now.Add(24*time.Hour), tz)
	if due.After(bo.StartedAt.Add(time.Duration(f2.TypicalDays[1]+1) * 24 * time.Hour)) {
		return nil
	}
	_, err = q.InsertReminder(ctx, sqlc.InsertReminderParams{UserID: userID, BatchID: batch.ID, BottleID: &bottleID, Kind: "burp", DueAt: due, Label: "Burp bottle " + bo.Label})
	return err
}

// validateMeasurements checks readings against their definitions and converts
// numbers to the canonical unit (22 °C and 71.6 °F are stored as the same value).
func validateMeasurements(defs defSet, list []domain.Measurement) ([]sqlc.InsertMeasurementParams, error) {
	var f apperr.Fields
	out := make([]sqlc.InsertMeasurementParams, 0, len(list))
	for i, m := range list {
		p := fmt.Sprintf("measurements[%d]", i)
		d, ok := defs.byKey[m.DefinitionKey]
		if !ok || d.ArchivedAt != nil {
			f.Add(p+".definitionKey", "unknown", "Unknown measurement "+m.DefinitionKey+".")
			continue
		}
		if m.Method != nil && !domain.Contains(domain.MeasureMethods, *m.Method) {
			f.Add(p+".method", "invalid_enum", "Unknown method.")
		}
		if d.RequiresMethod && m.Method == nil {
			f.Add(p+".method", "method_required", "Say how "+d.Label+" was measured (strip, meter…).")
		}
		row := sqlc.InsertMeasurementParams{DefinitionID: d.ID, Method: m.Method, Unit: strings.TrimSpace(m.Unit)}
		switch d.Kind {
		case "numeric", "scale":
			if m.Value.Num == nil || math.IsNaN(*m.Value.Num) || math.IsInf(*m.Value.Num, 0) {
				f.Add(p+".value", "invalid_type", d.Label+" needs a number.")
				continue
			}
			v := *m.Value.Num
			canon := v
			if d.Quantity != nil {
				if row.Unit == "" {
					row.Unit = d.Unit
				}
				c, dim, ok := domain.ToCanonical(v, row.Unit)
				if !ok || string(dim) != *d.Quantity {
					f.Add(p+".unit", "invalid_unit", fmt.Sprintf("%q is not a unit of %s.", row.Unit, *d.Quantity))
					continue
				}
				canon = c
			} else {
				if row.Unit != "" && domain.NormalizeUnit(row.Unit) != domain.NormalizeUnit(d.Unit) {
					f.Add(p+".unit", "invalid_unit", d.Label+" is recorded in "+d.Unit+".")
					continue
				}
				row.Unit = d.Unit
			}
			// Ranges are sanity checks on the stored canonical value, not judgements.
			if (d.MinValue != nil && canon < *d.MinValue) || (d.MaxValue != nil && canon > *d.MaxValue) {
				f.Add(p+".value", "out_of_range", fmt.Sprintf("%s must be between %s and %s %s.", d.Label, fmtNum(d.MinValue), fmtNum(d.MaxValue), d.Unit))
				continue
			}
			if d.Kind == "scale" && canon != math.Trunc(canon) {
				f.Add(p+".value", "invalid", d.Label+" is a whole-number scale.")
				continue
			}
			row.ValueNum, row.ValueCanonical = &v, &canon
		case "boolean":
			if m.Value.Bool == nil {
				f.Add(p+".value", "invalid_type", d.Label+" is yes or no.")
				continue
			}
			row.ValueBool, row.Unit = m.Value.Bool, ""
		case "text":
			if m.Value.Text == nil || strings.TrimSpace(*m.Value.Text) == "" || len([]rune(*m.Value.Text)) > 500 {
				f.Add(p+".value", "invalid", d.Label+" needs a short text (up to 500 characters).")
				continue
			}
			t := strings.TrimSpace(*m.Value.Text)
			row.ValueText, row.Unit = &t, ""
		}
		out = append(out, row)
	}
	return out, f.Err()
}

func fmtNum(v *float64) string {
	if v == nil {
		return "any"
	}
	return fmt.Sprintf("%g", *v)
}

func (s *Service) ListLogs(ctx context.Context, u auth.User, batchID string, bottleID *string, cursorToken string, limit int) (Page[LogEntry], error) {
	var page Page[LogEntry]
	scope := "logs|" + batchID + "|" + deref(bottleID)
	cur, err := s.decodeCursor(cursorToken, scope)
	if err != nil {
		return page, err
	}
	lim := pageLimit(limit)
	err = s.read(ctx, u, func(q *sqlc.Queries) error {
		if _, err := q.GetBatch(ctx, sqlc.GetBatchParams{ID: batchID, UserID: u.ID}); err != nil {
			return notFoundIf(err, "Batch")
		}
		p := sqlc.ListLogsParams{UserID: u.ID, BatchID: batchID, BottleID: bottleID, Lim: lim + 1}
		if cur != nil {
			p.AfterTime, p.AfterID = cur.Time, &cur.ID
		}
		rows, err := q.ListLogs(ctx, p)
		if err != nil {
			return err
		}
		if len(rows) > int(lim) {
			rows = rows[:lim]
			last := rows[len(rows)-1]
			page.NextCursor = s.encodeCursor(cursor{Scope: scope, Time: ptr(last.OccurredAt), ID: last.ID})
		}
		page.Data, err = loadLogs(ctx, q, u.ID, rows)
		return err
	})
	return page, err
}

// DeleteLog soft-deletes an entry: it disappears from reads, diffs and compares
// but stays in the database (and in exports).
func (s *Service) DeleteLog(ctx context.Context, u auth.User, id string) error {
	_, err := s.write(ctx, u, "deleteLog", "", nil, http.StatusNoContent, func(q *sqlc.Queries) (any, error) {
		l, err := q.GetLog(ctx, sqlc.GetLogParams{ID: id, UserID: u.ID})
		if err != nil {
			return nil, notFoundIf(err, "Log entry")
		}
		if l.Kind == "stage_change" {
			return nil, apperr.Conflict("invalid_state_transition", "Stage changes are written by Jarlore and can't be deleted.")
		}
		if l.DeletedAt != nil {
			return nil, nil
		}
		return nil, q.SoftDeleteLog(ctx, sqlc.SoftDeleteLogParams{ID: id, UserID: u.ID})
	})
	return err
}
