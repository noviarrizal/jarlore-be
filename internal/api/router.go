// Package api is the HTTP layer: routing, request decoding and response writing.
// Handlers are thin; every rule lives in internal/app and internal/domain.
package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"jarlore/internal/app"
	"jarlore/internal/apperr"
	"jarlore/internal/auth"
	"jarlore/internal/httpx"
)

type Deps struct {
	Service         *app.Service
	Verifier        *auth.Verifier
	Ready           func(ctx context.Context) error
	FrontendOrigins []string
	RequestTimeout  time.Duration
	OpenAPI         []byte
}

func routePattern(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
		return rc.RoutePattern()
	}
	return "unmatched"
}

func NewRouter(d Deps) http.Handler {
	h := &handlers{svc: d.Service}
	r := chi.NewRouter()
	r.Use(requestID, logRequests, recoverPanics, securityHeaders, cors(d.FrontendOrigins), timeout(d.RequestTimeout))

	r.NotFound(func(w http.ResponseWriter, r *http.Request) { httpx.Error(w, r, apperr.NotFound("Route")) })
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, &apperr.Error{Status: http.StatusMethodNotAllowed, Code: "method_not_allowed", Title: "Method not allowed", Detail: r.Method + " is not supported here."})
	})

	// Liveness: the process answers. Readiness: the database answers too.
	r.Get("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		if err := d.Ready(ctx); err != nil {
			httpx.Error(w, r, apperr.Unavailable("The database is not reachable yet.", err))
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	r.Get("/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write(d.OpenAPI)
	})

	r.Route("/v1", func(r chi.Router) {
		r.Use(authenticate(d.Verifier, d.Service))

		r.Get("/me", h.getMe)
		r.Patch("/me", h.updateMe)
		r.Get("/entitlements", h.entitlements)
		r.Get("/fermentation-types", h.fermentationTypes)
		r.Get("/measurement-definitions", h.measurementDefinitions)
		r.Post("/measurement-definitions", h.createMeasurementDefinition)
		r.Get("/today", h.today)

		r.Get("/batches", h.listBatches)
		r.Post("/batches", h.createBatch)
		r.Post("/batches/diff-preview", h.previewBatch)
		r.Route("/batches/{batchID}", func(r chi.Router) {
			r.Use(uuidParam("batchID", "Batch"))
			r.Get("/", h.getBatch)
			r.Patch("/", h.updateBatch)
			r.Get("/snapshot", h.snapshot)
			r.Get("/brew-again", h.brewAgain)
			r.Get("/reference", h.reference)
			r.Post("/bottles", h.createBottles)
			r.Get("/logs", h.listLogs)
			r.Post("/logs", h.createLogs)
			r.Post("/finish", h.finishBatch)
			r.Post("/discard", h.discardBatch)
			r.Post("/archive", h.archive(true))
			r.Post("/unarchive", h.archive(false))
			r.Post("/save-as-recipe", h.saveBatchAsRecipe)
		})

		r.Post("/bottles/diff-preview", h.previewBottle)
		r.Route("/bottles/{bottleID}", func(r chi.Router) {
			r.Use(uuidParam("bottleID", "Bottle"))
			r.Patch("/", h.updateBottle)
			r.Post("/finish", h.finishBottle)
			r.Post("/save-as-recipe", h.saveBottleAsRecipe)
		})

		r.With(uuidParam("logID", "Log entry")).Delete("/logs/{logID}", h.deleteLog)

		r.Get("/recipes", h.listRecipes)
		r.Route("/recipes/{recipeID}", func(r chi.Router) {
			r.Use(uuidParam("recipeID", "Recipe"))
			r.Get("/", h.getRecipe)
			r.Patch("/", h.updateRecipe)
			r.Get("/versions", h.listVersions)
			r.Get("/versions/{versionNo}", h.getVersionByNo)
			r.Get("/diff", h.diffVersions)
			r.Get("/batches", h.recipeBatches)
		})
		r.With(uuidParam("versionID", "Recipe version")).Get("/recipe-versions/{versionID}", h.getVersion)

		r.Post("/compare", h.compare)

		r.Get("/reminders", h.listReminders)
		r.With(uuidParam("reminderID", "Reminder")).Patch("/reminders/{reminderID}", h.updateReminder)
	})
	return r
}

// uuidParam answers 404 for path ids that can't be ids (never 400, so the
// response for a malformed id equals the one for another user's id).
func uuidParam(name, what string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			v := chi.URLParam(r, name)
			if !app.IsUUID(v) {
				httpx.Error(w, r, apperr.NotFound(what))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ---------------------------------------------------------------- query helpers

func qstr(r *http.Request, key string, max int) (*string, error) {
	v := strings.TrimSpace(r.URL.Query().Get(key))
	if v == "" {
		return nil, nil
	}
	if len(v) > max {
		return nil, apperr.BadRequest(key + " is too long.")
	}
	return &v, nil
}

func qenum(r *http.Request, key string, allowed ...string) (*string, error) {
	v, _ := qstr(r, key, 64)
	if v == nil {
		return nil, nil
	}
	for _, a := range allowed {
		if *v == a {
			return v, nil
		}
	}
	return nil, apperr.BadRequest(key + " must be one of " + strings.Join(allowed, ", ") + ".")
}

func qbool(r *http.Request, key string) (bool, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, apperr.BadRequest(key + " must be true or false.")
	}
	return b, nil
}

func qint(r *http.Request, key string, def int) (int, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, apperr.BadRequest(key + " must be a whole number.")
	}
	return n, nil
}

func qlimit(r *http.Request) (int, error) {
	n, err := qint(r, "limit", 20)
	if err != nil || n < 1 || n > 100 {
		return 0, apperr.BadRequest("limit must be between 1 and 100.")
	}
	return n, nil
}

func qtime(r *http.Request, key string) (*time.Time, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, apperr.BadRequest(key + " must be an ISO 8601 timestamp.")
	}
	return &t, nil
}

func quuid(r *http.Request, key string) (*string, error) {
	v, _ := qstr(r, key, 64)
	if v != nil && !app.IsUUID(*v) {
		return nil, apperr.BadRequest(key + " is not a valid id.")
	}
	return v, nil
}

// idemKey reads the Idempotency-Key header (8..128 characters).
func idemKey(r *http.Request) (string, error) {
	k := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if k != "" && (len(k) < 8 || len(k) > 128) {
		return "", apperr.Validation(apperr.FieldError{Field: "Idempotency-Key", Code: "invalid", Message: "Idempotency-Key must be 8 to 128 characters."})
	}
	return k, nil
}

func writeResult(w http.ResponseWriter, res app.Result) {
	if res.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	if res.Status == http.StatusNoContent {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(res.Status)
	_, _ = w.Write(append(res.Body, '\n'))
}
