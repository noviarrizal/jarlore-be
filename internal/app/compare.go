package app

import (
	"context"
	"fmt"

	"jarlore/internal/apperr"
	"jarlore/internal/auth"
	"jarlore/internal/db/sqlc"
	"jarlore/internal/domain"
)

// Compare lines up 2..6 batches or bottles, from one F1 or across batches. A
// bottle brings its batch's ingredients and conditions plus its own additions,
// readings and rating. The first item is the baseline.
func (s *Service) Compare(ctx context.Context, u auth.User, in CompareInput) (CompareResult, error) {
	var out CompareResult
	var f apperr.Fields
	if len(in.Items) < 2 || len(in.Items) > proCompareItems {
		f.Add("items", "count", fmt.Sprintf("Compare 2 to %d items.", proCompareItems))
	}
	seen := map[ItemRef]bool{}
	for i, it := range in.Items {
		if (it.Type != "batch" && it.Type != "bottle") || !isUUID(it.ID) {
			f.Add(fmt.Sprintf("items[%d]", i), "invalid", "Each item is a batch or a bottle id.")
		}
		if seen[it] {
			f.Add(fmt.Sprintf("items[%d]", i), "duplicate", "Each item can appear once.")
		}
		seen[it] = true
	}
	if err := f.Err(); err != nil {
		return out, err
	}
	err := s.read(ctx, u, func(q *sqlc.Queries) error {
		ent, err := s.entitlements(ctx, q, u.ID)
		if err != nil {
			return err
		}
		var batchIDs, bottleIDs []string
		for _, it := range in.Items {
			if it.Type == "batch" {
				batchIDs = append(batchIDs, it.ID)
			} else {
				bottleIDs = append(bottleIDs, it.ID)
			}
		}
		bottleRows, err := q.GetBottlesByIDs(ctx, sqlc.GetBottlesByIDsParams{UserID: u.ID, Ids: bottleIDs})
		if err != nil {
			return err
		}
		bottleBy := map[string]sqlc.Bottle{}
		for _, b := range bottleRows {
			bottleBy[b.ID] = b
			batchIDs = append(batchIDs, b.BatchID)
		}
		// The bottles of one batch are one F2 experiment and can always be compared
		// side by side; the plan limit applies to every other comparison.
		limit := ent.Limits.CompareItems
		if len(bottleRows) == len(in.Items) && sameBatch(bottleRows) {
			limit = proCompareItems
		}
		if len(in.Items) > limit {
			return apperr.EntitlementRequired(fmt.Sprintf("The free plan compares up to %d items. Bottles of one batch can always be compared.", limit),
				apperr.FieldError{Field: "items", Code: "too_many", Message: fmt.Sprintf("Maximum %d items on your plan", limit)})
		}
		batchRows, err := q.GetBatchesByIDs(ctx, sqlc.GetBatchesByIDsParams{UserID: u.ID, Ids: batchIDs})
		if err != nil {
			return err
		}
		batchBy := map[string]sqlc.Batch{}
		for _, b := range batchRows {
			batchBy[b.ID] = b
		}
		// Any id that isn't the caller's fails the whole request with 404.
		for i, it := range in.Items {
			if it.Type == "batch" {
				if _, ok := batchBy[it.ID]; !ok {
					return apperr.NotFound(fmt.Sprintf("Item %d", i+1))
				}
			} else if _, ok := bottleBy[it.ID]; !ok {
				return apperr.NotFound(fmt.Sprintf("Item %d", i+1))
			}
		}
		uniq := make([]sqlc.Batch, 0, len(batchBy))
		for _, id := range sortedKeys(batchBy) {
			uniq = append(uniq, batchBy[id])
		}
		subs, err := s.batchSubjects(ctx, q, u.ID, uniq)
		if err != nil {
			return err
		}
		readings, err := q.ListReadingsByBatchIDs(ctx, sqlc.ListReadingsByBatchIDsParams{UserID: u.ID, BatchIds: sortedKeys(batchBy)})
		if err != nil {
			return err
		}
		series := buildSeries(readings)
		var bottleVersionIDs []string
		for _, b := range bottleRows {
			if b.RecipeVersionID != nil {
				bottleVersionIDs = append(bottleVersionIDs, *b.RecipeVersionID)
			}
		}
		refs, err := versionRefs(ctx, q, u.ID, bottleVersionIDs)
		if err != nil {
			return err
		}

		subjects := make([]domain.Subject, len(in.Items))
		for i, it := range in.Items {
			if it.Type == "batch" {
				b := batchBy[it.ID]
				subjects[i] = subs[b.ID]
				out.Items = append(out.Items, CompareItem{Ref: it, Title: b.Code, Subtitle: ptr(b.Name), IsReference: b.IsReference})
				continue
			}
			bo := bottleBy[it.ID]
			b := batchBy[bo.BatchID]
			parent := subs[b.ID]
			spec, err := specOf(bo)
			if err != nil {
				return err
			}
			rating, err := ratingOf(bo.Rating)
			if err != nil {
				return err
			}
			sub := domain.Subject{
				VolumeMl: parent.VolumeMl, Ingredients: parent.Ingredients, Params: parent.Params, F1Days: parent.F1Days,
				Bottle: &spec, Measurements: series.bottles[bo.ID], Rating: rating,
			}
			end := bo.FinishedAt
			if end == nil {
				end = bo.ChilledAt
			}
			sub.F2Days = daysBetween(bo.StartedAt, end)
			if bo.RecipeVersionID != nil {
				if r := refs[*bo.RecipeVersionID]; r != nil {
					sub.RecipeLabel = ptr(fmt.Sprintf("%s v%d", r.RecipeName, r.VersionNo))
				}
			} else {
				sub.RecipeLabel = parent.RecipeLabel
			}
			subjects[i] = sub
			out.Items = append(out.Items, CompareItem{Ref: it, Title: b.Code + " " + bo.Label, Subtitle: ptr(bo.FlavorName), IsReference: bo.IsReference})
		}
		defs, err := loadDefs(ctx, q, u.ID)
		if err != nil {
			return err
		}
		profile, err := q.GetProfile(ctx, u.ID)
		if err != nil {
			return err
		}
		out.Rows = domain.Diff(subjects, domain.Options{Sections: domain.AllSections, TempUnit: profile.TempUnit, Defs: defs.list, OnlyDifferences: in.OnlyDifferences})
		return nil
	})
	return out, err
}

func sameBatch(rows []sqlc.Bottle) bool {
	for _, b := range rows {
		if b.BatchID != rows[0].BatchID {
			return false
		}
	}
	return len(rows) > 0
}
