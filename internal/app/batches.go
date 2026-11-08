package app

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"
	"time"

	"jarlore/internal/apperr"
	"jarlore/internal/auth"
	"jarlore/internal/db"
	"jarlore/internal/db/sqlc"
	"jarlore/internal/domain"
)

// ---------------------------------------------------------------- reads

type BatchQuery struct {
	Status, Stage, Type, RecipeID, Q *string
	Archived                         bool
	Sort                             string
	Cursor                           string
	Limit                            int
}

func (s *Service) ListBatches(ctx context.Context, u auth.User, in BatchQuery) (Page[BatchSummary], error) {
	var page Page[BatchSummary]
	sortKey := in.Sort
	if sortKey == "" {
		sortKey = "started_desc"
	}
	scope := strings.Join([]string{"batches", sortKey, deref(in.Status), deref(in.Stage), deref(in.Type), deref(in.RecipeID), deref(in.Q), fmt.Sprint(in.Archived)}, "|")
	cur, err := s.decodeCursor(in.Cursor, scope)
	if err != nil {
		return page, err
	}
	lim := pageLimit(in.Limit)
	var afterTime *time.Time
	var afterID *string
	var afterScore *int32
	if cur != nil {
		afterTime, afterID, afterScore = cur.Time, &cur.ID, cur.Num
	}
	err = s.read(ctx, u, func(q *sqlc.Queries) error {
		var rows []sqlc.Batch
		var err error
		switch sortKey {
		case "started_desc":
			rows, err = q.ListBatchesStartedDesc(ctx, sqlc.ListBatchesStartedDescParams{UserID: u.ID, Status: in.Status, Stage: in.Stage, Type: in.Type, RecipeID: in.RecipeID, Q: in.Q, Archived: in.Archived, AfterTime: afterTime, AfterID: afterID, Lim: lim + 1})
		case "started_asc":
			rows, err = q.ListBatchesStartedAsc(ctx, sqlc.ListBatchesStartedAscParams{UserID: u.ID, Status: in.Status, Stage: in.Stage, Type: in.Type, RecipeID: in.RecipeID, Q: in.Q, Archived: in.Archived, AfterTime: afterTime, AfterID: afterID, Lim: lim + 1})
		case "updated_desc":
			rows, err = q.ListBatchesUpdatedDesc(ctx, sqlc.ListBatchesUpdatedDescParams{UserID: u.ID, Status: in.Status, Stage: in.Stage, Type: in.Type, RecipeID: in.RecipeID, Q: in.Q, Archived: in.Archived, AfterTime: afterTime, AfterID: afterID, Lim: lim + 1})
		case "rating_desc":
			rows, err = q.ListBatchesRatingDesc(ctx, sqlc.ListBatchesRatingDescParams{UserID: u.ID, Status: in.Status, Stage: in.Stage, Type: in.Type, RecipeID: in.RecipeID, Q: in.Q, Archived: in.Archived, AfterScore: afterScore, AfterID: afterID, Lim: lim + 1})
		default:
			return apperr.BadRequest("sort must be started_desc, started_asc, rating_desc or updated_desc.")
		}
		if err != nil {
			return err
		}
		if len(rows) > int(lim) {
			rows = rows[:lim]
			last := rows[len(rows)-1]
			c := cursor{Scope: scope, ID: last.ID}
			switch sortKey {
			case "updated_desc":
				c.Time = ptr(last.UpdatedAt)
			case "rating_desc":
				score := int32(0)
				if r, _ := ratingOf(last.Rating); r != nil {
					score = int32(r.Overall)
				}
				c.Num = &score
			default:
				c.Time = ptr(last.StartedAt)
			}
			page.NextCursor = s.encodeCursor(c)
		}
		page.Data, err = s.summaries(ctx, q, u.ID, rows)
		return err
	})
	return page, err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (s *Service) GetBatch(ctx context.Context, u auth.User, id string) (Batch, error) {
	var b Batch
	err := s.read(ctx, u, func(q *sqlc.Queries) (err error) {
		b, err = s.batch(ctx, q, u.ID, id)
		return err
	})
	return b, err
}

// ---------------------------------------------------------------- create (blank, from a version, Brew Again)

func (s *Service) CreateBatch(ctx context.Context, u auth.User, idemKey string, in CreateBatchInput) (Result, error) {
	now := s.now()
	if err := in.validate(now); err != nil {
		return Result{}, err
	}
	return s.write(ctx, u, "createBatch", idemKey, in, http.StatusCreated, func(q *sqlc.Queries) (any, error) {
		id, err := s.createBatch(ctx, q, u.ID, in, now)
		if err != nil {
			return nil, err
		}
		return s.batch(ctx, q, u.ID, id)
	})
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// draftSubject is what a create request would brew.
func draftSubject(in CreateBatchInput) domain.Subject {
	v := float64(in.VolumeMl)
	return domain.Subject{VolumeMl: &v, Ingredients: in.Ingredients, Params: in.Params, Vessel: trimPtr(in.Vessel)}
}

// versionBase loads a version as a diff subject (recipe-level fields only).
func versionSubject(ctx context.Context, q *sqlc.Queries, userID string, v sqlc.RecipeVersion) (domain.Subject, []domain.Ingredient, domain.BatchParams, error) {
	ings, err := q.ListRecipeIngredients(ctx, sqlc.ListRecipeIngredientsParams{UserID: userID, VersionIds: []string{v.ID}})
	if err != nil {
		return domain.Subject{}, nil, domain.BatchParams{}, err
	}
	var p domain.BatchParams
	if err := unmarshalInto(v.Params, &p); err != nil {
		return domain.Subject{}, nil, p, err
	}
	list := make([]domain.Ingredient, len(ings))
	inputs := make([]domain.IngredientInput, len(ings))
	for i, in := range ings {
		inputs[i] = domain.IngredientInput{Name: in.Name, Role: in.Role, Amount: in.Amount, Unit: in.Unit}
		list[i] = domain.Ingredient{ID: in.ID, IngredientInput: inputs[i]}
	}
	vol := float64(v.BaseVolumeMl)
	sub := domain.Subject{VolumeMl: &vol, Ingredients: inputs, Params: p.RecipeParams()}
	if len(v.BottleSpec) > 0 {
		var spec domain.BottleSpec
		if err := unmarshalInto(v.BottleSpec, &spec); err != nil {
			return sub, nil, p, err
		}
		sub.Bottle = &spec
	}
	return sub, list, p, nil
}

// batchSubject turns a stored batch into a diff subject.
func (s *Service) batchSubjects(ctx context.Context, q *sqlc.Queries, userID string, rows []sqlc.Batch) (map[string]domain.Subject, error) {
	full, err := s.fullBatches(ctx, q, userID, rows)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	readings, err := q.ListReadingsByBatchIDs(ctx, sqlc.ListReadingsByBatchIDsParams{UserID: userID, BatchIds: ids})
	if err != nil {
		return nil, err
	}
	series := buildSeries(readings)
	out := map[string]domain.Subject{}
	for i, b := range full {
		row := rows[i]
		vol := float64(b.VolumeMl)
		ins := make([]domain.IngredientInput, len(b.Ingredients))
		for j, in := range b.Ingredients {
			ins[j] = in.IngredientInput
		}
		sub := domain.Subject{
			VolumeMl: &vol, Ingredients: ins, Params: b.Params, Vessel: b.Vessel, Rating: b.Rating,
			Measurements: series.batch[b.ID],
		}
		if b.RecipeVersion != nil {
			sub.RecipeLabel = ptr(fmt.Sprintf("%s v%d", b.RecipeVersion.RecipeName, b.RecipeVersion.VersionNo))
		}
		f1End := row.BottledAt
		if f1End == nil {
			f1End = row.FinishedAt
		}
		sub.F1Days = daysBetween(row.StartedAt, f1End)
		if row.BottledAt != nil {
			sub.F2Days = daysBetween(*row.BottledAt, row.FinishedAt)
		}
		if n := len(b.Bottles); n > 0 {
			sub.BottleCount = &n
		}
		out[b.ID] = sub
	}
	return out, nil
}

// resolvedVersion is the recipe version a new batch or bottle relates to.
type resolvedVersion struct {
	row    sqlc.RecipeVersion
	recipe sqlc.Recipe
	sub    domain.Subject
}

func loadVersion(ctx context.Context, q *sqlc.Queries, userID, id string) (*resolvedVersion, error) {
	v, err := q.GetRecipeVersion(ctx, sqlc.GetRecipeVersionParams{ID: id, UserID: userID})
	if err != nil {
		return nil, notFoundIf(err, "Recipe version")
	}
	r, err := q.GetRecipe(ctx, sqlc.GetRecipeParams{ID: v.RecipeID, UserID: userID})
	if err != nil {
		return nil, err
	}
	sub, _, _, err := versionSubject(ctx, q, userID, v)
	if err != nil {
		return nil, err
	}
	return &resolvedVersion{row: v, recipe: r, sub: sub}, nil
}

// snapshot is the frozen copy of a version stored on a batch or bottle.
func snapshotJSON(ctx context.Context, q *sqlc.Queries, userID string, v sqlc.RecipeVersion, recipeName string, at time.Time) ([]byte, error) {
	_, ings, params, err := versionSubject(ctx, q, userID, v)
	if err != nil {
		return nil, err
	}
	snap := map[string]any{
		"schemaVersion":  1,
		"capturedAt":     at,
		"recipe":         map[string]any{"id": v.RecipeID, "name": recipeName, "scope": v.Scope},
		"version":        map[string]any{"id": v.ID, "versionNo": v.VersionNo, "baseVolumeMl": v.BaseVolumeMl},
		"ingredients":    ings,
		"params":         params.RecipeParams(),
		"defaultBottles": jsonRaw(v.DefaultBottles, "[]"),
		"bottle":         jsonRaw(v.BottleSpec, "null"),
	}
	return mustJSON(snap), nil
}

type rawJSON []byte

func (r rawJSON) MarshalJSON() ([]byte, error) { return r, nil }

func jsonRaw(b []byte, def string) rawJSON {
	if len(b) == 0 {
		return rawJSON(def)
	}
	return rawJSON(b)
}

// newVersionArgs describes a version created by the version rule or Save as Recipe.
type newVersionArgs struct {
	recipe         sqlc.Recipe
	parentID       *string
	changeNote     *string
	sourceBatchID  *string
	sourceBottleID *string
	baseVolumeMl   int
	ingredients    []domain.IngredientInput
	params         domain.BatchParams
	defaultBottles []domain.DefaultBottle
	bottle         *domain.BottleSpec
	changedFields  []string
}

// createVersion allocates the next version number under the recipe's row lock and
// inserts the immutable version. Concurrent saves get distinct numbers.
func createVersion(ctx context.Context, q *sqlc.Queries, userID string, a newVersionArgs) (sqlc.RecipeVersion, error) {
	no, err := q.AllocateVersionNo(ctx, sqlc.AllocateVersionNoParams{ID: a.recipe.ID, UserID: userID})
	if err != nil {
		return sqlc.RecipeVersion{}, notFoundIf(err, "Recipe")
	}
	if a.defaultBottles == nil {
		a.defaultBottles = []domain.DefaultBottle{}
	}
	if a.changedFields == nil {
		a.changedFields = []string{}
	}
	var spec []byte
	if a.bottle != nil {
		spec = mustJSON(a.bottle)
	}
	v, err := q.InsertRecipeVersion(ctx, sqlc.InsertRecipeVersionParams{
		ID: newID(), UserID: userID, RecipeID: a.recipe.ID, Scope: a.recipe.Scope, FermentationTypeID: a.recipe.FermentationTypeID,
		VersionNo: no, ParentVersionID: a.parentID, ChangeNote: trimPtr(a.changeNote), SourceBatchID: a.sourceBatchID,
		SourceBottleID: a.sourceBottleID, BaseVolumeMl: int32(a.baseVolumeMl), Params: mustJSON(a.params.RecipeParams()),
		DefaultBottles: mustJSON(a.defaultBottles), BottleSpec: spec, ChangedFields: a.changedFields,
	})
	if err != nil {
		return v, err
	}
	if len(a.ingredients) > 0 {
		args := make([]sqlc.InsertRecipeIngredientsParams, len(a.ingredients))
		for i, in := range a.ingredients {
			args[i] = sqlc.InsertRecipeIngredientsParams{UserID: userID, RecipeVersionID: v.ID, Position: int16(i), Name: strings.TrimSpace(in.Name), Role: in.Role, Amount: in.Amount, Unit: strings.TrimSpace(in.Unit)}
		}
		if err := execBatch(q.InsertRecipeIngredients(ctx, args)); err != nil {
			return v, err
		}
	}
	return v, nil
}

type batchExecResults interface {
	Exec(func(int, error))
	Close() error
}

func execBatch(br batchExecResults) error {
	var first error
	br.Exec(func(_ int, err error) {
		if err != nil && first == nil {
			first = err
		}
	})
	if err := br.Close(); err != nil && first == nil {
		first = err
	}
	return first
}

func (s *Service) createBatch(ctx context.Context, q *sqlc.Queries, userID string, in CreateBatchInput, now time.Time) (string, error) {
	ft, err := q.GetFermentationType(ctx, in.FermentationTypeID)
	if err != nil {
		return "", notFoundIf(err, "Fermentation type")
	}
	profile, err := q.GetProfile(ctx, userID)
	if err != nil {
		return "", err
	}
	id := newID()
	draft := draftSubject(in)

	var version *resolvedVersion
	var parent *sqlc.Batch
	if in.Source != nil {
		switch in.Source.Type {
		case "recipe_version":
			if version, err = loadVersion(ctx, q, userID, in.Source.ID); err != nil {
				return "", err
			}
		case "batch":
			p, err := q.GetBatch(ctx, sqlc.GetBatchParams{ID: in.Source.ID, UserID: userID})
			if err != nil {
				return "", notFoundIf(err, "Source batch")
			}
			parent = &p
			if p.RecipeVersionID != nil {
				if version, err = loadVersion(ctx, q, userID, *p.RecipeVersionID); err != nil {
					return "", err
				}
			}
		}
	}
	if version != nil && version.row.Scope != "batch" {
		return "", apperr.Validation(apperr.FieldError{Field: "source", Code: "wrong_scope", Message: "That is a bottle recipe. Use it when bottling."})
	}
	if version != nil && version.row.FermentationTypeID != in.FermentationTypeID {
		return "", apperr.Validation(apperr.FieldError{Field: "fermentationTypeId", Code: "type_mismatch", Message: "A batch and its recipe must be the same fermentation type."})
	}
	if parent != nil && parent.FermentationTypeID != in.FermentationTypeID {
		return "", apperr.Validation(apperr.FieldError{Field: "fermentationTypeId", Code: "type_mismatch", Message: "Brew again keeps the fermentation type of the source batch."})
	}

	// The version rule (DOMAIN_MODEL.md §8.3): recipe-level changes create version n+1,
	// pinned to this batch; batch conditions never do.
	var versionID *string
	var snapshot []byte
	if version != nil {
		pinned := version.row
		if changes := domain.RecipeChanges(version.sub, draft); len(changes) > 0 {
			var defaults []domain.DefaultBottle
			if err := unmarshalInto(version.row.DefaultBottles, &defaults); err != nil {
				return "", err
			}
			pinned, err = createVersion(ctx, q, userID, newVersionArgs{
				recipe: version.recipe, parentID: &version.row.ID, changeNote: in.Hypothesis, sourceBatchID: &id,
				baseVolumeMl: in.VolumeMl, ingredients: in.Ingredients, params: in.Params, defaultBottles: defaults, changedFields: changes,
			})
			if err != nil {
				return "", err
			}
		}
		versionID = &pinned.ID
		if snapshot, err = snapshotJSON(ctx, q, userID, pinned, version.recipe.Name, now); err != nil {
			return "", err
		}
	}

	// What changed versus the source: the parent batch, or the version brewed from.
	changed := []string{}
	if parent != nil {
		subs, err := s.batchSubjects(ctx, q, userID, []sqlc.Batch{*parent})
		if err != nil {
			return "", err
		}
		changed = changedKeys(domain.Diff([]domain.Subject{subs[parent.ID], draft}, domain.Options{Sections: domain.ConfigSections}))
	} else if version != nil {
		changed = changedKeys(domain.Diff([]domain.Subject{version.sub, draft}, domain.Options{Sections: domain.RecipeSections}))
	}

	no, err := q.NextBatchCodeNo(ctx, sqlc.NextBatchCodeNoParams{UserID: userID, Prefix: ft.CodePrefix})
	if err != nil {
		return "", err
	}
	code := fmt.Sprintf("%s-%03d", ft.CodePrefix, no)
	name := code
	if n := trimPtr(in.Name); n != nil {
		name = *n
	} else if version != nil {
		name = version.recipe.Name
	} else if parent != nil {
		name = parent.Name
	}
	started := now
	if in.StartedAt != nil {
		started = in.StartedAt.UTC()
	}
	var parentID *string
	if parent != nil {
		parentID = &parent.ID
	}
	if _, err := q.InsertBatch(ctx, sqlc.InsertBatchParams{
		ID: id, UserID: userID, Code: code, Name: name, FermentationTypeID: in.FermentationTypeID, StartedAt: started,
		VolumeMl: int32(in.VolumeMl), Vessel: trimPtr(in.Vessel), Params: mustJSON(in.Params), RecipeVersionID: versionID,
		RecipeSnapshot: snapshot, ParentBatchID: parentID, Hypothesis: trimPtr(in.Hypothesis), ChangedFields: changed,
	}); err != nil {
		return "", err
	}
	if len(in.Ingredients) > 0 {
		args := make([]sqlc.InsertBatchIngredientsParams, len(in.Ingredients))
		for i, ing := range in.Ingredients {
			args[i] = sqlc.InsertBatchIngredientsParams{UserID: userID, BatchID: id, Position: int16(i), Name: strings.TrimSpace(ing.Name), Role: ing.Role, Amount: ing.Amount, Unit: strings.TrimSpace(ing.Unit)}
		}
		if err := execBatch(q.InsertBatchIngredients(ctx, args)); err != nil {
			return "", err
		}
	}

	// Taste check at the target F1 length (or the type's typical minimum), 09:00 local.
	stages, err := parseStages(ft.Stages)
	if err != nil {
		return "", err
	}
	days := 0.0
	if in.Params.TargetF1Days != nil {
		days = *in.Params.TargetF1Days
	} else if len(stages) > 0 {
		days = float64(stages[0].TypicalDays[0])
	}
	if days > 0 {
		due := localMorning(started.Add(time.Duration(days*24)*time.Hour), profile.Timezone)
		if due.After(now) {
			if _, err := q.InsertReminder(ctx, sqlc.InsertReminderParams{UserID: userID, BatchID: id, Kind: "taste_check", DueAt: due, Label: "Taste check " + code}); err != nil {
				return "", err
			}
		}
	}
	return id, nil
}

func changedKeys(rows []domain.Row) []string {
	out := []string{}
	for _, r := range rows {
		if r.Differs {
			out = append(out, r.Key)
		}
	}
	return out
}

// localMorning returns 09:00 on t's calendar day in the user's time zone.
func localMorning(t time.Time, tz string) time.Time {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 9, 0, 0, 0, loc).UTC()
}

// endOfLocalDay is midnight after t in the user's time zone.
func endOfLocalDay(t time.Time, tz string) time.Time {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day()+1, 0, 0, 0, 0, loc).UTC()
}

// ---------------------------------------------------------------- update and lifecycle

func (s *Service) UpdateBatch(ctx context.Context, u auth.User, id string, in UpdateBatchInput) (Result, error) {
	var f apperr.Fields
	textLen(&f, "name", in.Name, 1, 120)
	if in.Vessel.Set {
		textLen(&f, "vessel", in.Vessel.Value, 0, 80)
	}
	if in.Params != nil {
		numRange(&f, "params.roomTempC", in.Params.RoomTempC.Value, -10, 60)
	}
	if in.Name == nil && !in.Vessel.Set && in.IsReference == nil && in.Params == nil {
		f.Add("body", "empty", "Send at least one field to change.")
	}
	if err := f.Err(); err != nil {
		return Result{}, err
	}
	return s.write(ctx, u, "updateBatch", "", nil, http.StatusOK, func(q *sqlc.Queries) (any, error) {
		cur, err := q.GetBatchForUpdate(ctx, sqlc.GetBatchForUpdateParams{ID: id, UserID: u.ID})
		if err != nil {
			return nil, notFoundIf(err, "Batch")
		}
		if in.IsReference != nil && *in.IsReference && cur.Status != "finished" {
			return nil, apperr.InvalidState("Only a finished batch can be your reference.")
		}
		p := sqlc.UpdateBatchDetailsParams{ID: id, UserID: u.ID, Name: trimPtr(in.Name), SetVessel: in.Vessel.Set, Vessel: trimPtr(in.Vessel.Value), IsReference: in.IsReference}
		if in.Params != nil && in.Params.RoomTempC.Set {
			p.SetRoomTemp, p.RoomTempC = true, in.Params.RoomTempC.Value
		}
		if _, err := q.UpdateBatchDetails(ctx, p); err != nil {
			return nil, err
		}
		return s.batch(ctx, q, u.ID, id)
	})
}

func (s *Service) FinishBatch(ctx context.Context, u auth.User, id, idemKey string, in FinishInput) (Result, error) {
	var f apperr.Fields
	validRating(&f, in.Rating)
	now := s.now()
	notFuture(&f, "finishedAt", in.FinishedAt, now)
	if err := f.Err(); err != nil {
		return Result{}, err
	}
	return s.write(ctx, u, "finishBatch:"+id, idemKey, in, http.StatusOK, func(q *sqlc.Queries) (any, error) {
		cur, err := q.GetBatchForUpdate(ctx, sqlc.GetBatchForUpdateParams{ID: id, UserID: u.ID})
		if err != nil {
			return nil, notFoundIf(err, "Batch")
		}
		if cur.Status != "active" {
			return nil, apperr.InvalidState("This batch is already " + cur.Status + ".")
		}
		at := now
		if in.FinishedAt != nil {
			at = in.FinishedAt.UTC()
		}
		if at.Before(cur.StartedAt) || (cur.BottledAt != nil && at.Before(*cur.BottledAt)) {
			return nil, apperr.Validation(apperr.FieldError{Field: "finishedAt", Code: "before_start", Message: "A batch can't finish before it started or was bottled."})
		}
		// Bottles close with their batch, unrated; they can still be rated afterwards.
		if err := q.CloseOpenBottles(ctx, sqlc.CloseOpenBottlesParams{Status: "finished", FinishedAt: &at, BatchID: id, UserID: u.ID}); err != nil {
			return nil, err
		}
		if _, err := q.FinishBatch(ctx, sqlc.FinishBatchParams{ID: id, UserID: u.ID, FinishedAt: &at, Rating: mustJSON(in.Rating)}); err != nil {
			return nil, err
		}
		if err := q.DismissOpenReminders(ctx, sqlc.DismissOpenRemindersParams{UserID: u.ID, BatchID: id, Kinds: []string{}}); err != nil {
			return nil, err
		}
		if _, err := q.InsertLog(ctx, sqlc.InsertLogParams{UserID: u.ID, BatchID: id, Kind: "stage_change", OccurredAt: at, Note: ptr("Finished")}); err != nil {
			return nil, err
		}
		return s.batch(ctx, q, u.ID, id)
	})
}

func (s *Service) DiscardBatch(ctx context.Context, u auth.User, id string, in DiscardInput) (Result, error) {
	var f apperr.Fields
	textLen(&f, "reason", &in.Reason, 1, 500)
	if err := f.Err(); err != nil {
		return Result{}, err
	}
	now := s.now()
	return s.write(ctx, u, "discardBatch", "", nil, http.StatusOK, func(q *sqlc.Queries) (any, error) {
		cur, err := q.GetBatchForUpdate(ctx, sqlc.GetBatchForUpdateParams{ID: id, UserID: u.ID})
		if err != nil {
			return nil, notFoundIf(err, "Batch")
		}
		if cur.Status != "active" {
			return nil, apperr.InvalidState("This batch is already " + cur.Status + ".")
		}
		if err := q.CloseOpenBottles(ctx, sqlc.CloseOpenBottlesParams{Status: "discarded", FinishedAt: &now, BatchID: id, UserID: u.ID}); err != nil {
			return nil, err
		}
		if _, err := q.DiscardBatch(ctx, sqlc.DiscardBatchParams{ID: id, UserID: u.ID, FinishedAt: &now, Reason: ptr(strings.TrimSpace(in.Reason))}); err != nil {
			return nil, err
		}
		if err := q.DismissOpenReminders(ctx, sqlc.DismissOpenRemindersParams{UserID: u.ID, BatchID: id, Kinds: []string{}}); err != nil {
			return nil, err
		}
		if _, err := q.InsertLog(ctx, sqlc.InsertLogParams{UserID: u.ID, BatchID: id, Kind: "stage_change", OccurredAt: now, Note: ptr("Discarded: " + strings.TrimSpace(in.Reason))}); err != nil {
			return nil, err
		}
		return s.batch(ctx, q, u.ID, id)
	})
}

func (s *Service) SetBatchArchived(ctx context.Context, u auth.User, id string, archived bool) (Result, error) {
	now := s.now()
	return s.write(ctx, u, "archiveBatch", "", nil, http.StatusOK, func(q *sqlc.Queries) (any, error) {
		cur, err := q.GetBatchForUpdate(ctx, sqlc.GetBatchForUpdateParams{ID: id, UserID: u.ID})
		if err != nil {
			return nil, notFoundIf(err, "Batch")
		}
		if cur.Status == "active" {
			return nil, apperr.InvalidState("Finish or discard a batch before archiving it.")
		}
		var at *time.Time
		if archived {
			at = &now
		}
		if _, err := q.SetBatchArchived(ctx, sqlc.SetBatchArchivedParams{ID: id, UserID: u.ID, ArchivedAt: at}); err != nil {
			return nil, err
		}
		return s.batch(ctx, q, u.ID, id)
	})
}

// ---------------------------------------------------------------- brew again and previews

func (s *Service) BrewAgainDraft(ctx context.Context, u auth.User, id string) (BrewAgainDraft, error) {
	var out BrewAgainDraft
	err := s.read(ctx, u, func(q *sqlc.Queries) error {
		b, err := s.batch(ctx, q, u.ID, id)
		if err != nil {
			return err
		}
		ins := make([]domain.IngredientInput, len(b.Ingredients))
		for i, in := range b.Ingredients {
			ins[i] = in.IngredientInput
		}
		name := b.Name
		out = BrewAgainDraft{
			Draft: CreateBatchInput{
				FermentationTypeID: b.FermentationTypeID, Name: &name, VolumeMl: b.VolumeMl, Vessel: b.Vessel,
				Ingredients: ins, Params: b.Params, Source: &SourceRef{Type: "batch", ID: b.ID},
			},
			Base:          BrewAgainBase{Ref: ItemRef{Type: "batch", ID: b.ID}, Title: b.Code + " " + b.Name},
			RecipeVersion: b.RecipeVersion,
		}
		return nil
	})
	return out, err
}

// PreviewBatch diffs a draft batch against a batch, a bottle's batch or a recipe
// version, and says whether starting it would create a new recipe version.
func (s *Service) PreviewBatch(ctx context.Context, u auth.User, in BatchPreviewInput) (DiffResult, error) {
	var out DiffResult
	var f apperr.Fields
	validSource(&f, "base", &in.Base, "batch", "bottle", "recipe_version")
	if err := f.Err(); err != nil {
		return out, err
	}
	if err := in.Draft.validate(s.now()); err != nil {
		return out, err
	}
	err := s.read(ctx, u, func(q *sqlc.Queries) error {
		profile, err := q.GetProfile(ctx, u.ID)
		if err != nil {
			return err
		}
		draft := draftSubject(in.Draft)
		var base domain.Subject
		var version *resolvedVersion
		switch in.Base.Type {
		case "recipe_version":
			if version, err = loadVersion(ctx, q, u.ID, in.Base.ID); err != nil {
				return err
			}
			base = version.sub
		default:
			batchID := in.Base.ID
			if in.Base.Type == "bottle" {
				bo, err := q.GetBottle(ctx, sqlc.GetBottleParams{ID: in.Base.ID, UserID: u.ID})
				if err != nil {
					return notFoundIf(err, "Bottle")
				}
				batchID = bo.BatchID
			}
			b, err := q.GetBatch(ctx, sqlc.GetBatchParams{ID: batchID, UserID: u.ID})
			if err != nil {
				return notFoundIf(err, "Batch")
			}
			subs, err := s.batchSubjects(ctx, q, u.ID, []sqlc.Batch{b})
			if err != nil {
				return err
			}
			base = subs[b.ID]
			if b.RecipeVersionID != nil {
				if version, err = loadVersion(ctx, q, u.ID, *b.RecipeVersionID); err != nil {
					return err
				}
			}
		}
		out.Rows = domain.Diff([]domain.Subject{base, draft}, domain.Options{Sections: domain.ConfigSections, TempUnit: profile.TempUnit})
		out.ChangeCount = domain.ChangeCount(out.Rows)
		will := version != nil && len(domain.RecipeChanges(version.sub, draft)) > 0
		out.WillCreateVersion = &will
		if will {
			out.NextVersionNo = ptr(int(version.recipe.LatestVersionNo) + 1)
		}
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------- snapshot and reference

func (s *Service) Snapshot(ctx context.Context, u auth.User, id string) (BatchSnapshot, error) {
	var out BatchSnapshot
	err := s.read(ctx, u, func(q *sqlc.Queries) error {
		row, err := q.GetBatch(ctx, sqlc.GetBatchParams{ID: id, UserID: u.ID})
		if err != nil {
			return notFoundIf(err, "Batch")
		}
		if len(row.RecipeSnapshot) == 0 {
			e := apperr.NotFound("Snapshot")
			e.Code, e.Detail = "snapshot_not_found", "This batch wasn't brewed from a recipe."
			return e
		}
		var snap struct {
			CapturedAt time.Time `json:"capturedAt"`
			Recipe     struct {
				ID, Name, Scope string
			} `json:"recipe"`
			Version struct {
				ID           string `json:"id"`
				VersionNo    int    `json:"versionNo"`
				BaseVolumeMl int    `json:"baseVolumeMl"`
			} `json:"version"`
			Ingredients    []domain.Ingredient    `json:"ingredients"`
			Params         domain.BatchParams     `json:"params"`
			DefaultBottles []domain.DefaultBottle `json:"defaultBottles"`
		}
		if err := unmarshalInto(row.RecipeSnapshot, &snap); err != nil {
			return err
		}
		subs, err := s.batchSubjects(ctx, q, u.ID, []sqlc.Batch{row})
		if err != nil {
			return err
		}
		profile, err := q.GetProfile(ctx, u.ID)
		if err != nil {
			return err
		}
		vol := float64(snap.Version.BaseVolumeMl)
		ins := make([]domain.IngredientInput, len(snap.Ingredients))
		for i, in := range snap.Ingredients {
			ins[i] = in.IngredientInput
		}
		base := domain.Subject{VolumeMl: &vol, Ingredients: ins, Params: snap.Params}
		rows := domain.Diff([]domain.Subject{base, subs[row.ID]}, domain.Options{Sections: domain.ConfigSections, TempUnit: profile.TempUnit, OnlyDifferences: true, PerLitre: true})
		if snap.DefaultBottles == nil {
			snap.DefaultBottles = []domain.DefaultBottle{}
		}
		out = BatchSnapshot{
			BatchID: id, CapturedAt: snap.CapturedAt.UTC(),
			RecipeVersion: RecipeVersionRef{RecipeID: snap.Recipe.ID, RecipeName: snap.Recipe.Name, VersionID: snap.Version.ID, VersionNo: snap.Version.VersionNo, Scope: "batch"},
			Ingredients:   snap.Ingredients, Params: snap.Params, DefaultBottles: snap.DefaultBottles, Deviations: rows,
		}
		return nil
	})
	return out, err
}

func (s *Service) Reference(ctx context.Context, u auth.User, id string) (Reference, error) {
	out := Reference{}
	err := s.read(ctx, u, func(q *sqlc.Queries) error {
		b, err := q.GetBatch(ctx, sqlc.GetBatchParams{ID: id, UserID: u.ID})
		if err != nil {
			return notFoundIf(err, "Batch")
		}
		var recipeID *string
		if b.RecipeVersionID != nil {
			v, err := q.GetRecipeVersion(ctx, sqlc.GetRecipeVersionParams{ID: *b.RecipeVersionID, UserID: u.ID})
			if err != nil {
				return err
			}
			recipeID = &v.RecipeID
		}
		steps := []struct {
			byRecipe, marked bool
			reason           string
		}{
			{true, true, "marked_reference_same_recipe"},
			{true, false, "last_good_same_recipe"},
			{false, true, "marked_reference_same_type"},
			{false, false, "last_good_same_type"},
		}
		for _, st := range steps {
			if st.byRecipe && recipeID == nil {
				continue
			}
			p := sqlc.FindReferenceBatchParams{UserID: u.ID, ExcludeID: id, Before: b.StartedAt, FermentationTypeID: b.FermentationTypeID, Marked: st.marked}
			if st.byRecipe {
				p.RecipeID = recipeID
			}
			ref, err := q.FindReferenceBatch(ctx, p)
			if db.IsNoRows(err) {
				continue
			}
			if err != nil {
				return err
			}
			list, err := s.summaries(ctx, q, u.ID, []sqlc.Batch{ref})
			if err != nil {
				return err
			}
			out.Batch, out.Reason = &list[0], ptr(st.reason)
			return nil
		}
		return nil
	})
	return out, err
}
