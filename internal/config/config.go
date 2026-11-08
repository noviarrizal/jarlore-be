// Package config reads and validates the environment once at startup.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env      string // development | test | production
	Port     string
	LogLevel slog.Level

	// DatabaseURL is the Neon pooled connection string (host contains -pooler),
	// used by the API at runtime. Migrations use DIRECT_DATABASE_URL (cmd/migrate).
	DatabaseURL     string
	DBMaxConns      int32
	DBQueryExecMode string // cache_statement (default) | exec

	AuthJWKSURL  string
	AuthIssuer   string
	AuthAudience string

	FrontendOrigins []string
	CursorSecret    []byte
	BetaProForAll   bool
	RequestTimeout  time.Duration
}

func (c Config) Production() bool { return c.Env == "production" }

// Load reads the API configuration. Every problem is reported at once.
func Load() (Config, error) {
	var errs []error
	get := func(k, def string) string {
		if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return def
	}
	require := func(k string) string {
		v := get(k, "")
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", k))
		}
		return v
	}

	c := Config{
		Env:             get("APP_ENV", "development"),
		Port:            get("PORT", "8080"),
		DatabaseURL:     require("DATABASE_URL"),
		DBQueryExecMode: get("DB_QUERY_EXEC_MODE", "cache_statement"),
		AuthJWKSURL:     require("AUTH_JWKS_URL"),
		AuthIssuer:      require("AUTH_ISSUER"),
		AuthAudience:    get("AUTH_AUDIENCE", get("AUTH_ISSUER", "")), // Neon Auth: aud = iss = origin of the auth URL
		CursorSecret:    []byte(get("CURSOR_SIGNING_SECRET", "")),
	}
	switch c.Env {
	case "development", "test", "production":
	default:
		errs = append(errs, fmt.Errorf("APP_ENV must be development, test or production, got %q", c.Env))
	}
	if err := c.LogLevel.UnmarshalText([]byte(get("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}
	if n, err := strconv.Atoi(get("DB_MAX_CONNS", "4")); err != nil || n < 1 || n > 50 {
		errs = append(errs, errors.New("DB_MAX_CONNS must be between 1 and 50"))
	} else {
		c.DBMaxConns = int32(n)
	}
	if c.DBQueryExecMode != "cache_statement" && c.DBQueryExecMode != "exec" {
		errs = append(errs, errors.New("DB_QUERY_EXEC_MODE must be cache_statement or exec"))
	}
	if d, err := time.ParseDuration(get("REQUEST_TIMEOUT", "15s")); err != nil || d <= 0 {
		errs = append(errs, errors.New("REQUEST_TIMEOUT must be a positive duration such as 15s"))
	} else {
		c.RequestTimeout = d
	}
	c.BetaProForAll, _ = strconv.ParseBool(get("BILLING_BETA_PRO_FOR_ALL", "false"))

	for _, o := range strings.Split(get("FRONTEND_ORIGIN", ""), ",") {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			c.FrontendOrigins = append(c.FrontendOrigins, o)
		}
	}

	if c.AuthJWKSURL != "" {
		u, err := url.Parse(c.AuthJWKSURL)
		if err != nil || u.Host == "" {
			errs = append(errs, errors.New("AUTH_JWKS_URL must be an absolute URL"))
		} else if c.Production() && u.Scheme != "https" {
			errs = append(errs, errors.New("AUTH_JWKS_URL must use https in production"))
		}
	}
	if c.Production() {
		if len(c.CursorSecret) < 32 {
			errs = append(errs, errors.New("CURSOR_SIGNING_SECRET must be at least 32 bytes in production"))
		}
		if len(c.FrontendOrigins) == 0 {
			errs = append(errs, errors.New("FRONTEND_ORIGIN is required in production"))
		}
	} else if len(c.CursorSecret) == 0 {
		c.CursorSecret = []byte("development-only-cursor-secret-do-not-use")
	}
	return c, errors.Join(errs...)
}
