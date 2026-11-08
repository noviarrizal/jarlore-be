package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"jarlore/internal/apperr"
	"jarlore/internal/auth"
	"jarlore/internal/db/sqlc"
	"jarlore/internal/domain"
)

func parseStages(raw []byte) ([]FermentationStage, error) {
	var st []FermentationStage
	return st, unmarshalInto(raw, &st)
}

func stageOf(stages []FermentationStage, key string) *FermentationStage {
	for i := range stages {
		if stages[i].Key == key {
			return &stages[i]
		}
	}
	return nil
}

func bottleSubject(spec domain.BottleSpec) domain.Subject {
	return domain.Subject{Bottle: &spec}
}

func specOf(b sqlc.Bottle) (domain.BottleSpec, error) {
	spec := domain.BottleSpec{FlavorName: b.FlavorName, VolumeMl: int(b.VolumeMl), PrimingSugarG: b.PrimingSugarG, HeadspaceCm: b.HeadspaceCm}
	if err := unmarshalInto(b.Additions, &spec.Additions); err != nil {
		return spec, err
	}
	if spec.Additions == nil {
		spec.Additions = []domain.Addition{}
	}
	return spec, nil
}

// CreateBottles splits an F1 batch into F2 bottles (Bottle it). Each bottle is
// its own experiment: it may start from a bottle recipe or from an earlier
// bottle, and the version rule applies to its additions.
func (s *Service) CreateBottles(ctx context.Context, u auth.User, batchID, idemKey string, in CreateBottlesInput) (Result, error) {
	now := s.now()
	var f apperr.Fields
	if len(in.Bottles) == 0 || len(in.Bottles) > 50 {
		f.Add("bottles", "count", "Add between 1 and 50 bottles.")
	}
	labels := map[string]bool{}
	for i := range in.Bottles {
		b := &in.Bottles[i]
		if b.Additions == nil {
			b.Additions = []domain.Addition{}
		}
		p := fmt.Sprintf("bottles[%d]", i)
		validBottle(&f, p, *b)
		l := strings.ToUpper(strings.TrimSpace(b.Label))
		if labels[l] {
			f.Add(p+".label", "duplicate", "Each bottle needs its own label.")
		}
		labels[l] = true
	}
	notFuture(&f, "bottledAt", in.BottledAt, now)
	if err := f.Err(); err != nil {
		return Result{}, err
	}
	return s.write(ctx, u, "createBottles:"+batchID, idemKey, in, http.StatusCreated, func(q *sqlc.Queries) (any, error) {
		batch, err := q.GetBatchForUpdate(ctx, sqlc.GetBatchForUpdateParams{ID: batchID, UserID: u.ID})
		if err != nil {
			return nil, notFoundIf(err, "Batch")
		}
		ft, err := q.GetFermentationType(ctx, batch.FermentationTypeID)
		if err != nil {
			return nil, err
		}
		stages, err := parseStages(ft.Stages)
		if err != nil {
			return nil, err
		}
		f2 := stageOf(stages, "f2")
		if batch.Status != "active" || f2 == nil || (batch.Stage != "f1" && batch.Stage != "f2") {
			return nil, apperr.InvalidState("Bottles can be added only to an active batch whose type has an F2 stage.")
		}
		at := now
		if in.BottledAt != nil {
			at = in.BottledAt.UTC()
		}
		if at.Before(batch.StartedAt) {
			return nil, apperr.Validation(apperr.FieldError{Field: "bottledAt", Code: "before_start", Message: "Bottling can't be before the batch started."})
		}
		profile, err := q.GetProfile(ctx, u.ID)
		if err != nil {
			return nil, err
		}

		for i, bi := range in.Bottles {
			p := fmt.Sprintf("bottles[%d]", i)
			spec := bi.Spec()
			draft := bottleSubject(spec)
			var version *resolvedVersion
			var parent *sqlc.Bottle
			if bi.Source != nil {
				switch bi.Source.Type {
				case "recipe_version":
					if version, err = loadVersion(ctx, q, u.ID, bi.Source.ID); err != nil {
						return nil, err
					}
				case "bottle":
					pb, err := q.GetBottle(ctx, sqlc.GetBottleParams{ID: bi.Source.ID, UserID: u.ID})
					if err != nil {
						return nil, notFoundIf(err, "Source bottle")
					}
					parent = &pb
					if pb.RecipeVersionID != nil {
						if version, err = loadVersion(ctx, q, u.ID, *pb.RecipeVersionID); err != nil {
							return nil, err
						}
					}
				}
			}
			if version != nil && (version.row.Scope != "bottle" || version.row.FermentationTypeID != batch.FermentationTypeID) {
				return nil, apperr.Validation(apperr.FieldError{Field: p + ".source", Code: "wrong_scope", Message: "Use a bottle recipe of the same fermentation type."})
			}

			bottleID := newID()
			var versionID *string
			var snapshot []byte
			changed := []string{}
			if version != nil {
				pinned := version.row
				if changes := domain.RecipeChanges(version.sub, draft); len(changes) > 0 {
					pinned, err = createVersion(ctx, q, u.ID, newVersionArgs{
						recipe: version.recipe, parentID: &version.row.ID, changeNote: bi.Hypothesis, sourceBottleID: &bottleID,
						baseVolumeMl: spec.VolumeMl, bottle: &spec, changedFields: changes,
					})
					if err != nil {
						return nil, err
					}
				}
				versionID = &pinned.ID
				if snapshot, err = snapshotJSON(ctx, q, u.ID, pinned, version.recipe.Name, at); err != nil {
					return nil, err
				}
				changed = changedKeys(domain.Diff([]domain.Subject{version.sub, draft}, domain.Options{Sections: domain.ConfigSections}))
			}
			var parentID *string
			if parent != nil {
				parentID = &parent.ID
				ps, err := specOf(*parent)
				if err != nil {
					return nil, err
				}
				changed = changedKeys(domain.Diff([]domain.Subject{bottleSubject(ps), draft}, domain.Options{Sections: domain.ConfigSections}))
			}
			bo, err := q.InsertBottle(ctx, sqlc.InsertBottleParams{
				ID: bottleID, UserID: u.ID, BatchID: batchID, Label: strings.ToUpper(strings.TrimSpace(bi.Label)), FlavorName: spec.FlavorName,
				VolumeMl: int32(spec.VolumeMl), Additions: mustJSON(spec.Additions), PrimingSugarG: spec.PrimingSugarG,
				HeadspaceCm: spec.HeadspaceCm, StartedAt: at, RecipeVersionID: versionID, RecipeSnapshot: snapshot,
				ParentBottleID: parentID, Hypothesis: trimPtr(bi.Hypothesis), ChangedFields: changed,
			})
			if err != nil {
				return nil, err
			}
			if due := localMorning(at.Add(24*time.Hour), profile.Timezone); due.After(now) {
				if _, err := q.InsertReminder(ctx, sqlc.InsertReminderParams{UserID: u.ID, BatchID: batchID, BottleID: &bo.ID, Kind: "burp", DueAt: due, Label: "Burp bottle " + bo.Label}); err != nil {
					return nil, err
				}
			}
		}

		if batch.Stage == "f1" {
			if err := q.MoveBatchToF2(ctx, sqlc.MoveBatchToF2Params{ID: batchID, UserID: u.ID, BottledAt: &at}); err != nil {
				return nil, err
			}
			if _, err := q.InsertLog(ctx, sqlc.InsertLogParams{UserID: u.ID, BatchID: batchID, Kind: "stage_change", OccurredAt: at, Note: ptr(fmt.Sprintf("F2 started: %d %s", len(in.Bottles), plural(len(in.Bottles), "bottle", "bottles")))}); err != nil {
				return nil, err
			}
			if err := q.DismissOpenReminders(ctx, sqlc.DismissOpenRemindersParams{UserID: u.ID, BatchID: batchID, Kinds: []string{"taste_check", "bottle"}}); err != nil {
				return nil, err
			}
			if due := localMorning(at.Add(time.Duration(f2.TypicalDays[1])*24*time.Hour), profile.Timezone); f2.TypicalDays[1] > 0 && due.After(now) {
				if _, err := q.InsertReminder(ctx, sqlc.InsertReminderParams{UserID: u.ID, BatchID: batchID, Kind: "chill", DueAt: due, Label: "Move " + batch.Code + " to the fridge"}); err != nil {
					return nil, err
				}
			}
		}
		if err := q.TouchBatch(ctx, sqlc.TouchBatchParams{ID: batchID, UserID: u.ID}); err != nil {
			return nil, err
		}
		return s.batch(ctx, q, u.ID, batchID)
	})
}

func (s *Service) UpdateBottle(ctx context.Context, u auth.User, id string, in UpdateBottleInput) (Result, error) {
	var f apperr.Fields
	textLen(&f, "label", in.Label, 1, 8)
	textLen(&f, "flavorName", in.FlavorName, 1, 80)
	if in.VolumeMl != nil && (*in.VolumeMl < 1 || *in.VolumeMl > 20000) {
		f.Add("volumeMl", "out_of_range", "Bottle size must be between 1 and 20000 ml.")
	}
	if in.Additions != nil {
		validAdditions(&f, "additions", *in.Additions)
	}
	numRange(&f, "primingSugarG", in.PrimingSugarG.Value, 0, 1000)
	numRange(&f, "headspaceCm", in.HeadspaceCm.Value, 0, 50)
	if in.Status != nil && !domain.Contains(domain.BottleUserStatus, *in.Status) {
		f.Add("status", "invalid_enum", "Status can be conditioning, chilled or discarded. Use finish to rate a bottle.")
	}
	configEdit := in.Label != nil || in.FlavorName != nil || in.VolumeMl != nil || in.Additions != nil || in.PrimingSugarG.Set || in.HeadspaceCm.Set
	if !configEdit && in.Status == nil && in.IsReference == nil {
		f.Add("body", "empty", "Send at least one field to change.")
	}
	if err := f.Err(); err != nil {
		return Result{}, err
	}
	now := s.now()
	return s.write(ctx, u, "updateBottle", "", nil, http.StatusOK, func(q *sqlc.Queries) (any, error) {
		cur, err := q.GetBottleForUpdate(ctx, sqlc.GetBottleForUpdateParams{ID: id, UserID: u.ID})
		if err != nil {
			return nil, notFoundIf(err, "Bottle")
		}
		closed := cur.Status == "finished" || cur.Status == "discarded"
		if closed && (configEdit || in.Status != nil) {
			return nil, apperr.InvalidState("This bottle is " + cur.Status + " and kept as history. You can still mark it as the best one.")
		}
		if configEdit && cur.RecipeVersionID != nil {
			return nil, apperr.FieldImmutable("additions", "This bottle was brewed from a recipe version, so its setup is fixed. Start a new bottle to try a change.")
		}
		if in.Status != nil {
			switch {
			case *in.Status == cur.Status:
			case *in.Status == "chilled" && cur.Status == "conditioning", *in.Status == "discarded":
			default:
				return nil, apperr.InvalidState(fmt.Sprintf("A bottle can't go from %s to %s.", cur.Status, *in.Status))
			}
		}
		if in.IsReference != nil && *in.IsReference {
			if cur.Status != "finished" {
				return nil, apperr.InvalidState("Finish and rate a bottle before marking it as the best one.")
			}
			if err := q.ClearBottleReference(ctx, sqlc.ClearBottleReferenceParams{BatchID: cur.BatchID, UserID: u.ID, KeepID: id}); err != nil {
				return nil, err
			}
		}
		p := sqlc.UpdateBottleParams{
			ID: id, UserID: u.ID, Now: &now, Label: upperTrim(in.Label), FlavorName: trimPtr(in.FlavorName), IsReference: in.IsReference,
			SetPrimingSugar: in.PrimingSugarG.Set, PrimingSugarG: in.PrimingSugarG.Value,
			SetHeadspace: in.HeadspaceCm.Set, HeadspaceCm: in.HeadspaceCm.Value, Status: in.Status,
		}
		if in.VolumeMl != nil {
			p.VolumeMl = ptr(int32(*in.VolumeMl))
		}
		if in.Additions != nil {
			p.Additions = mustJSON(*in.Additions)
		}
		row, err := q.UpdateBottle(ctx, p)
		if err != nil {
			return nil, err
		}
		if in.Status != nil && *in.Status != cur.Status {
			kinds := []string{"burp"}
			if *in.Status == "discarded" {
				kinds = []string{}
			}
			if err := q.DismissOpenReminders(ctx, sqlc.DismissOpenRemindersParams{UserID: u.ID, BatchID: cur.BatchID, BottleID: &id, Kinds: kinds}); err != nil {
				return nil, err
			}
		}
		if err := q.TouchBatch(ctx, sqlc.TouchBatchParams{ID: cur.BatchID, UserID: u.ID}); err != nil {
			return nil, err
		}
		return s.bottle(ctx, q, u.ID, row)
	})
}

func upperTrim(s *string) *string {
	if t := trimPtr(s); t != nil {
		u := strings.ToUpper(*t)
		return &u
	}
	return nil
}

// FinishBottle records the result of one F2 experiment. A bottle that closed with
// its batch, unrated, can be rated once afterwards.
func (s *Service) FinishBottle(ctx context.Context, u auth.User, id, idemKey string, in FinishInput) (Result, error) {
	var f apperr.Fields
	validRating(&f, in.Rating)
	now := s.now()
	notFuture(&f, "finishedAt", in.FinishedAt, now)
	if err := f.Err(); err != nil {
		return Result{}, err
	}
	return s.write(ctx, u, "finishBottle:"+id, idemKey, in, http.StatusOK, func(q *sqlc.Queries) (any, error) {
		cur, err := q.GetBottleForUpdate(ctx, sqlc.GetBottleForUpdateParams{ID: id, UserID: u.ID})
		if err != nil {
			return nil, notFoundIf(err, "Bottle")
		}
		switch {
		case cur.Status == "discarded":
			return nil, apperr.InvalidState("This bottle was discarded.")
		case cur.Status == "finished" && len(cur.Rating) > 0:
			return nil, apperr.InvalidState("This bottle is already rated. Ratings are kept as history.")
		}
		at := now
		if in.FinishedAt != nil {
			at = in.FinishedAt.UTC()
		}
		if at.Before(cur.StartedAt) {
			return nil, apperr.Validation(apperr.FieldError{Field: "finishedAt", Code: "before_start", Message: "A bottle can't finish before it was bottled."})
		}
		row, err := q.FinishBottle(ctx, sqlc.FinishBottleParams{ID: id, UserID: u.ID, FinishedAt: &at, Rating: mustJSON(in.Rating)})
		if err != nil {
			return nil, err
		}
		if err := q.DismissOpenReminders(ctx, sqlc.DismissOpenRemindersParams{UserID: u.ID, BatchID: cur.BatchID, BottleID: &id, Kinds: []string{}}); err != nil {
			return nil, err
		}
		if err := q.TouchBatch(ctx, sqlc.TouchBatchParams{ID: cur.BatchID, UserID: u.ID}); err != nil {
			return nil, err
		}
		return s.bottle(ctx, q, u.ID, row)
	})
}

// PreviewBottle diffs a draft bottle against an earlier bottle or a bottle recipe version.
func (s *Service) PreviewBottle(ctx context.Context, u auth.User, in BottlePreviewInput) (DiffResult, error) {
	var out DiffResult
	var f apperr.Fields
	validSource(&f, "base", &in.Base, "bottle", "recipe_version")
	if in.Draft.Additions == nil {
		in.Draft.Additions = []domain.Addition{}
	}
	validBottle(&f, "draft", in.Draft)
	if err := f.Err(); err != nil {
		return out, err
	}
	err := s.read(ctx, u, func(q *sqlc.Queries) error {
		draft := bottleSubject(in.Draft.Spec())
		var base domain.Subject
		var version *resolvedVersion
		var err error
		if in.Base.Type == "recipe_version" {
			if version, err = loadVersion(ctx, q, u.ID, in.Base.ID); err != nil {
				return err
			}
			if version.row.Scope != "bottle" {
				return apperr.Validation(apperr.FieldError{Field: "base", Code: "wrong_scope", Message: "Compare a bottle with a bottle recipe."})
			}
			base = version.sub
		} else {
			bo, err := q.GetBottle(ctx, sqlc.GetBottleParams{ID: in.Base.ID, UserID: u.ID})
			if err != nil {
				return notFoundIf(err, "Bottle")
			}
			spec, err := specOf(bo)
			if err != nil {
				return err
			}
			base = bottleSubject(spec)
			if bo.RecipeVersionID != nil {
				if version, err = loadVersion(ctx, q, u.ID, *bo.RecipeVersionID); err != nil {
					return err
				}
			}
		}
		out.Rows = domain.Diff([]domain.Subject{base, draft}, domain.Options{Sections: domain.ConfigSections})
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

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
