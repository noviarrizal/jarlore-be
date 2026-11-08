// Package testdb gives integration tests a real, freshly migrated PostgreSQL
// database set up like production on Neon:
//
//   - an owner role that is NOT a superuser (like neondb_owner) runs the migrations;
//   - the API connects as a separate login role that is only a member of jarlore_app,
//     so row level security and the restricted grants apply to every test query.
//
// Tests are skipped unless JARLORE_TEST_DATABASE_URL points at a server where the
// connecting role may create roles and databases (a local PostgreSQL superuser, or a
// throwaway Neon branch's owner). Each test package gets its own database, which is
// dropped at the end.
package testdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"jarlore/internal/db"
	"jarlore/internal/migrate"
)

const (
	ownerRole = "jarlore_test_owner"
	apiRole   = "jarlore_test_api"
	password  = "jarlore-test-only-password"
)

type DB struct {
	Name  string
	admin string        // JARLORE_TEST_DATABASE_URL, for URLAs
	Owner *pgxpool.Pool // migration/owner role: bypasses nothing, but owns the tables
	App   *pgxpool.Pool // runtime role: subject to RLS and grants
	Store *db.Store
}

// New creates and migrates a database for the calling test package.
func New(t testing.TB) *DB {
	t.Helper()
	admin := os.Getenv("JARLORE_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("JARLORE_TEST_DATABASE_URL is not set; skipping database integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("testdb: connect admin: %v", err)
	}
	defer conn.Close(ctx)

	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "jarlore_t_" + hex.EncodeToString(b[:])

	stmts := []string{
		fmt.Sprintf(`DO $$ BEGIN
		  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '%[1]s') THEN
		    CREATE ROLE %[1]s LOGIN NOSUPERUSER CREATEROLE PASSWORD '%[3]s';
		  END IF;
		  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '%[2]s') THEN
		    CREATE ROLE %[2]s LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '%[3]s';
		  END IF;
		END $$`, ownerRole, apiRole, password),
		fmt.Sprintf(`CREATE DATABASE %s OWNER %s`, name, ownerRole),
	}
	for _, s := range stmts {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("testdb: %v", err)
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c, err := pgx.Connect(ctx, admin)
		if err != nil {
			return
		}
		defer c.Close(ctx)
		_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})

	ownerURL := withUser(t, admin, ownerRole, name)
	sqldb, err := sql.Open("pgx", ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Up(ctx, sqldb); err != nil {
		t.Fatalf("testdb: migrate: %v", err)
	}
	_ = sqldb.Close()

	// Like the Neon setup: the API's login role is a member of jarlore_app.
	if _, err := conn.Exec(ctx, fmt.Sprintf("GRANT jarlore_app TO %s", apiRole)); err != nil {
		t.Fatalf("testdb: grant: %v", err)
	}

	owner, err := pgxpool.New(context.Background(), ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	mode := os.Getenv("JARLORE_TEST_EXEC_MODE")
	app, err := db.NewPool(context.Background(), withUser(t, admin, apiRole, name), db.PoolOptions{MaxConns: 4, QueryExecMode: mode})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return &DB{Name: name, admin: admin, Owner: owner, App: app, Store: &db.Store{Pool: app}}
}

// Password is the password of every role testdb creates (test-only).
const Password = password

// OwnerRole and APIRole are the migration owner and runtime roles testdb creates.
const (
	OwnerRole = ownerRole
	APIRole   = apiRole
)

// Admin returns a connection to the server as the JARLORE_TEST_DATABASE_URL role.
func (d *DB) Admin(t testing.TB) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), d.admin)
	if err != nil {
		t.Fatalf("testdb: connect admin: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// URLAs returns a URL for this test database as user (with Password); an empty
// user keeps the JARLORE_TEST_DATABASE_URL role.
func (d *DB) URLAs(t testing.TB, user string) string {
	t.Helper()
	if user == "" {
		u, err := url.Parse(d.admin)
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + d.Name
		return u.String()
	}
	return withUser(t, d.admin, user, d.Name)
}

func withUser(t testing.TB, base, user, dbname string) string {
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("testdb: JARLORE_TEST_DATABASE_URL must be a URL: %v", err)
	}
	u.User = url.UserPassword(user, password)
	u.Path = "/" + dbname
	return u.String()
}
