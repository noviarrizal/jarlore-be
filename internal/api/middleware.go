package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"jarlore/internal/apperr"
	"jarlore/internal/auth"
	"jarlore/internal/httpx"
)

// requestID accepts a sane X-Request-Id from the caller (the frontend passes its
// own) or generates one, and echoes it in the response.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if len(id) < 8 || len(id) > 64 || strings.ContainsAny(id, " \t\r\n") {
			var b [12]byte
			_, _ = rand.Read(b[:])
			id = "req_" + hex.EncodeToString(b[:])
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(httpx.WithRequestID(r.Context(), id)))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(s int) { w.status = s; w.ResponseWriter.WriteHeader(s) }
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// logRequests writes one structured line per request. It never logs headers,
// bodies or query strings (they can carry tokens and notes).
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if r.URL.Path == "/health/live" {
			return
		}
		level := slog.LevelInfo
		if sw.status >= 500 {
			level = slog.LevelError
		}
		slog.Log(r.Context(), level, "request",
			"method", r.Method, "route", routePattern(r), "status", sw.status, "bytes", sw.bytes,
			"duration_ms", time.Since(start).Milliseconds(), "request_id", httpx.RequestID(r.Context()))
	})
}

func recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				slog.ErrorContext(r.Context(), "panic", "value", v, "stack", string(debug.Stack()), "request_id", httpx.RequestID(r.Context()))
				httpx.Error(w, r, nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// cors allows only the configured frontend origins. The API uses bearer tokens,
// never cookies, so credentials are not allowed.
func cors(origins []string) func(http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range origins {
		allowed[o] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && allowed[origin] {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Add("Vary", "Origin")
				h.Set("Access-Control-Expose-Headers", "X-Request-Id, Idempotent-Replayed, Location, Retry-After")
				if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
					h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE")
					h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Request-Id")
					h.Set("Access-Control-Max-Age", "600")
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// timeout bounds every request so a slow database wake-up can't hold an
// instance forever. Cloud Run's own request timeout is the outer limit.
func timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ProfileEnsurer creates the user's profile on first contact.
type ProfileEnsurer interface {
	EnsureProfile(ctx context.Context, u auth.User) error
}

// authenticate verifies the bearer token and puts the user in the context.
func authenticate(v *auth.Verifier, p ProfileEnsurer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := auth.BearerToken(r)
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer realm="jarlore"`)
				httpx.Error(w, r, apperr.Unauthenticated("Send an access token in the Authorization header."))
				return
			}
			u, err := v.Verify(token)
			if err != nil {
				slog.DebugContext(r.Context(), "token rejected", "err", err, "request_id", httpx.RequestID(r.Context()))
				w.Header().Set("WWW-Authenticate", `Bearer realm="jarlore", error="invalid_token"`)
				httpx.Error(w, r, apperr.Unauthenticated("The access token is invalid or expired. Sign in again."))
				return
			}
			if err := p.EnsureProfile(r.Context(), u); err != nil {
				httpx.Error(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), u)))
		})
	}
}
