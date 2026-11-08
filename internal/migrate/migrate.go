// Package migrate applies the embedded SQL migrations with goose.
package migrate

import (
	"context"
	"database/sql"
	"io/fs"

	"github.com/pressly/goose/v3"

	dbfiles "jarlore/db"
)

// Provider returns a goose provider over the embedded migrations.
func Provider(db *sql.DB) (*goose.Provider, error) {
	sub, err := fs.Sub(dbfiles.Migrations, "migrations")
	if err != nil {
		return nil, err
	}
	return goose.NewProvider(goose.DialectPostgres, db, sub)
}

// Up applies every pending migration.
func Up(ctx context.Context, db *sql.DB) ([]*goose.MigrationResult, error) {
	p, err := Provider(db)
	if err != nil {
		return nil, err
	}
	return p.Up(ctx)
}
