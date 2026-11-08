package app

import (
	"context"
	"fmt"
	"net/http"

	"jarlore/internal/apperr"
	"jarlore/internal/auth"
	"jarlore/internal/db/sqlc"
	"jarlore/internal/domain"
)

// ---------------------------------------------------------------- save as recipe

func (in SaveAsRecipeInput) validate() error {
	var f apperr.Fields
	textLen(&f, "name", in.Name, 1, 120)
	textLen(&f, "changeNote", in.ChangeNote, 0, 1000)
	if in.RecipeID == nil && trimPtr(in.Name) == nil {
		f.Add("name", "required", "Name the new recipe.")
	}
	if in.RecipeID != nil && !isUUID(*in.RecipeID) {
		f.Add("recipeId", "invalid", "recipeId is not valid.")
	}
	return f.Err()
}

// targetRecipe returns the recipe to version: an existing one (recipeId) or a new one.
func targetRecipe(ctx context.Context, q *sqlc.Queries, userID string, in SaveAsRecipeInput, scope, typeID string) (sqlc.Recipe, bool, error) {
	if in.RecipeID != nil {
		r, err := q.GetRecipe(ctx, sqlc.GetRecipeParams{ID: *in.RecipeID, UserID: userID})
		if err != nil {
			return r, false, notFoundIf(err, "Recipe")
		}
		if r.Scope != scope || r.FermentationTypeID != typeID {
			return r, false, apperr.Validation(apperr.FieldError{Field: "recipeId", Code: "wrong_scope", Message: fmt.Sprintf("Save into a %s recipe of the same fermentation type.", scope)})
		}
		return r, false, nil
	}
	r, err := q.InsertRecipe(ctx, sqlc.InsertRecipeParams{UserID: userID, Scope: scope, FermentationTypeID: typeID, Name: *trimPtr(in.Name)})
	return r, true, err
}

// SaveBatchAsRecipe creates v1 of a new base recipe from a batch, or the next
// version of an existing one. 409 no_changes when it would equal the latest version.
func (s *Service) SaveBatchAsRecipe(ctx context.Context, u auth.User, batchID, idemKey string, in SaveAsRecipeInput) (Result, error) {
	if err := in.validate(); err != nil {
		return Result{}, err
	}
	return s.write(ctx, u, "saveBatchAsRecipe:"+batchID, idemKey, in, http.StatusCreated, func(q *sqlc.Queries) (any, error) {
		b, err := s.batch(ctx, q, u.ID, batchID)
		if err != nil {
			return nil, err
		}
		recipe, created, err := targetRecipe(ctx, q, u.ID, in, "batch", b.FermentationTypeID)
		if err != nil {
			return nil, err
		}
		ins := make([]domain.IngredientInput, len(b.Ingredients))
		for i, x := range b.Ingredients {
			ins[i] = x.IngredientInput
		}
		vol := float64(b.VolumeMl)
		draft := domain.Subject{VolumeMl: &vol, Ingredients: ins, Params: b.Params}
		args := newVersionArgs{recipe: recipe, changeNote: in.ChangeNote, sourceBatchID: &b.ID, baseVolumeMl: b.VolumeMl, ingredients: ins, params: b.Params}
		for _, bo := range b.Bottles {
			args.defaultBottles = append(args.defaultBottles, domain.DefaultBottle{FlavorName: bo.FlavorName, Additions: bo.Additions, PrimingSugarG: bo.PrimingSugarG})
		}
		if !created {
			latest, err := q.GetRecipeVersionByNo(ctx, sqlc.GetRecipeVersionByNoParams{RecipeID: recipe.ID, VersionNo: recipe.LatestVersionNo, UserID: u.ID})
			if err != nil {
				return nil, err
			}
			base, _, _, err := versionSubject(ctx, q, u.ID, latest)
			if err != nil {
				return nil, err
			}
			changes := domain.RecipeChanges(base, draft)
			if len(changes) == 0 {
				return nil, apperr.Conflict("no_changes", fmt.Sprintf("This batch matches %s v%d already.", recipe.Name, latest.VersionNo))
			}
			args.parentID, args.changedFields = &latest.ID, changes
		}
		v, err := createVersion(ctx, q, u.ID, args)
		if err != nil {
			return nil, err
		}
		// A batch brewed without a recipe now belongs to v1 (its snapshot stays empty).
		if b.RecipeVersion == nil {
			if err := q.PinBatchToVersion(ctx, sqlc.PinBatchToVersionParams{ID: b.ID, UserID: u.ID, RecipeVersionID: &v.ID}); err != nil {
				return nil, err
			}
		}
		return s.recipe(ctx, q, u.ID, recipe.ID)
	})
}

// SaveBottleAsRecipe turns one F2 bottle into a reusable bottle recipe (or a new
// version of one). Later bottles brewed from it keep their own frozen snapshot.
func (s *Service) SaveBottleAsRecipe(ctx context.Context, u auth.User, bottleID, idemKey string, in SaveAsRecipeInput) (Result, error) {
	if err := in.validate(); err != nil {
		return Result{}, err
	}
	return s.write(ctx, u, "saveBottleAsRecipe:"+bottleID, idemKey, in, http.StatusCreated, func(q *sqlc.Queries) (any, error) {
		bo, err := q.GetBottle(ctx, sqlc.GetBottleParams{ID: bottleID, UserID: u.ID})
		if err != nil {
			return nil, notFoundIf(err, "Bottle")
		}
		batch, err := q.GetBatch(ctx, sqlc.GetBatchParams{ID: bo.BatchID, UserID: u.ID})
		if err != nil {
			return nil, err
		}
		spec, err := specOf(bo)
		if err != nil {
			return nil, err
		}
		recipe, created, err := targetRecipe(ctx, q, u.ID, in, "bottle", batch.FermentationTypeID)
		if err != nil {
			return nil, err
		}
		args := newVersionArgs{recipe: recipe, changeNote: in.ChangeNote, sourceBatchID: &batch.ID, sourceBottleID: &bo.ID, baseVolumeMl: spec.VolumeMl, bottle: &spec}
		if !created {
			latest, err := q.GetRecipeVersionByNo(ctx, sqlc.GetRecipeVersionByNoParams{RecipeID: recipe.ID, VersionNo: recipe.LatestVersionNo, UserID: u.ID})
			if err != nil {
				return nil, err
			}
			base, _, _, err := versionSubject(ctx, q, u.ID, latest)
			if err != nil {
				return nil, err
			}
			changes := domain.RecipeChanges(base, bottleSubject(spec))
			if len(changes) == 0 {
				return nil, apperr.Conflict("no_changes", fmt.Sprintf("This bottle matches %s v%d already.", recipe.Name, latest.VersionNo))
			}
			args.parentID, args.changedFields = &latest.ID, changes
		}
		v, err := createVersion(ctx, q, u.ID, args)
		if err != nil {
			return nil, err
		}
		if bo.RecipeVersionID == nil {
			if err := q.PinBottleToVersion(ctx, sqlc.PinBottleToVersionParams{ID: bo.ID, UserID: u.ID, RecipeVersionID: &v.ID}); err != nil {
				return nil, err
			}
		}
		return s.recipe(ctx, q, u.ID, recipe.ID)
	})
}

// ---------------------------------------------------------------- reads

func (s *Service) versions(ctx context.Context, q *sqlc.Queries, userID string, rows []sqlc.RecipeVersion) ([]RecipeVersion, error) {
	ids := make([]string, len(rows))
	for i, v := range rows {
		ids[i] = v.ID
	}
	ings, err := q.ListRecipeIngredients(ctx, sqlc.ListRecipeIngredientsParams{UserID: userID, VersionIds: ids})
	if err != nil {
		return nil, err
	}
	ingBy := map[string][]domain.Ingredient{}
	for _, in := range ings {
		ingBy[in.RecipeVersionID] = append(ingBy[in.RecipeVersionID], domain.Ingredient{ID: in.ID, IngredientInput: domain.IngredientInput{Name: in.Name, Role: in.Role, Amount: in.Amount, Unit: in.Unit}})
	}
	stats, err := q.VersionUseStats(ctx, sqlc.VersionUseStatsParams{UserID: userID, VersionIds: ids})
	if err != nil {
		return nil, err
	}
	statBy := map[string]sqlc.VersionUseStatsRow{}
	for _, st := range stats {
		statBy[st.VersionID] = st
	}
	out := make([]RecipeVersion, len(rows))
	for i, v := range rows {
		var p domain.BatchParams
		var defaults []domain.DefaultBottle
		if err := unmarshalInto(v.Params, &p); err != nil {
			return nil, err
		}
		if err := unmarshalInto(v.DefaultBottles, &defaults); err != nil {
			return nil, err
		}
		if defaults == nil {
			defaults = []domain.DefaultBottle{}
		}
		var spec *domain.BottleSpec
		if len(v.BottleSpec) > 0 {
			spec = &domain.BottleSpec{}
			if err := unmarshalInto(v.BottleSpec, spec); err != nil {
				return nil, err
			}
		}
		list := ingBy[v.ID]
		if list == nil {
			list = []domain.Ingredient{}
		}
		st := statBy[v.ID]
		out[i] = RecipeVersion{
			ID: v.ID, RecipeID: v.RecipeID, VersionNo: int(v.VersionNo), ParentVersionID: v.ParentVersionID, ChangeNote: v.ChangeNote,
			SourceBatchID: v.SourceBatchID, SourceBottleID: v.SourceBottleID, BaseVolumeMl: int(v.BaseVolumeMl), Ingredients: list,
			Params: p, DefaultBottles: defaults, Bottle: spec, ChangedFields: v.ChangedFields, CreatedAt: v.CreatedAt.UTC(),
			BatchCount: int(st.BatchCount), AvgOverall: avgOf(st.OverallSum, st.RatedCount),
		}
	}
	return out, nil
}

// avgOf is the mean overall score of the rated uses, or nil when none is rated.
func avgOf(sum, n int32) *float64 {
	if n == 0 {
		return nil
	}
	return ptr(domain.Round(float64(sum)/float64(n), 1))
}

func (s *Service) recipes(ctx context.Context, q *sqlc.Queries, userID string, rows []sqlc.Recipe) ([]Recipe, error) {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	latest, err := q.GetLatestVersions(ctx, sqlc.GetLatestVersionsParams{UserID: userID, RecipeIds: ids})
	if err != nil {
		return nil, err
	}
	lv, err := s.versions(ctx, q, userID, latest)
	if err != nil {
		return nil, err
	}
	latestBy := map[string]RecipeVersion{}
	for _, v := range lv {
		latestBy[v.RecipeID] = v
	}
	out := make([]Recipe, 0, len(rows))
	for _, r := range rows {
		// Totals over every version of the recipe.
		all, err := q.ListRecipeVersions(ctx, sqlc.ListRecipeVersionsParams{RecipeID: r.ID, UserID: userID, Lim: 1000})
		if err != nil {
			return nil, err
		}
		vids := make([]string, len(all))
		for i, v := range all {
			vids[i] = v.ID
		}
		stats, err := q.VersionUseStats(ctx, sqlc.VersionUseStatsParams{UserID: userID, VersionIds: vids})
		if err != nil {
			return nil, err
		}
		batches, err := q.ListRecipeBatches(ctx, sqlc.ListRecipeBatchesParams{UserID: userID, RecipeID: r.ID, Lim: 10000})
		if err != nil {
			return nil, err
		}
		var sum, n int32
		for _, st := range stats {
			sum += st.OverallSum
			n += st.RatedCount
		}
		avg := avgOf(sum, n)
		out = append(out, Recipe{
			ID: r.ID, Name: r.Name, Scope: r.Scope, FermentationTypeID: r.FermentationTypeID, LatestVersion: latestBy[r.ID],
			VersionCount: int(r.LatestVersionNo), BatchCount: len(batches), AvgOverall: avg, ArchivedAt: utcPtr(r.ArchivedAt), CreatedAt: r.CreatedAt.UTC(),
		})
	}
	return out, nil
}

func (s *Service) recipe(ctx context.Context, q *sqlc.Queries, userID, id string) (Recipe, error) {
	r, err := q.GetRecipe(ctx, sqlc.GetRecipeParams{ID: id, UserID: userID})
	if err != nil {
		return Recipe{}, notFoundIf(err, "Recipe")
	}
	list, err := s.recipes(ctx, q, userID, []sqlc.Recipe{r})
	if err != nil {
		return Recipe{}, err
	}
	return list[0], nil
}

type RecipeQuery struct {
	Q, Type, Scope *string
	Archived       bool
	Cursor         string
	Limit          int
}

func (s *Service) ListRecipes(ctx context.Context, u auth.User, in RecipeQuery) (Page[Recipe], error) {
	var page Page[Recipe]
	scope := fmt.Sprintf("recipes|%s|%s|%s|%v", deref(in.Q), deref(in.Type), deref(in.Scope), in.Archived)
	cur, err := s.decodeCursor(in.Cursor, scope)
	if err != nil {
		return page, err
	}
	lim := pageLimit(in.Limit)
	err = s.read(ctx, u, func(q *sqlc.Queries) error {
		p := sqlc.ListRecipesParams{UserID: u.ID, Q: in.Q, Type: in.Type, Scope: in.Scope, Archived: in.Archived, Lim: lim + 1}
		if cur != nil {
			p.AfterTime, p.AfterID = cur.Time, &cur.ID
		}
		rows, err := q.ListRecipes(ctx, p)
		if err != nil {
			return err
		}
		if len(rows) > int(lim) {
			rows = rows[:lim]
			last := rows[len(rows)-1]
			page.NextCursor = s.encodeCursor(cursor{Scope: scope, Time: ptr(last.CreatedAt), ID: last.ID})
		}
		page.Data, err = s.recipes(ctx, q, u.ID, rows)
		return err
	})
	return page, err
}

func (s *Service) GetRecipe(ctx context.Context, u auth.User, id string) (Recipe, error) {
	var r Recipe
	err := s.read(ctx, u, func(q *sqlc.Queries) (err error) {
		r, err = s.recipe(ctx, q, u.ID, id)
		return err
	})
	return r, err
}

func (s *Service) UpdateRecipe(ctx context.Context, u auth.User, id string, in UpdateRecipeInput) (Result, error) {
	var f apperr.Fields
	textLen(&f, "name", in.Name, 1, 120)
	if in.Name == nil && in.Archived == nil {
		f.Add("body", "empty", "Send name or archived.")
	}
	if err := f.Err(); err != nil {
		return Result{}, err
	}
	now := s.now()
	return s.write(ctx, u, "updateRecipe", "", nil, http.StatusOK, func(q *sqlc.Queries) (any, error) {
		p := sqlc.UpdateRecipeParams{ID: id, UserID: u.ID, Name: trimPtr(in.Name)}
		if in.Archived != nil {
			p.SetArchived = true
			if *in.Archived {
				p.ArchivedAt = &now
			}
		}
		if _, err := q.UpdateRecipe(ctx, p); err != nil {
			return nil, notFoundIf(err, "Recipe")
		}
		return s.recipe(ctx, q, u.ID, id)
	})
}

func (s *Service) ListRecipeVersions(ctx context.Context, u auth.User, recipeID, cursorToken string, limit int) (Page[RecipeVersion], error) {
	var page Page[RecipeVersion]
	scope := "versions|" + recipeID
	cur, err := s.decodeCursor(cursorToken, scope)
	if err != nil {
		return page, err
	}
	lim := pageLimit(limit)
	err = s.read(ctx, u, func(q *sqlc.Queries) error {
		if _, err := q.GetRecipe(ctx, sqlc.GetRecipeParams{ID: recipeID, UserID: u.ID}); err != nil {
			return notFoundIf(err, "Recipe")
		}
		p := sqlc.ListRecipeVersionsParams{RecipeID: recipeID, UserID: u.ID, Lim: lim + 1}
		if cur != nil {
			p.BeforeNo = cur.Num
		}
		rows, err := q.ListRecipeVersions(ctx, p)
		if err != nil {
			return err
		}
		if len(rows) > int(lim) {
			rows = rows[:lim]
			last := rows[len(rows)-1]
			page.NextCursor = s.encodeCursor(cursor{Scope: scope, Num: ptr(last.VersionNo), ID: last.ID})
		}
		page.Data, err = s.versions(ctx, q, u.ID, rows)
		return err
	})
	return page, err
}

func (s *Service) GetRecipeVersionByNo(ctx context.Context, u auth.User, recipeID string, no int) (RecipeVersion, error) {
	var out RecipeVersion
	err := s.read(ctx, u, func(q *sqlc.Queries) error {
		v, err := q.GetRecipeVersionByNo(ctx, sqlc.GetRecipeVersionByNoParams{RecipeID: recipeID, VersionNo: int32(no), UserID: u.ID})
		if err != nil {
			return notFoundIf(err, "Recipe version")
		}
		list, err := s.versions(ctx, q, u.ID, []sqlc.RecipeVersion{v})
		if err != nil {
			return err
		}
		out = list[0]
		return nil
	})
	return out, err
}

func (s *Service) GetRecipeVersion(ctx context.Context, u auth.User, id string) (RecipeVersionDetail, error) {
	var out RecipeVersionDetail
	err := s.read(ctx, u, func(q *sqlc.Queries) error {
		v, err := q.GetRecipeVersion(ctx, sqlc.GetRecipeVersionParams{ID: id, UserID: u.ID})
		if err != nil {
			return notFoundIf(err, "Recipe version")
		}
		r, err := q.GetRecipe(ctx, sqlc.GetRecipeParams{ID: v.RecipeID, UserID: u.ID})
		if err != nil {
			return err
		}
		list, err := s.versions(ctx, q, u.ID, []sqlc.RecipeVersion{v})
		if err != nil {
			return err
		}
		out = RecipeVersionDetail{RecipeVersion: list[0], RecipeName: r.Name, Scope: r.Scope, FermentationTypeID: r.FermentationTypeID}
		return nil
	})
	return out, err
}

// DiffRecipeVersions compares two versions of one recipe. The free plan diffs
// adjacent versions; any pair needs Pro.
func (s *Service) DiffRecipeVersions(ctx context.Context, u auth.User, recipeID string, from, to int) (DiffResult, error) {
	var out DiffResult
	if from < 1 || to < 1 {
		return out, apperr.BadRequest("from and to must be version numbers (1 or more).")
	}
	err := s.read(ctx, u, func(q *sqlc.Queries) error {
		if _, err := q.GetRecipe(ctx, sqlc.GetRecipeParams{ID: recipeID, UserID: u.ID}); err != nil {
			return notFoundIf(err, "Recipe")
		}
		if d := from - to; d > 1 || d < -1 {
			e, err := s.entitlements(ctx, q, u.ID)
			if err != nil {
				return err
			}
			if e.Plan != "pro" {
				return apperr.EntitlementRequired("The free plan compares neighbouring versions. Pro compares any two.")
			}
		}
		subs := make([]domain.Subject, 0, 2)
		for _, no := range []int{from, to} {
			v, err := q.GetRecipeVersionByNo(ctx, sqlc.GetRecipeVersionByNoParams{RecipeID: recipeID, VersionNo: int32(no), UserID: u.ID})
			if err != nil {
				return notFoundIf(err, fmt.Sprintf("Version %d", no))
			}
			sub, _, _, err := versionSubject(ctx, q, u.ID, v)
			if err != nil {
				return err
			}
			subs = append(subs, sub)
		}
		profile, err := q.GetProfile(ctx, u.ID)
		if err != nil {
			return err
		}
		out.Rows = domain.Diff(subs, domain.Options{Sections: domain.RecipeSections, TempUnit: profile.TempUnit, PerLitre: true})
		out.ChangeCount = domain.ChangeCount(out.Rows)
		return nil
	})
	return out, err
}

func (s *Service) ListRecipeBatches(ctx context.Context, u auth.User, recipeID, cursorToken string, limit int) (Page[BatchSummary], error) {
	var page Page[BatchSummary]
	scope := "recipe-batches|" + recipeID
	cur, err := s.decodeCursor(cursorToken, scope)
	if err != nil {
		return page, err
	}
	lim := pageLimit(limit)
	err = s.read(ctx, u, func(q *sqlc.Queries) error {
		if _, err := q.GetRecipe(ctx, sqlc.GetRecipeParams{ID: recipeID, UserID: u.ID}); err != nil {
			return notFoundIf(err, "Recipe")
		}
		p := sqlc.ListRecipeBatchesParams{UserID: u.ID, RecipeID: recipeID, Lim: lim + 1}
		if cur != nil {
			p.AfterTime, p.AfterID = cur.Time, &cur.ID
		}
		rows, err := q.ListRecipeBatches(ctx, p)
		if err != nil {
			return err
		}
		if len(rows) > int(lim) {
			rows = rows[:lim]
			last := rows[len(rows)-1]
			page.NextCursor = s.encodeCursor(cursor{Scope: scope, Time: ptr(last.StartedAt), ID: last.ID})
		}
		page.Data, err = s.summaries(ctx, q, u.ID, rows)
		return err
	})
	return page, err
}
