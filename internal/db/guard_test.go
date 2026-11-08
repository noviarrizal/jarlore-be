package db_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"jarlore/internal/db"
	"jarlore/internal/testdb"
)

// The API must refuse to start with a database role that could skip row level
// security. Each case connects as a different role to the same migrated database.
func TestRuntimeRoleGuard(t *testing.T) {
	d := testdb.New(t)
	admin := d.Admin(t)
	ctx := context.Background()

	var adminIsSuper bool
	if err := admin.QueryRow(ctx, "SELECT rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&adminIsSuper); err != nil {
		t.Fatal(err)
	}
	// Extra roles for the unsafe cases (test-only names and password).
	setup := []string{
		fmt.Sprintf(`DO $$ BEGIN
		  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'jarlore_test_owner_member') THEN
		    CREATE ROLE jarlore_test_owner_member LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '%s';
		  END IF;
		END $$`, testdb.Password),
		"GRANT " + testdb.OwnerRole + " TO jarlore_test_owner_member",
	}
	if adminIsSuper {
		setup = append(setup, fmt.Sprintf(`DO $$ BEGIN
		  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'jarlore_test_bypass') THEN
		    CREATE ROLE jarlore_test_bypass LOGIN NOSUPERUSER BYPASSRLS PASSWORD '%s';
		  END IF;
		  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'jarlore_test_super') THEN
		    CREATE ROLE jarlore_test_super LOGIN SUPERUSER PASSWORD '%s';
		  END IF;
		  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'jarlore_test_can_bypass') THEN
		    CREATE ROLE jarlore_test_can_bypass LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '%s';
		  END IF;
		END $$`, testdb.Password, testdb.Password, testdb.Password),
			"GRANT jarlore_test_bypass TO jarlore_test_can_bypass",
			"GRANT CONNECT ON DATABASE "+d.Name+" TO jarlore_test_bypass, jarlore_test_super, jarlore_test_can_bypass")
	}
	for _, s := range setup {
		if _, err := admin.Exec(ctx, s); err != nil {
			t.Fatalf("setup %q: %v", s[:min(40, len(s))], err)
		}
	}

	cases := []struct {
		name, role string
		wantSafe   bool
		wantWord   string
		needsSuper bool
	}{
		{"restricted API role", testdb.APIRole, true, "", false},
		{"migration owner", testdb.OwnerRole, false, "owns", false},
		{"member of the owner role", "jarlore_test_owner_member", false, "owns", false},
		{"BYPASSRLS role", "jarlore_test_bypass", false, "BYPASSRLS", true},
		{"superuser", "jarlore_test_super", false, "superuser", true},
		{"can SET ROLE to a BYPASSRLS role", "jarlore_test_can_bypass", false, "SET ROLE", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.needsSuper && !adminIsSuper {
				t.Skip("JARLORE_TEST_DATABASE_URL is not a superuser; cannot create this role")
			}
			pool, err := db.NewPool(ctx, d.URLAs(t, c.role), db.PoolOptions{MaxConns: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			gctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			rep, err := db.GuardRuntimeRole(gctx, pool)
			if c.wantSafe {
				if err != nil {
					t.Fatalf("want safe, got %v", err)
				}
				return
			}
			if !errors.Is(err, db.ErrUnsafeRole) {
				t.Fatalf("want ErrUnsafeRole, got %v (report %+v)", err, rep)
			}
			if !strings.Contains(err.Error(), c.wantWord) {
				t.Errorf("error %q does not mention %q", err, c.wantWord)
			}
			if strings.Contains(err.Error(), testdb.Password) || strings.Contains(err.Error(), "postgres://") {
				t.Errorf("error leaks connection details: %q", err)
			}
		})
	}

	t.Run("unreachable database gives a sanitized error", func(t *testing.T) {
		pool, err := db.NewPool(ctx, "postgres://someone:secret-pw@127.0.0.1:1/nope?sslmode=disable&connect_timeout=1", db.PoolOptions{MaxConns: 1})
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		gctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		_, err = db.GuardRuntimeRole(gctx, pool)
		if err == nil {
			t.Fatal("want an error")
		}
		if msg := err.Error(); strings.Contains(msg, "secret-pw") || strings.Contains(msg, "someone") || strings.Contains(msg, "127.0.0.1") {
			t.Errorf("error leaks connection details: %q", msg)
		}
	})
}
