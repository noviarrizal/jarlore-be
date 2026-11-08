// Command api is the Jarlore HTTP API. It is built for Cloud Run: it listens on
// $PORT on all interfaces, checks once at startup that its database role is
// subject to row level security, and shuts down gracefully on SIGTERM within
// Cloud Run's 10-second grace period.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // user time zones work in a minimal container image

	apispec "jarlore/api"
	"jarlore/internal/api"
	"jarlore/internal/app"
	"jarlore/internal/auth"
	"jarlore/internal/config"
	"jarlore/internal/db"
)

func main() {
	if err := run(); err != nil {
		slog.Error("api stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel,
		// Cloud Logging reads "severity" and "message".
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.LevelKey:
				a.Key = "severity"
			case slog.MessageKey:
				a.Key = "message"
			}
			return a
		},
	}))
	slog.SetDefault(logger)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.NewPool(ctx, cfg.DatabaseURL, db.PoolOptions{MaxConns: cfg.DBMaxConns, QueryExecMode: cfg.DBQueryExecMode})
	if err != nil {
		return err
	}
	defer pool.Close()
	// Refuse to serve if DATABASE_URL could skip row level security (an owner,
	// superuser or BYPASSRLS role). Runs before listening, so no request is served
	// with an unsafe identity; a resuming Neon compute gets up to 20 s.
	guardCtx, cancelGuard := context.WithTimeout(ctx, 20*time.Second)
	role, err := db.GuardRuntimeRole(guardCtx, pool)
	cancelGuard()
	if err != nil {
		return err
	}
	slog.Info("database role checked", "role", role.User)
	store := &db.Store{Pool: pool}

	verifier, err := auth.NewVerifier(ctx, cfg.AuthJWKSURL, cfg.AuthIssuer, cfg.AuthAudience)
	if err != nil {
		return err
	}
	svc := app.New(store, app.Options{CursorSecret: cfg.CursorSecret, BetaProForAll: cfg.BetaProForAll})

	srv := &http.Server{
		Addr: net.JoinHostPort("0.0.0.0", cfg.Port),
		Handler: api.NewRouter(api.Deps{
			Service: svc, Verifier: verifier, Ready: store.Ping, FrontendOrigins: cfg.FrontendOrigins,
			RequestTimeout: cfg.RequestTimeout, OpenAPI: apispec.OpenAPI,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      cfg.RequestTimeout + 5*time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", srv.Addr, "env", cfg.Env)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	// Cloud Run sends SIGKILL 10 s after SIGTERM: finish in-flight requests in 8 s.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
