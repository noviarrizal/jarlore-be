// Package db owns the connection pool and the per-request transaction that
// carries the authenticated user into PostgreSQL for row level security.
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"jarlore/internal/db/sqlc"
)

// PoolOptions are tuned for Neon behind PgBouncer (transaction mode) and a
// scale-to-zero API: no idle connections are kept, nothing pings the database in
// the background, so both the API and the Neon compute can go idle.
type PoolOptions struct {
	MaxConns      int32
	QueryExecMode string // cache_statement | exec
}

func NewPool(ctx context.Context, url string, o PoolOptions) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, errors.New("DATABASE_URL is not a valid PostgreSQL connection string")
	}
	cfg.MinConns = 0
	cfg.MaxConns = o.MaxConns
	cfg.MaxConnIdleTime = time.Minute // let Neon's 5-minute idle suspend happen
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnLifetimeJitter = 2 * time.Minute
	cfg.HealthCheckPeriod = time.Minute // only closes expired idle conns; sends no queries
	// A suspended Neon compute usually resumes in well under a second; allow headroom.
	cfg.ConnConfig.ConnectTimeout = 10 * time.Second
	cfg.ConnConfig.RuntimeParams["application_name"] = "jarlore-api"
	switch o.QueryExecMode {
	case "exec":
		// Fallback for poolers without protocol-level prepared statement support.
		cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	default:
		// Neon's PgBouncer supports protocol-level prepared statements
		// (max_prepared_statements), so pgx's default statement cache works.
		cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	}
	// No connection is opened here; cmd/api opens the first one for the role
	// guard (GuardRuntimeRole) before it starts listening.
	return pgxpool.NewWithConfig(ctx, cfg)
}

// Store runs application work inside transactions.
type Store struct {
	Pool *pgxpool.Pool
}

// UserTx runs fn in one transaction scoped to userID. The user id is set with
// set_config(..., is_local = true), so it disappears at commit or rollback and
// can never leak to the next request that gets this pooled connection. This is
// also the only form of session state that is safe with transaction pooling.
func (s *Store) UserTx(ctx context.Context, userID string, fn func(q *sqlc.Queries) error) error {
	return s.tx(ctx, userID, pgx.TxOptions{}, fn)
}

// UserReadTx is UserTx for read-only work.
func (s *Store) UserReadTx(ctx context.Context, userID string, fn func(q *sqlc.Queries) error) error {
	return s.tx(ctx, userID, pgx.TxOptions{AccessMode: pgx.ReadOnly}, fn)
}

func (s *Store) tx(ctx context.Context, userID string, opts pgx.TxOptions, fn func(q *sqlc.Queries) error) error {
	if userID == "" {
		return errors.New("db: user transaction without a user id")
	}
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var tx pgx.Tx
		tx, err = s.Pool.BeginTx(ctx, opts)
		if err == nil {
			_, err = tx.Exec(ctx, "SELECT set_config('jarlore.user_id', $1, true)", userID)
			if err == nil {
				err = fn(sqlc.New(tx))
			}
			if err == nil {
				err = tx.Commit(ctx)
			} else {
				_ = tx.Rollback(context.WithoutCancel(ctx))
			}
		}
		// Retry once when the connection was dropped before anything was sent,
		// e.g. a connection that died while Neon suspended its compute.
		if err == nil || attempt == 1 || !pgconn.SafeToRetry(err) || ctx.Err() != nil {
			break
		}
	}
	return err
}

// Ping checks that the database answers. Used by the readiness probe only.
func (s *Store) Ping(ctx context.Context) error {
	var one int
	if err := s.Pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("db: ping: %w", err)
	}
	return nil
}
