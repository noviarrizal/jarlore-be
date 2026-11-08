// Package httpx holds JSON and problem+json helpers shared by the handlers.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"jarlore/internal/apperr"
)

// MaxBody is the request body limit (API_CONTRACT.md §2).
const MaxBody = 64 << 10

type requestIDKey struct{}

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// JSON writes v with the given status.
func JSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		slog.Error("encode response", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// Problem is the RFC 9457 body.
type Problem struct {
	Type      string              `json:"type"`
	Title     string              `json:"title"`
	Status    int                 `json:"status"`
	Detail    string              `json:"detail,omitempty"`
	Code      string              `json:"code"`
	Errors    []apperr.FieldError `json:"errors,omitempty"`
	RequestID string              `json:"requestId"`
}

// Error writes err as problem+json. Unknown errors become a generic 500 and are
// logged with the request id; their text never reaches the client.
func Error(w http.ResponseWriter, r *http.Request, err error) {
	e, ok := apperr.As(err)
	if !ok {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			e = &apperr.Error{Status: http.StatusServiceUnavailable, Code: "unavailable", Title: "Service unavailable", Detail: "The request took too long. Try again."}
		} else {
			e = &apperr.Error{Status: http.StatusInternalServerError, Code: "internal_error", Title: "Something went wrong", Detail: "An unexpected error occurred."}
		}
	}
	rid := RequestID(r.Context())
	if e.Status >= 500 {
		slog.ErrorContext(r.Context(), "request failed", "request_id", rid, "code", e.Code, "err", err)
	} else if e.Unwrap() != nil {
		slog.DebugContext(r.Context(), "client error", "request_id", rid, "code", e.Code, "err", err)
	}
	p := Problem{
		Type:      "https://docs.jarlore.app/errors/" + e.Code,
		Title:     e.Title,
		Status:    e.Status,
		Detail:    e.Detail,
		Code:      e.Code,
		Errors:    e.Fields,
		RequestID: rid,
	}
	body, _ := json.Marshal(p)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(e.Status)
	_, _ = w.Write(append(body, '\n'))
}

// Decode reads a JSON body into dst. Unknown fields are rejected so a typo on a
// PATCH never silently does nothing.
func Decode(r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		return apperr.BadRequest("Send the body as application/json.")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil {
		return apperr.BadRequest("Couldn't read the request body.")
	}
	if len(body) > MaxBody {
		return &apperr.Error{Status: http.StatusRequestEntityTooLarge, Code: "payload_too_large", Title: "Request too large", Detail: "The request body is larger than 64 KB."}
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return apperr.BadRequest("The request body is empty.")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var se *json.SyntaxError
		var te *json.UnmarshalTypeError
		switch {
		case errors.As(err, &se), errors.Is(err, io.ErrUnexpectedEOF):
			return apperr.BadRequest("The body is not valid JSON.")
		case errors.As(err, &te):
			field := te.Field
			if field == "" {
				field = "body"
			}
			return apperr.Validation(apperr.FieldError{Field: field, Code: "invalid_type", Message: field + " has the wrong type."})
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			name := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
			return apperr.Validation(apperr.FieldError{Field: name, Code: "unknown_field", Message: name + " is not a field of this request."})
		}
		return apperr.Validation(apperr.FieldError{Field: "body", Code: "invalid", Message: "The body doesn't match the expected shape."})
	}
	if dec.More() {
		return apperr.BadRequest("The body must contain one JSON value.")
	}
	return nil
}

// Optional distinguishes "absent" from "null" in PATCH bodies.
type Optional[T any] struct {
	Set   bool
	Value *T
}

func (o *Optional[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(bytes.TrimSpace(b)) == "null" {
		o.Value = nil
		return nil
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}
