// Package auth verifies access tokens issued by Neon Auth (Managed Better Auth)
// against its JWKS. The API never mints tokens and never accepts
// a user id from the client: the user is the verified `sub` claim.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/time/rate"
)

// User is the authenticated caller.
type User struct {
	ID    string // JWT sub, a UUID
	Email *string
}

type claims struct {
	jwt.RegisteredClaims
	Email  string `json:"email"`
	Role   string `json:"role"`
	Banned bool   `json:"banned"`
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Verifier checks signature, issuer, audience, expiry and subject.
type Verifier struct {
	keys     jwt.Keyfunc
	issuer   string
	audience string
}

// NewVerifier fetches the JWKS (without failing startup if the provider is briefly
// unreachable), refreshes it hourly and on unknown key ids at most once a minute.
func NewVerifier(ctx context.Context, jwksURL, issuer, audience string) (*Verifier, error) {
	k, err := keyfunc.NewDefaultOverrideCtx(ctx, []string{jwksURL}, keyfunc.Override{
		HTTPTimeout:       5 * time.Second,
		RefreshInterval:   time.Hour,
		RefreshUnknownKID: rate.NewLimiter(rate.Every(time.Minute), 1),
		Client:            &http.Client{Timeout: 5 * time.Second},
	})
	if err != nil {
		return nil, fmt.Errorf("auth: jwks: %w", err)
	}
	return &Verifier{keys: k.Keyfunc, issuer: issuer, audience: audience}, nil
}

// NewVerifierWithKeyfunc is used by tests.
func NewVerifierWithKeyfunc(k jwt.Keyfunc, issuer, audience string) *Verifier {
	return &Verifier{keys: k, issuer: issuer, audience: audience}
}

var ErrInvalidToken = errors.New("invalid token")

// Verify returns the user of a valid access token.
func (v *Verifier) Verify(token string) (User, error) {
	var c claims
	opts := []jwt.ParserOption{
		// Neon Auth signs with Ed25519 only. Accepting nothing else rules out
		// algorithm confusion, and the API never needs a token-minting secret.
		jwt.WithValidMethods([]string{"EdDSA"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(60 * time.Second),
	}
	if v.audience != "" {
		opts = append(opts, jwt.WithAudience(v.audience))
	}
	if _, err := jwt.ParseWithClaims(token, &c, v.keys, opts...); err != nil {
		return User{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if !uuidRe.MatchString(c.Subject) {
		return User{}, fmt.Errorf("%w: sub is not a user id", ErrInvalidToken)
	}
	if c.Banned || (c.Role != "" && c.Role != "authenticated") {
		return User{}, fmt.Errorf("%w: not a signed-in user", ErrInvalidToken)
	}
	u := User{ID: strings.ToLower(c.Subject)}
	if c.Email != "" {
		e := c.Email
		u.Email = &e
	}
	return u, nil
}

type ctxKey struct{}

// WithUser stores the authenticated user in the request context.
func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// FromContext returns the authenticated user. Handlers behind the middleware can rely on it.
func FromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(ctxKey{}).(User)
	return u, ok
}

// BearerToken extracts the token from an Authorization header.
func BearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) < 8 || !strings.EqualFold(h[:7], "bearer ") {
		return "", false
	}
	t := strings.TrimSpace(h[7:])
	return t, t != ""
}
