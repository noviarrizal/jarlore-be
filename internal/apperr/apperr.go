// Package apperr defines the errors the application returns to clients. Each one
// becomes an RFC 9457 problem with a stable code (docs/architecture/API_CONTRACT.md §5).
package apperr

import (
	"errors"
	"fmt"
	"net/http"
)

// FieldError points at one invalid field, e.g. "ingredients[2].amount".
type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error is an error that is safe to show to the client.
type Error struct {
	Status int
	Code   string
	Title  string
	Detail string
	Fields []FieldError
	cause  error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Detail, e.cause)
	}
	return e.Code + ": " + e.Detail
}

func (e *Error) Unwrap() error { return e.cause }

// As returns the *Error inside err, if any.
func As(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

func newErr(status int, code, title, detail string) *Error {
	return &Error{Status: status, Code: code, Title: title, Detail: detail}
}

func BadRequest(detail string) *Error {
	return newErr(http.StatusBadRequest, "bad_request", "Bad request", detail)
}

func InvalidCursor() *Error {
	return newErr(http.StatusBadRequest, "invalid_cursor", "Bad request", "The cursor doesn't belong to this query. Start from the first page.")
}

func Unauthenticated(detail string) *Error {
	return newErr(http.StatusUnauthorized, "unauthenticated", "Sign in required", detail)
}

func EntitlementRequired(detail string, fields ...FieldError) *Error {
	e := newErr(http.StatusPaymentRequired, "entitlement_required", "Upgrade required", detail)
	e.Fields = fields
	return e
}

func Forbidden(code, detail string) *Error {
	return newErr(http.StatusForbidden, code, "Not allowed", detail)
}

// NotFound is also returned for other users' resources, so ids can't be probed.
func NotFound(what string) *Error {
	return newErr(http.StatusNotFound, "not_found", "Not found", what+" not found.")
}

func Conflict(code, detail string) *Error {
	return newErr(http.StatusConflict, code, "Conflict", detail)
}

func InvalidState(detail string) *Error {
	return Conflict("invalid_state_transition", detail)
}

func Validation(fields ...FieldError) *Error {
	e := newErr(http.StatusUnprocessableEntity, "validation_failed", "Check the highlighted fields", "Some fields are invalid.")
	e.Fields = fields
	if len(fields) == 1 {
		e.Detail = fields[0].Message
	}
	return e
}

func FieldImmutable(field, message string) *Error {
	e := newErr(http.StatusUnprocessableEntity, "field_immutable", "Field can't be changed", message)
	e.Fields = []FieldError{{Field: field, Code: "immutable", Message: message}}
	return e
}

func IdempotencyReused() *Error {
	return newErr(http.StatusUnprocessableEntity, "idempotency_key_reused", "Idempotency key reused",
		"This Idempotency-Key was already used for a different request.")
}

func IdempotencyInProgress() *Error {
	return Conflict("idempotency_in_progress", "The first request with this Idempotency-Key is still running. Retry shortly.")
}

func Unavailable(detail string, cause error) *Error {
	e := newErr(http.StatusServiceUnavailable, "unavailable", "Service unavailable", detail)
	e.cause = cause
	return e
}

// Wrap attaches an internal cause (logged, never sent) to a client error.
func Wrap(e *Error, cause error) *Error {
	c := *e
	c.cause = cause
	return &c
}

// Fields collects field errors while validating a request.
type Fields []FieldError

func (f *Fields) Add(field, code, message string) {
	*f = append(*f, FieldError{Field: field, Code: code, Message: message})
}

// Err returns a validation error when anything was added, else nil.
func (f Fields) Err() error {
	if len(f) == 0 {
		return nil
	}
	return Validation(f...)
}
