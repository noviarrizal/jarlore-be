package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RoleReport describes the database identity the API runs as.
type RoleReport struct {
	User string
	// Problems lists every reason the identity is unsafe; empty means safe.
	Problems []string
}

// roleCheckSQL inspects current_user. Row level security protects users from each
// other only if the runtime role cannot skip it, and PostgreSQL skips it for:
//   - superusers and roles with BYPASSRLS, including any such role current_user
//     can SET ROLE to (attributes are not inherited, but SET ROLE reaches them);
//   - the owner of a table, and any role with the owner's privileges, because the
//     policies are not FORCEd (the migration role must manage the tables).
const roleCheckSQL = `
SELECT current_user::text,
       r.rolsuper,
       r.rolbypassrls,
       COALESCE((SELECT string_agg(p.rolname, ',' ORDER BY p.rolname)
                   FROM pg_roles p
                  WHERE p.oid <> r.oid
                    AND (p.rolsuper OR p.rolbypassrls)
                    AND pg_has_role(r.oid, p.oid, 'MEMBER')), ''),
       COALESCE((SELECT string_agg(c.relname, ',' ORDER BY c.relname)
                   FROM pg_class c
                   JOIN pg_namespace n ON n.oid = c.relnamespace
                  WHERE n.nspname = 'public'
                    AND c.relkind IN ('r', 'p')
                    AND c.relrowsecurity
                    AND pg_has_role(r.oid, c.relowner, 'MEMBER')), '')
  FROM pg_roles r
 WHERE r.rolname = current_user`

// InspectRole reports whether the pool's role is safe to serve user requests.
// Errors are returned unsanitized; GuardRuntimeRole sanitizes them.
func InspectRole(ctx context.Context, pool *pgxpool.Pool) (RoleReport, error) {
	var (
		rep                  RoleReport
		super, bypass        bool
		elevated, ownedTable string
	)
	if err := pool.QueryRow(ctx, roleCheckSQL).Scan(&rep.User, &super, &bypass, &elevated, &ownedTable); err != nil {
		return rep, err
	}
	if super {
		rep.Problems = append(rep.Problems, "it is a superuser")
	}
	if bypass {
		rep.Problems = append(rep.Problems, "it has BYPASSRLS")
	}
	if elevated != "" {
		rep.Problems = append(rep.Problems, "it can SET ROLE to a superuser or BYPASSRLS role ("+elevated+")")
	}
	if ownedTable != "" {
		rep.Problems = append(rep.Problems, "it owns, or has the privileges of the owner of, tables protected by row level security ("+truncateList(ownedTable, 5)+")")
	}
	return rep, nil
}

// ErrUnsafeRole is returned when the runtime role could bypass row level security.
var ErrUnsafeRole = errors.New("unsafe runtime database role")

// GuardRuntimeRole fails unless the pool connects as a role that is subject to row
// level security. It retries connection errors (a suspended Neon compute is
// resuming) until ctx is done. Nothing it returns contains connection strings or
// passwords.
func GuardRuntimeRole(ctx context.Context, pool *pgxpool.Pool) (RoleReport, error) {
	var (
		rep RoleReport
		err error
	)
	for wait := 250 * time.Millisecond; ; wait *= 2 {
		rep, err = InspectRole(ctx, pool)
		if err == nil || ctx.Err() != nil || !isConnectError(err) {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(min(wait, 4*time.Second)):
		}
	}
	if err != nil {
		return rep, fmt.Errorf("db: inspect runtime role: %w", sanitize(err))
	}
	if len(rep.Problems) > 0 {
		return rep, fmt.Errorf("%w %q: %s. DATABASE_URL must use the restricted API role (a member of jarlore_app, e.g. jarlore_api), never the migration owner; see docs/deployment/NEON_SETUP.md",
			ErrUnsafeRole, rep.User, strings.Join(rep.Problems, "; "))
	}
	return rep, nil
}

func isConnectError(err error) bool {
	var ce *pgconn.ConnectError
	return errors.As(err, &ce) || pgconn.SafeToRetry(err)
}

// sanitize drops connection details (host, user, database) from pgx errors.
func sanitize(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return errors.New("timed out reaching the database")
	}
	if isConnectError(err) {
		return errors.New("could not connect to the database; check DATABASE_URL and network access")
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s (SQLSTATE %s)", pe.Message, pe.Code)
	}
	return err
}

func truncateList(csv string, n int) string {
	parts := strings.Split(csv, ",")
	if len(parts) <= n {
		return csv
	}
	return strings.Join(parts[:n], ",") + fmt.Sprintf(" and %d more", len(parts)-n)
}
