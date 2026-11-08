// Package app implements Jarlore's use cases. Every method runs in one database
// transaction scoped to the authenticated user (db.Store.UserTx), checks
// ownership explicitly and returns read models from models.go.
package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"jarlore/internal/apperr"
	"jarlore/internal/auth"
	"jarlore/internal/db"
	"jarlore/internal/db/sqlc"
)

type Service struct {
	store         *db.Store
	now           func() time.Time
	cursorSecret  []byte
	betaProForAll bool

	ensuredMu sync.Mutex
	ensured   map[string]time.Time // users whose profile row exists, to skip the upsert
}

type Options struct {
	CursorSecret  []byte
	BetaProForAll bool
	Now           func() time.Time
}

func New(store *db.Store, o Options) *Service {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, now: func() time.Time { return now().UTC() }, cursorSecret: o.CursorSecret, betaProForAll: o.BetaProForAll, ensured: map[string]time.Time{}}
}

// Result is a response body ready to send. Writes go through idempotency
// handling, which stores and replays this exact body.
type Result struct {
	Status   int
	Body     json.RawMessage
	Replayed bool
}

// write runs a state-changing use case in a user transaction.
func (s *Service) write(ctx context.Context, u auth.User, op, idemKey string, req any, status int, fn func(q *sqlc.Queries) (any, error)) (Result, error) {
	var res Result
	err := s.store.UserTx(ctx, u.ID, func(q *sqlc.Queries) error {
		var hash string
		if idemKey != "" {
			raw, _ := json.Marshal(req)
			sum := sha256.Sum256(append([]byte(op+"\n"), raw...))
			hash = base64.RawURLEncoding.EncodeToString(sum[:])
			prev, err := q.GetIdempotencyKey(ctx, sqlc.GetIdempotencyKeyParams{UserID: u.ID, Key: idemKey})
			switch {
			case err == nil:
				if prev.RequestHash != hash || prev.Operation != op {
					return apperr.IdempotencyReused()
				}
				res = Result{Status: int(prev.StatusCode), Body: prev.Response, Replayed: true}
				return nil
			case !db.IsNoRows(err):
				return err
			}
			if err := q.DeleteExpiredIdempotencyKey(ctx, sqlc.DeleteExpiredIdempotencyKeyParams{UserID: u.ID, Key: idemKey}); err != nil {
				return err
			}
		}
		out, err := fn(q)
		if err != nil {
			return err
		}
		body, err := json.Marshal(out)
		if err != nil {
			return err
		}
		res = Result{Status: status, Body: body}
		if idemKey != "" {
			// Inserted in the same transaction as the write: a concurrent retry with the
			// same key waits on this row and then sees the stored response or a conflict.
			return q.InsertIdempotencyKey(ctx, sqlc.InsertIdempotencyKeyParams{
				UserID: u.ID, Key: idemKey, Operation: op, RequestHash: hash, StatusCode: int32(status), Response: body,
			})
		}
		return nil
	})
	return res, db.MapError(err)
}

// read runs a read-only use case.
func (s *Service) read(ctx context.Context, u auth.User, fn func(q *sqlc.Queries) error) error {
	return db.MapError(s.store.UserReadTx(ctx, u.ID, fn))
}

// EnsureProfile creates the profile on the first request of a user. The result is
// remembered for an hour per instance so normal requests skip the upsert.
func (s *Service) EnsureProfile(ctx context.Context, u auth.User) error {
	s.ensuredMu.Lock()
	t, ok := s.ensured[u.ID]
	s.ensuredMu.Unlock()
	if ok && s.now().Sub(t) < time.Hour {
		return nil
	}
	err := s.store.UserTx(ctx, u.ID, func(q *sqlc.Queries) error {
		_, err := q.EnsureProfile(ctx, sqlc.EnsureProfileParams{ID: u.ID, Email: u.Email})
		if db.IsNoRows(err) { // unchanged row: DO UPDATE ... WHERE matched nothing
			return nil
		}
		return err
	})
	if err != nil {
		return db.MapError(err)
	}
	s.ensuredMu.Lock()
	if len(s.ensured) > 10000 {
		s.ensured = map[string]time.Time{}
	}
	s.ensured[u.ID] = s.now()
	s.ensuredMu.Unlock()
	return nil
}

// ---------------------------------------------------------------- entitlements

const (
	freeCompareItems       = 2
	proCompareItems        = 6
	freeCustomMeasurements = 3
)

func (s *Service) entitlements(ctx context.Context, q *sqlc.Queries, userID string) (Entitlements, error) {
	var e Entitlements
	e.Plan, e.Source = "free", "default"
	if _, err := q.GetActiveSubscription(ctx, userID); err == nil {
		e.Plan, e.Source = "pro", "subscription"
	} else if !db.IsNoRows(err) {
		return e, err
	} else if s.betaProForAll {
		e.Plan, e.Source = "pro", "beta"
	}
	if e.Plan == "pro" {
		e.Limits.CompareItems = proCompareItems
	} else {
		e.Limits.CompareItems = freeCompareItems
		n := freeCustomMeasurements
		e.Limits.CustomMeasurements = &n
	}
	return e, nil
}

func (s *Service) Entitlements(ctx context.Context, u auth.User) (Entitlements, error) {
	var e Entitlements
	err := s.read(ctx, u, func(q *sqlc.Queries) (err error) {
		e, err = s.entitlements(ctx, q, u.ID)
		return err
	})
	return e, err
}

// ---------------------------------------------------------------- cursors

// cursor is opaque to clients: base64url(JSON) + "." + HMAC. It is bound to the
// query (filters and sort) through scope, so it can't be replayed elsewhere.
type cursor struct {
	Scope string     `json:"s"`
	Time  *time.Time `json:"t,omitempty"`
	Num   *int32     `json:"n,omitempty"`
	ID    string     `json:"i"`
}

func (s *Service) encodeCursor(c cursor) *string {
	raw, _ := json.Marshal(c)
	mac := hmac.New(sha256.New, s.cursorSecret)
	mac.Write(raw)
	out := base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return &out
}

func (s *Service) decodeCursor(token, scope string) (*cursor, error) {
	if token == "" {
		return nil, nil
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, apperr.InvalidCursor()
	}
	raw, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	sig, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	if err1 != nil || err2 != nil {
		return nil, apperr.InvalidCursor()
	}
	mac := hmac.New(sha256.New, s.cursorSecret)
	mac.Write(raw)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, apperr.InvalidCursor()
	}
	var c cursor
	if err := json.Unmarshal(raw, &c); err != nil || c.Scope != scope || !isUUID(c.ID) {
		return nil, apperr.InvalidCursor()
	}
	return &c, nil
}

func pageLimit(limit int) int32 {
	if limit <= 0 {
		return 20
	}
	if limit > 100 {
		return 100
	}
	return int32(limit)
}

// ---------------------------------------------------------------- helpers

func ptr[T any](v T) *T { return &v }

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("app: marshal %T: %v", v, err)) // only our own value types are marshalled
	}
	return b
}

func unmarshalInto[T any](raw []byte, dst *T) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("app: decode stored %T: %w", *dst, err)
	}
	return nil
}

func notFoundIf(err error, what string) error {
	if db.IsNoRows(err) {
		return apperr.NotFound(what)
	}
	return err
}
