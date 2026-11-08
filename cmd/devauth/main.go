// Command devauth is a LOCAL DEVELOPMENT stand-in for Neon Auth (Managed Better
// Auth). It signs short-lived EdDSA (Ed25519) access tokens shaped like Neon
// Auth's (iss = aud = issuer origin, role "authenticated") and serves the
// matching JWKS, so the API runs its real verification path locally.
//
// It binds to 127.0.0.1 only and must never be deployed. Production uses Neon
// Auth's JWKS (AUTH_JWKS_URL); the API never holds a signing key.
//
//	go run ./cmd/devauth                  # http://127.0.0.1:9999
//	curl -s -X POST 127.0.0.1:9999/token -d '{"user":"demo"}'
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Two fixed demo users, so you can test isolation between accounts.
var users = map[string]struct{ id, email string }{
	"demo":  {"00000000-0000-4000-8000-000000000001", "demo@jarlore.local"},
	"other": {"00000000-0000-4000-8000-000000000002", "other@jarlore.local"},
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func main() {
	addr := flag.String("addr", "127.0.0.1:9999", "listen address (loopback only)")
	keyPath := flag.String("key", filepath.Join(os.TempDir(), "jarlore-devauth-ed25519.pem"), "signing key file (created if missing)")
	flag.Parse()

	key, err := loadOrCreateKey(*keyPath)
	if err != nil {
		log.Fatal(err)
	}
	issuer := "http://" + *addr // Neon Auth uses the origin of the auth URL as iss and aud
	pub := key.Public().(ed25519.PublicKey)
	kid := keyID(pub)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"keys": []map[string]string{{
			"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "use": "sig", "kid": kid,
			"x": base64.RawURLEncoding.EncodeToString(pub),
		}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			User  string `json:"user"`
			Sub   string `json:"sub"`
			Email string `json:"email"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in)
		sub, email := in.Sub, in.Email
		if u, ok := users[in.User]; ok {
			sub, email = u.id, u.email
		}
		if !uuidRe.MatchString(sub) {
			http.Error(w, `send {"user":"demo"} or {"sub":"<uuid>","email":"..."}`, http.StatusBadRequest)
			return
		}
		// Neon Auth tokens live 15 minutes and the SDK refreshes them; development
		// sign-in has no refresh, so these last 12 hours.
		now := time.Now()
		tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
			"iss": issuer, "aud": issuer, "sub": sub, "id": sub, "email": email, "emailVerified": true,
			"role": "authenticated", "banned": false, "iat": now.Unix(), "exp": now.Add(12 * time.Hour).Unix(),
		})
		tok.Header["kid"] = kid
		signed, err := tok.SignedString(key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"access_token": signed, "token_type": "bearer", "expires_in": 12 * 3600, "user": map[string]string{"id": sub, "email": email}})
	})

	log.Printf("devauth: issuer %s (development only)", issuer)
	log.Printf("devauth: AUTH_JWKS_URL=%s/.well-known/jwks.json AUTH_ISSUER=%s", issuer, issuer)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func loadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	if b, err := os.ReadFile(path); err == nil {
		block, _ := pem.Decode(b)
		if block == nil {
			return nil, errors.New("devauth: bad key file " + path)
		}
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		ek, ok := k.(ed25519.PrivateKey)
		if !ok {
			return nil, errors.New("devauth: key file is not Ed25519: " + path)
		}
		return ek, nil
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, fmt.Errorf("devauth: write key: %w", err)
	}
	return key, nil
}

func keyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return base64.RawURLEncoding.EncodeToString(sum[:8])
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
