package app

import (
	"context"
	"sort"
	"time"

	"jarlore/internal/db/sqlc"
	"jarlore/internal/domain"
)

// Loading aggregates: a fixed number of queries per list, never one per row.

func toReminder(r sqlc.Reminder) Reminder {
	return Reminder{ID: r.ID, BatchID: r.BatchID, BottleID: r.BottleID, Kind: r.Kind, DueAt: r.DueAt.UTC(), Status: r.Status, Label: r.Label}
}

func ratingOf(raw []byte) (*domain.Rating, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var r domain.Rating
	if err := unmarshalInto(raw, &r); err != nil {
		return nil, err
	}
	if r.OffNotes == nil {
		r.OffNotes = []string{}
	}
	return &r, nil
}

// loadLogs attaches measurements to log rows.
func loadLogs(ctx context.Context, q *sqlc.Queries, userID string, rows []sqlc.BatchLog) ([]LogEntry, error) {
	ids := make([]string, len(rows))
	for i, l := range rows {
		ids[i] = l.ID
	}
	ms, err := q.ListMeasurementsByLogIDs(ctx, sqlc.ListMeasurementsByLogIDsParams{UserID: userID, LogIds: ids})
	if err != nil {
		return nil, err
	}
	byLog := map[string][]domain.Measurement{}
	for _, m := range ms {
		byLog[m.LogID] = append(byLog[m.LogID], domain.Measurement{
			DefinitionKey: m.DefinitionKey, Value: domain.Value{Num: m.ValueNum, Bool: m.ValueBool, Text: m.ValueText}, Unit: m.Unit, Method: m.Method,
		})
	}
	out := make([]LogEntry, len(rows))
	for i, l := range rows {
		list := byLog[l.ID]
		if list == nil {
			list = []domain.Measurement{}
		}
		out[i] = LogEntry{ID: l.ID, BatchID: l.BatchID, BottleID: l.BottleID, Kind: l.Kind, OccurredAt: l.OccurredAt.UTC(), Note: l.Note, Measurements: list, CreatedAt: l.CreatedAt.UTC()}
	}
	return out, nil
}

// versionRefs resolves recipe version ids to {recipe, versionNo} references.
func versionRefs(ctx context.Context, q *sqlc.Queries, userID string, ids []string) (map[string]*RecipeVersionRef, error) {
	out := map[string]*RecipeVersionRef{}
	if len(ids) == 0 {
		return out, nil
	}
	versions, err := q.GetRecipeVersionsByIDs(ctx, sqlc.GetRecipeVersionsByIDsParams{UserID: userID, Ids: ids})
	if err != nil {
		return nil, err
	}
	recipeIDs := make([]string, 0, len(versions))
	for _, v := range versions {
		recipeIDs = append(recipeIDs, v.RecipeID)
	}
	recipes, err := q.GetRecipesByIDs(ctx, sqlc.GetRecipesByIDsParams{UserID: userID, Ids: recipeIDs})
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, r := range recipes {
		names[r.ID] = r.Name
	}
	for _, v := range versions {
		out[v.ID] = &RecipeVersionRef{RecipeID: v.RecipeID, RecipeName: names[v.RecipeID], VersionID: v.ID, VersionNo: int(v.VersionNo), Scope: v.Scope}
	}
	return out, nil
}

type batchExtras struct {
	refs      map[string]*RecipeVersionRef
	lastLogs  map[string]*LogEntry
	reminders map[string]*Reminder
	counts    map[string]int
}

func loadBatchExtras(ctx context.Context, q *sqlc.Queries, userID string, rows []sqlc.Batch) (batchExtras, error) {
	x := batchExtras{lastLogs: map[string]*LogEntry{}, reminders: map[string]*Reminder{}, counts: map[string]int{}}
	ids := make([]string, len(rows))
	var versionIDs []string
	for i, b := range rows {
		ids[i] = b.ID
		if b.RecipeVersionID != nil {
			versionIDs = append(versionIDs, *b.RecipeVersionID)
		}
	}
	var err error
	if x.refs, err = versionRefs(ctx, q, userID, versionIDs); err != nil {
		return x, err
	}
	logRows, err := q.LastLogsByBatchIDs(ctx, sqlc.LastLogsByBatchIDsParams{UserID: userID, BatchIds: ids})
	if err != nil {
		return x, err
	}
	logs, err := loadLogs(ctx, q, userID, logRows)
	if err != nil {
		return x, err
	}
	for i := range logs {
		x.lastLogs[logs[i].BatchID] = &logs[i]
	}
	rems, err := q.NextRemindersByBatchIDs(ctx, sqlc.NextRemindersByBatchIDsParams{UserID: userID, BatchIds: ids})
	if err != nil {
		return x, err
	}
	for _, r := range rems {
		rr := toReminder(r)
		x.reminders[r.BatchID] = &rr
	}
	counts, err := q.CountBottlesByBatch(ctx, sqlc.CountBottlesByBatchParams{UserID: userID, BatchIds: ids})
	if err != nil {
		return x, err
	}
	for _, c := range counts {
		x.counts[c.BatchID] = int(c.N)
	}
	return x, nil
}

func core(b sqlc.Batch, x batchExtras) (batchCore, error) {
	rating, err := ratingOf(b.Rating)
	if err != nil {
		return batchCore{}, err
	}
	c := batchCore{
		ID: b.ID, Code: b.Code, Name: b.Name, FermentationTypeID: b.FermentationTypeID, Status: b.Status, Stage: b.Stage,
		ArchivedAt: utcPtr(b.ArchivedAt), StartedAt: b.StartedAt.UTC(), FinishedAt: utcPtr(b.FinishedAt), VolumeMl: int(b.VolumeMl),
		Vessel: b.Vessel, ParentBatchID: b.ParentBatchID, Hypothesis: b.Hypothesis, ChangedFields: b.ChangedFields,
		IsReference: b.IsReference, Rating: rating, LastLog: x.lastLogs[b.ID], NextReminder: x.reminders[b.ID],
	}
	if b.RecipeVersionID != nil {
		c.RecipeVersion = x.refs[*b.RecipeVersionID]
	}
	return c, nil
}

func (s *Service) summaries(ctx context.Context, q *sqlc.Queries, userID string, rows []sqlc.Batch) ([]BatchSummary, error) {
	x, err := loadBatchExtras(ctx, q, userID, rows)
	if err != nil {
		return nil, err
	}
	out := make([]BatchSummary, len(rows))
	for i, b := range rows {
		c, err := core(b, x)
		if err != nil {
			return nil, err
		}
		out[i] = BatchSummary{batchCore: c, BottleCount: x.counts[b.ID]}
	}
	return out, nil
}

// fullBatches loads batches with ingredients, params and bottles.
func (s *Service) fullBatches(ctx context.Context, q *sqlc.Queries, userID string, rows []sqlc.Batch) ([]Batch, error) {
	x, err := loadBatchExtras(ctx, q, userID, rows)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(rows))
	for i, b := range rows {
		ids[i] = b.ID
	}
	ings, err := q.ListBatchIngredients(ctx, sqlc.ListBatchIngredientsParams{UserID: userID, BatchIds: ids})
	if err != nil {
		return nil, err
	}
	ingBy := map[string][]domain.Ingredient{}
	for _, in := range ings {
		ingBy[in.BatchID] = append(ingBy[in.BatchID], domain.Ingredient{ID: in.ID, IngredientInput: domain.IngredientInput{Name: in.Name, Role: in.Role, Amount: in.Amount, Unit: in.Unit}})
	}
	bottles, err := s.bottlesOf(ctx, q, userID, ids)
	if err != nil {
		return nil, err
	}
	out := make([]Batch, len(rows))
	for i, b := range rows {
		c, err := core(b, x)
		if err != nil {
			return nil, err
		}
		var p domain.BatchParams
		if err := unmarshalInto(b.Params, &p); err != nil {
			return nil, err
		}
		list := ingBy[b.ID]
		if list == nil {
			list = []domain.Ingredient{}
		}
		bl := bottles[b.ID]
		if bl == nil {
			bl = []Bottle{}
		}
		out[i] = Batch{batchCore: c, Ingredients: list, Params: p, Bottles: bl}
	}
	return out, nil
}

func (s *Service) batch(ctx context.Context, q *sqlc.Queries, userID, id string) (Batch, error) {
	row, err := q.GetBatch(ctx, sqlc.GetBatchParams{ID: id, UserID: userID})
	if err != nil {
		return Batch{}, notFoundIf(err, "Batch")
	}
	list, err := s.fullBatches(ctx, q, userID, []sqlc.Batch{row})
	if err != nil {
		return Batch{}, err
	}
	return list[0], nil
}

// bottlesOf returns the bottles of the given batches, with their latest readings.
func (s *Service) bottlesOf(ctx context.Context, q *sqlc.Queries, userID string, batchIDs []string) (map[string][]Bottle, error) {
	rows, err := q.ListBottlesByBatchIDs(ctx, sqlc.ListBottlesByBatchIDsParams{UserID: userID, BatchIds: batchIDs})
	if err != nil {
		return nil, err
	}
	out := map[string][]Bottle{}
	if len(rows) == 0 {
		return out, nil
	}
	readings, err := q.ListReadingsByBatchIDs(ctx, sqlc.ListReadingsByBatchIDsParams{UserID: userID, BatchIds: batchIDs})
	if err != nil {
		return nil, err
	}
	conv, err := s.toBottles(ctx, q, userID, rows, readings)
	if err != nil {
		return nil, err
	}
	for _, b := range conv {
		out[b.BatchID] = append(out[b.BatchID], b)
	}
	return out, nil
}

func (s *Service) toBottles(ctx context.Context, q *sqlc.Queries, userID string, rows []sqlc.Bottle, readings []sqlc.ListReadingsByBatchIDsRow) ([]Bottle, error) {
	var versionIDs []string
	for _, b := range rows {
		if b.RecipeVersionID != nil {
			versionIDs = append(versionIDs, *b.RecipeVersionID)
		}
	}
	refs, err := versionRefs(ctx, q, userID, versionIDs)
	if err != nil {
		return nil, err
	}
	// Latest value per definition key, per bottle (readings are oldest first).
	last := map[string]map[string]domain.Measurement{}
	order := map[string][]string{}
	for _, r := range readings {
		if r.BottleID == nil {
			continue
		}
		m := last[*r.BottleID]
		if m == nil {
			m = map[string]domain.Measurement{}
			last[*r.BottleID] = m
		}
		if _, seen := m[r.DefinitionKey]; !seen {
			order[*r.BottleID] = append(order[*r.BottleID], r.DefinitionKey)
		}
		m[r.DefinitionKey] = domain.Measurement{DefinitionKey: r.DefinitionKey, Value: domain.Value{Num: r.ValueNum, Bool: r.ValueBool, Text: r.ValueText}, Unit: r.Unit, Method: r.Method}
	}
	out := make([]Bottle, len(rows))
	for i, b := range rows {
		rating, err := ratingOf(b.Rating)
		if err != nil {
			return nil, err
		}
		var adds []domain.Addition
		if err := unmarshalInto(b.Additions, &adds); err != nil {
			return nil, err
		}
		if adds == nil {
			adds = []domain.Addition{}
		}
		lm := []domain.Measurement{}
		for _, k := range order[b.ID] {
			lm = append(lm, last[b.ID][k])
		}
		bo := Bottle{
			ID: b.ID, BatchID: b.BatchID, Label: b.Label, FlavorName: b.FlavorName, VolumeMl: int(b.VolumeMl), Additions: adds,
			PrimingSugarG: b.PrimingSugarG, HeadspaceCm: b.HeadspaceCm, Status: b.Status, StartedAt: b.StartedAt.UTC(),
			FinishedAt: utcPtr(b.FinishedAt), LastMeasurements: lm, Rating: rating, ParentBottleID: b.ParentBottleID,
			Hypothesis: b.Hypothesis, ChangedFields: b.ChangedFields, IsReference: b.IsReference,
		}
		if b.RecipeVersionID != nil {
			bo.RecipeVersion = refs[*b.RecipeVersionID]
		}
		out[i] = bo
	}
	return out, nil
}

func (s *Service) bottle(ctx context.Context, q *sqlc.Queries, userID string, row sqlc.Bottle) (Bottle, error) {
	readings, err := q.ListReadingsByBatchIDs(ctx, sqlc.ListReadingsByBatchIDsParams{UserID: userID, BatchIds: []string{row.BatchID}})
	if err != nil {
		return Bottle{}, err
	}
	list, err := s.toBottles(ctx, q, userID, []sqlc.Bottle{row}, readings)
	if err != nil {
		return Bottle{}, err
	}
	return list[0], nil
}

// ---------------------------------------------------------------- measurement definitions

type defSet struct {
	byKey map[string]sqlc.MeasurementDefinition
	list  []domain.MeasurementDef
}

func loadDefs(ctx context.Context, q *sqlc.Queries, userID string) (defSet, error) {
	rows, err := q.ListAllMeasurementDefinitions(ctx, &userID)
	if err != nil {
		return defSet{}, err
	}
	d := defSet{byKey: map[string]sqlc.MeasurementDefinition{}}
	for _, r := range rows {
		// A user's own definition takes precedence over a system one with the same key.
		if prev, ok := d.byKey[r.Key]; ok && prev.UserID != nil {
			continue
		}
		d.byKey[r.Key] = r
	}
	for _, r := range rows {
		if d.byKey[r.Key].ID != r.ID {
			continue
		}
		d.list = append(d.list, toDomainDef(r))
	}
	return d, nil
}

func toDomainDef(r sqlc.MeasurementDefinition) domain.MeasurementDef {
	q := ""
	if r.Quantity != nil {
		q = *r.Quantity
	}
	return domain.MeasurementDef{Key: r.Key, Label: r.Label, Kind: r.Kind, Quantity: q, Unit: r.Unit, Decimals: int(r.Decimals), Min: r.MinValue, Max: r.MaxValue, RequiresMethod: r.RequiresMethod}
}

// series groups readings into first/last per definition, separately for the
// batch itself (bottle_id null) and for each bottle.
type seriesSet struct {
	batch   map[string]map[string]domain.Series // batch id -> key -> series
	bottles map[string]map[string]domain.Series // bottle id -> key -> series
}

func readingOfRow(r sqlc.ListReadingsByBatchIDsRow) *domain.Reading {
	if r.ValueNum != nil {
		return &domain.Reading{Num: r.ValueCanonical}
	}
	return &domain.Reading{Bool: r.ValueBool, Text: r.ValueText}
}

func buildSeries(readings []sqlc.ListReadingsByBatchIDsRow) seriesSet {
	s := seriesSet{batch: map[string]map[string]domain.Series{}, bottles: map[string]map[string]domain.Series{}}
	add := func(m map[string]map[string]domain.Series, id, key string, rd *domain.Reading) {
		if m[id] == nil {
			m[id] = map[string]domain.Series{}
		}
		cur := m[id][key]
		if cur.First == nil {
			cur.First = rd
		}
		cur.Last = rd
		m[id][key] = cur
	}
	for _, r := range readings {
		if r.BottleID == nil {
			add(s.batch, r.BatchID, r.DefinitionKey, readingOfRow(r))
		} else {
			add(s.bottles, *r.BottleID, r.DefinitionKey, readingOfRow(r))
		}
	}
	return s
}

func daysBetween(from time.Time, to *time.Time) *float64 {
	if to == nil {
		return nil
	}
	d := to.Sub(from).Hours() / 24
	return &d
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
