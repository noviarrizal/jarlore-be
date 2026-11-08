// Command migrate applies the SQL migrations in db/migrations with goose. It uses
// DIRECT_DATABASE_URL (Neon's direct, non-pooled endpoint) with the owner role.
//
//	go run ./cmd/migrate up        apply pending migrations
//	go run ./cmd/migrate status    list applied and pending migrations
//
// There is deliberately no "down": migrations are fixed forward.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"jarlore/internal/migrate"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run() error {
	cmd := "up"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	if cmd != "up" && cmd != "status" {
		return fmt.Errorf("unknown command %q (use up or status)", cmd)
	}
	url := os.Getenv("DIRECT_DATABASE_URL")
	if url == "" {
		return errors.New("DIRECT_DATABASE_URL is required (Neon direct connection, not the -pooler host)")
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		return err
	}
	defer db.Close()
	// A suspended Neon compute wakes up on the first connection.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	p, err := migrate.Provider(db)
	if err != nil {
		return err
	}
	if cmd == "status" {
		st, err := p.Status(ctx)
		if err != nil {
			return err
		}
		for _, s := range st {
			fmt.Printf("%-8s %s\n", s.State, s.Source.Path)
		}
		return nil
	}
	res, err := p.Up(ctx)
	for _, r := range res {
		fmt.Printf("applied %s (%s)\n", r.Source.Path, r.Duration.Round(time.Millisecond))
	}
	if err != nil {
		return err
	}
	if len(res) == 0 {
		fmt.Println("database is up to date")
	}
	return nil
}
