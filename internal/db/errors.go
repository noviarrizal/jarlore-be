package db

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"jarlore/internal/apperr"
)

// IsNoRows reports a query that matched nothing (absent, or another user's row).
func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// MapError turns database errors into client errors. Integrity triggers and
// constraints are the last line of defence: if the application's own checks
// missed something, the client still gets a precise, safe error and never the
// PostgreSQL message. Unknown errors are returned unchanged (500 upstream).
func MapError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := apperr.As(err); ok {
		return err
	}
	if IsNoRows(err) {
		return apperr.Wrap(apperr.NotFound("Resource"), err)
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		if pgconn.Timeout(err) || isConnErr(err) {
			return apperr.Unavailable("The database is waking up or unreachable. Try again in a moment.", err)
		}
		return err
	}
	switch pg.Code {
	case "JL001":
		return apperr.Wrap(apperr.Conflict("recipe_version_immutable", "Recipe versions can't be changed. Save a new version instead."), err)
	case "JL002":
		return apperr.Wrap(apperr.FieldImmutable("recipeVersion", "The recipe snapshot of a batch or bottle never changes."), err)
	case "JL003":
		return apperr.Wrap(apperr.InvalidState("This record is closed and kept as history."), err)
	case "JL004":
		return apperr.Wrap(apperr.Conflict("append_only", "History entries can't be edited."), err)
	case "JL005", "23503": // foreign key: the referenced row isn't the caller's
		return apperr.Wrap(apperr.NotFound("Referenced resource"), err)
	case "23505":
		switch {
		case pg.ConstraintName == "bottles_batch_id_label_key":
			return apperr.Wrap(apperr.Validation(apperr.FieldError{Field: "label", Code: "duplicate", Message: "Another bottle in this batch already has this label."}), err)
		case pg.ConstraintName == "idempotency_keys_pkey":
			return apperr.Wrap(apperr.IdempotencyInProgress(), err)
		case strings.HasPrefix(pg.ConstraintName, "measurement_definitions"):
			return apperr.Wrap(apperr.Conflict("duplicate_key", "A measurement with this key already exists."), err)
		}
		return apperr.Wrap(apperr.Conflict("duplicate_key", "This already exists."), err)
	case "23514", "22P02", "22003", "23502":
		return apperr.Wrap(apperr.Validation(apperr.FieldError{Field: "", Code: "invalid", Message: "A value is out of range or invalid."}), err)
	case "40001", "40P01":
		return apperr.Wrap(apperr.Conflict("concurrent_update", "Someone else changed this at the same time. Try again."), err)
	case "57P01", "57P02", "57P03", "08000", "08003", "08006", "08001", "08004":
		return apperr.Unavailable("The database is restarting. Try again in a moment.", err)
	}
	return err
}

func isConnErr(err error) bool {
	var ce *pgconn.ConnectError
	return errors.As(err, &ce)
}
