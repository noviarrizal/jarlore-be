package api

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"jarlore/internal/app"
	"jarlore/internal/apperr"
	"jarlore/internal/auth"
	"jarlore/internal/httpx"
)

type handlers struct {
	svc *app.Service
}

func user(r *http.Request) auth.User {
	u, _ := auth.FromContext(r.Context())
	return u
}

// respond writes v or the error.
func respond[T any](w http.ResponseWriter, r *http.Request, v T, err error) {
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, v)
}

func result(w http.ResponseWriter, r *http.Request, res app.Result, err error) {
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	writeResult(w, res)
}

// write decodes the body and runs a state-changing use case.
func write[T any](w http.ResponseWriter, r *http.Request, run func(in T) (app.Result, error)) {
	var in T
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	res, err := run(in)
	result(w, r, res, err)
}

// ---------------------------------------------------------------- me and reference data

func (h *handlers) getMe(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Me(r.Context(), user(r))
	respond(w, r, v, err)
}

func (h *handlers) updateMe(w http.ResponseWriter, r *http.Request) {
	write(w, r, func(in app.UpdateMeInput) (app.Result, error) { return h.svc.UpdateMe(r.Context(), user(r), in) })
}

func (h *handlers) entitlements(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Entitlements(r.Context(), user(r))
	respond(w, r, v, err)
}

func (h *handlers) fermentationTypes(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.FermentationTypes(r.Context(), user(r))
	if err == nil {
		w.Header().Set("Cache-Control", "private, max-age=3600")
	}
	respond(w, r, map[string]any{"data": v}, err)
}

func (h *handlers) measurementDefinitions(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.MeasurementDefinitions(r.Context(), user(r))
	respond(w, r, map[string]any{"data": v}, err)
}

func (h *handlers) createMeasurementDefinition(w http.ResponseWriter, r *http.Request) {
	write(w, r, func(in app.CreateDefinitionInput) (app.Result, error) {
		return h.svc.CreateMeasurementDefinition(r.Context(), user(r), in)
	})
}

func (h *handlers) today(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Today(r.Context(), user(r))
	respond(w, r, v, err)
}

// ---------------------------------------------------------------- batches

func (h *handlers) listBatches(w http.ResponseWriter, r *http.Request) {
	var q app.BatchQuery
	var err error
	fail := func(e error) bool {
		if e != nil && err == nil {
			err = e
		}
		return err != nil
	}
	q.Status, err = qenum(r, "status", "active", "finished", "discarded")
	s, e := qenum(r, "stage", "f1", "f2", "done")
	fail(e)
	q.Stage = s
	t, e := qenum(r, "type", "kombucha", "water_kefir", "custom")
	fail(e)
	q.Type = t
	rid, e := quuid(r, "recipeId")
	fail(e)
	q.RecipeID = rid
	qq, e := qstr(r, "q", 100)
	fail(e)
	q.Q = qq
	q.Archived, e = qbool(r, "archived")
	fail(e)
	if sort, e := qenum(r, "sort", "started_desc", "started_asc", "rating_desc", "updated_desc"); !fail(e) && sort != nil {
		q.Sort = *sort
	}
	q.Limit, e = qlimit(r)
	fail(e)
	q.Cursor = r.URL.Query().Get("cursor")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	v, err := h.svc.ListBatches(r.Context(), user(r), q)
	respond(w, r, v, err)
}

func (h *handlers) createBatch(w http.ResponseWriter, r *http.Request) {
	key, err := idemKey(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var in app.CreateBatchInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	res, err := h.svc.CreateBatch(r.Context(), user(r), key, in)
	if err == nil && !res.Replayed {
		w.Header().Set("Location", "/v1/batches/"+batchIDOf(res))
	}
	result(w, r, res, err)
}

func batchIDOf(res app.Result) string {
	// The body starts with {"id":"<uuid>" (batchCore's first field).
	const prefix = `{"id":"`
	if len(res.Body) > len(prefix)+36 && string(res.Body[:len(prefix)]) == prefix {
		return string(res.Body[len(prefix) : len(prefix)+36])
	}
	return ""
}

func (h *handlers) previewBatch(w http.ResponseWriter, r *http.Request) {
	var in app.BatchPreviewInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	v, err := h.svc.PreviewBatch(r.Context(), user(r), in)
	respond(w, r, v, err)
}

func (h *handlers) getBatch(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.GetBatch(r.Context(), user(r), chi.URLParam(r, "batchID"))
	respond(w, r, v, err)
}

func (h *handlers) updateBatch(w http.ResponseWriter, r *http.Request) {
	write(w, r, func(in app.UpdateBatchInput) (app.Result, error) {
		return h.svc.UpdateBatch(r.Context(), user(r), chi.URLParam(r, "batchID"), in)
	})
}

func (h *handlers) snapshot(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Snapshot(r.Context(), user(r), chi.URLParam(r, "batchID"))
	respond(w, r, v, err)
}

func (h *handlers) brewAgain(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.BrewAgainDraft(r.Context(), user(r), chi.URLParam(r, "batchID"))
	respond(w, r, v, err)
}

func (h *handlers) reference(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Reference(r.Context(), user(r), chi.URLParam(r, "batchID"))
	respond(w, r, v, err)
}

func (h *handlers) createBottles(w http.ResponseWriter, r *http.Request) {
	key, err := idemKey(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	write(w, r, func(in app.CreateBottlesInput) (app.Result, error) {
		return h.svc.CreateBottles(r.Context(), user(r), chi.URLParam(r, "batchID"), key, in)
	})
}

func (h *handlers) listLogs(w http.ResponseWriter, r *http.Request) {
	bottleID, err := quuid(r, "bottleId")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	limit, err := qlimit(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	v, err := h.svc.ListLogs(r.Context(), user(r), chi.URLParam(r, "batchID"), bottleID, r.URL.Query().Get("cursor"), limit)
	respond(w, r, v, err)
}

func (h *handlers) createLogs(w http.ResponseWriter, r *http.Request) {
	key, err := idemKey(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	write(w, r, func(in app.CreateLogInput) (app.Result, error) {
		return h.svc.CreateLogs(r.Context(), user(r), chi.URLParam(r, "batchID"), key, in)
	})
}

func (h *handlers) finishBatch(w http.ResponseWriter, r *http.Request) {
	key, err := idemKey(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	write(w, r, func(in app.FinishInput) (app.Result, error) {
		return h.svc.FinishBatch(r.Context(), user(r), chi.URLParam(r, "batchID"), key, in)
	})
}

func (h *handlers) discardBatch(w http.ResponseWriter, r *http.Request) {
	write(w, r, func(in app.DiscardInput) (app.Result, error) {
		return h.svc.DiscardBatch(r.Context(), user(r), chi.URLParam(r, "batchID"), in)
	})
}

func (h *handlers) archive(archived bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res, err := h.svc.SetBatchArchived(r.Context(), user(r), chi.URLParam(r, "batchID"), archived)
		result(w, r, res, err)
	}
}

func (h *handlers) saveBatchAsRecipe(w http.ResponseWriter, r *http.Request) {
	key, err := idemKey(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	write(w, r, func(in app.SaveAsRecipeInput) (app.Result, error) {
		return h.svc.SaveBatchAsRecipe(r.Context(), user(r), chi.URLParam(r, "batchID"), key, in)
	})
}

// ---------------------------------------------------------------- bottles and logs

func (h *handlers) previewBottle(w http.ResponseWriter, r *http.Request) {
	var in app.BottlePreviewInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	v, err := h.svc.PreviewBottle(r.Context(), user(r), in)
	respond(w, r, v, err)
}

func (h *handlers) updateBottle(w http.ResponseWriter, r *http.Request) {
	write(w, r, func(in app.UpdateBottleInput) (app.Result, error) {
		return h.svc.UpdateBottle(r.Context(), user(r), chi.URLParam(r, "bottleID"), in)
	})
}

func (h *handlers) finishBottle(w http.ResponseWriter, r *http.Request) {
	key, err := idemKey(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	write(w, r, func(in app.FinishInput) (app.Result, error) {
		return h.svc.FinishBottle(r.Context(), user(r), chi.URLParam(r, "bottleID"), key, in)
	})
}

func (h *handlers) saveBottleAsRecipe(w http.ResponseWriter, r *http.Request) {
	key, err := idemKey(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	write(w, r, func(in app.SaveAsRecipeInput) (app.Result, error) {
		return h.svc.SaveBottleAsRecipe(r.Context(), user(r), chi.URLParam(r, "bottleID"), key, in)
	})
}

func (h *handlers) deleteLog(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeleteLog(r.Context(), user(r), chi.URLParam(r, "logID")); err != nil {
		httpx.Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- recipes

func (h *handlers) listRecipes(w http.ResponseWriter, r *http.Request) {
	var q app.RecipeQuery
	var err error
	if q.Q, err = qstr(r, "q", 100); err == nil {
		if q.Type, err = qenum(r, "type", "kombucha", "water_kefir", "custom"); err == nil {
			if q.Scope, err = qenum(r, "scope", "batch", "bottle"); err == nil {
				if q.Archived, err = qbool(r, "archived"); err == nil {
					q.Limit, err = qlimit(r)
				}
			}
		}
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	q.Cursor = r.URL.Query().Get("cursor")
	v, err := h.svc.ListRecipes(r.Context(), user(r), q)
	respond(w, r, v, err)
}

func (h *handlers) getRecipe(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.GetRecipe(r.Context(), user(r), chi.URLParam(r, "recipeID"))
	respond(w, r, v, err)
}

func (h *handlers) updateRecipe(w http.ResponseWriter, r *http.Request) {
	write(w, r, func(in app.UpdateRecipeInput) (app.Result, error) {
		return h.svc.UpdateRecipe(r.Context(), user(r), chi.URLParam(r, "recipeID"), in)
	})
}

func (h *handlers) listVersions(w http.ResponseWriter, r *http.Request) {
	limit, err := qlimit(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	v, err := h.svc.ListRecipeVersions(r.Context(), user(r), chi.URLParam(r, "recipeID"), r.URL.Query().Get("cursor"), limit)
	respond(w, r, v, err)
}

func (h *handlers) getVersionByNo(w http.ResponseWriter, r *http.Request) {
	no, err := strconv.Atoi(chi.URLParam(r, "versionNo"))
	if err != nil || no < 1 {
		httpx.Error(w, r, apperr.NotFound("Recipe version"))
		return
	}
	v, err := h.svc.GetRecipeVersionByNo(r.Context(), user(r), chi.URLParam(r, "recipeID"), no)
	respond(w, r, v, err)
}

func (h *handlers) getVersion(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.GetRecipeVersion(r.Context(), user(r), chi.URLParam(r, "versionID"))
	respond(w, r, v, err)
}

func (h *handlers) diffVersions(w http.ResponseWriter, r *http.Request) {
	from, err1 := qint(r, "from", 0)
	to, err2 := qint(r, "to", 0)
	if err1 != nil || err2 != nil {
		httpx.Error(w, r, apperr.BadRequest("from and to must be version numbers."))
		return
	}
	v, err := h.svc.DiffRecipeVersions(r.Context(), user(r), chi.URLParam(r, "recipeID"), from, to)
	respond(w, r, v, err)
}

func (h *handlers) recipeBatches(w http.ResponseWriter, r *http.Request) {
	limit, err := qlimit(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	v, err := h.svc.ListRecipeBatches(r.Context(), user(r), chi.URLParam(r, "recipeID"), r.URL.Query().Get("cursor"), limit)
	respond(w, r, v, err)
}

// ---------------------------------------------------------------- compare and reminders

func (h *handlers) compare(w http.ResponseWriter, r *http.Request) {
	var in app.CompareInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	v, err := h.svc.Compare(r.Context(), user(r), in)
	respond(w, r, v, err)
}

func (h *handlers) listReminders(w http.ResponseWriter, r *http.Request) {
	var q app.ReminderQuery
	var err error
	if q.Status, err = qenum(r, "status", "pending", "done", "snoozed", "dismissed"); err == nil {
		if q.BatchID, err = quuid(r, "batchId"); err == nil {
			if q.From, err = qtime(r, "from"); err == nil {
				q.To, err = qtime(r, "to")
			}
		}
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	v, err := h.svc.ListReminders(r.Context(), user(r), q)
	respond(w, r, v, err)
}

func (h *handlers) updateReminder(w http.ResponseWriter, r *http.Request) {
	write(w, r, func(in app.UpdateReminderInput) (app.Result, error) {
		return h.svc.UpdateReminder(r.Context(), user(r), chi.URLParam(r, "reminderID"), in)
	})
}
