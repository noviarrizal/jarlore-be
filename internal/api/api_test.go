package api_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	apispec "jarlore/api"
	"jarlore/internal/api"
	"jarlore/internal/app"
	"jarlore/internal/auth"
	"jarlore/internal/testdb"
)

const (
	issuer   = "https://ep-test-123456.aws.neon.tech" // Neon Auth: iss = aud = origin of the auth URL
	audience = issuer
	aliceID  = "11111111-1111-4111-8111-111111111111"
	bobID    = "22222222-2222-4222-8222-222222222222"
	origin   = "http://localhost:3000"
)

type env struct {
	t      *testing.T
	srv    *httptest.Server
	key    ed25519.PrivateKey
	schema *jsonschema.Compiler
	cache  map[string]*jsonschema.Schema
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d := testdb.New(t)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	verifier := auth.NewVerifierWithKeyfunc(func(tok *jwt.Token) (any, error) {
		if tok.Header["kid"] != "test-key" {
			return nil, fmt.Errorf("unknown kid")
		}
		return pub, nil
	}, issuer, audience)
	svc := app.New(d.Store, app.Options{CursorSecret: []byte("test")})
	h := api.NewRouter(api.Deps{
		Service: svc, Verifier: verifier, Ready: d.Store.Ping, FrontendOrigins: []string{origin},
		RequestTimeout: 10 * time.Second, OpenAPI: apispec.OpenAPI,
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, key: key, schema: compileSpec(t), cache: map[string]*jsonschema.Schema{}}
}

// compileSpec loads api/openapi.yaml as a JSON Schema 2020-12 resource (OpenAPI 3.1
// schemas are JSON Schema), so responses can be validated against components.
func compileSpec(t *testing.T) *jsonschema.Compiler {
	t.Helper()
	var doc any
	if err := yaml.Unmarshal(apispec.OpenAPI, &doc); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(normalize(doc))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource("https://jarlore.test/openapi.json", parsed); err != nil {
		t.Fatal(err)
	}
	return c
}

// normalize turns YAML maps into JSON-compatible maps.
func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			x[k] = normalize(val)
		}
		return x
	case map[any]any:
		m := map[string]any{}
		for k, val := range x {
			m[fmt.Sprint(k)] = normalize(val)
		}
		return m
	case []any:
		for i := range x {
			x[i] = normalize(x[i])
		}
	}
	return v
}

func (e *env) token(sub string, mutate func(jwt.MapClaims)) string {
	now := time.Now()
	c := jwt.MapClaims{"iss": issuer, "aud": audience, "sub": sub, "role": "authenticated", "email": sub[:4] + "@example.com",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	if mutate != nil {
		mutate(c)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, c)
	tok.Header["kid"] = "test-key"
	s, err := tok.SignedString(e.key)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

type resp struct {
	status int
	header http.Header
	body   []byte
}

func (r resp) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode: %v: %s", err, r.body)
	}
}

func (e *env) do(method, path, token string, body any, headers ...string) resp {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		if s, ok := body.(string); ok {
			b = []byte(s)
		}
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{status: res.StatusCode, header: res.Header, body: b}
}

// conforms validates a response body against a component schema of the spec.
func (e *env) conforms(r resp, component string) {
	e.t.Helper()
	s, ok := e.cache[component]
	if !ok {
		var err error
		s, err = e.schema.Compile("https://jarlore.test/openapi.json#/components/schemas/" + component)
		if err != nil {
			e.t.Fatalf("compile %s: %v", component, err)
		}
		e.cache[component] = s
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(r.body))
	if err != nil {
		e.t.Fatalf("%s: response is not JSON: %s", component, r.body)
	}
	if err := s.Validate(v); err != nil {
		e.t.Errorf("response does not match %s:\n%v\nbody: %s", component, err, truncate(r.body))
	}
}

func truncate(b []byte) string {
	if len(b) > 1500 {
		return string(b[:1500]) + "…"
	}
	return string(b)
}

func (e *env) expect(r resp, status int, component string) {
	e.t.Helper()
	if r.status != status {
		e.t.Fatalf("want %d, got %d: %s", status, r.status, truncate(r.body))
	}
	if component != "" {
		e.conforms(r, component)
	}
}

func (e *env) problem(r resp, status int, code string) {
	e.t.Helper()
	e.expect(r, status, "Problem")
	if ct := r.header.Get("Content-Type"); ct != "application/problem+json" {
		e.t.Errorf("problem content type = %q", ct)
	}
	var p struct{ Code, RequestID string }
	r.decode(e.t, &p)
	if p.Code != code || p.RequestID == "" || p.RequestID != r.header.Get("X-Request-Id") {
		e.t.Errorf("problem = %+v, want code %s with the request id", p, code)
	}
	if strings.Contains(string(r.body), "SQLSTATE") || strings.Contains(string(r.body), "pq:") {
		e.t.Errorf("database details leaked: %s", r.body)
	}
}

func TestAuthentication(t *testing.T) {
	e := newEnv(t)
	e.problem(e.do("GET", "/v1/me", "", nil), 401, "unauthenticated")
	e.problem(e.do("GET", "/v1/me", "not-a-jwt", nil), 401, "unauthenticated")
	cases := map[string]func(jwt.MapClaims){
		"expired":        func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-2 * time.Minute).Unix() },
		"no expiry":      func(c jwt.MapClaims) { delete(c, "exp") },
		"wrong issuer":   func(c jwt.MapClaims) { c["iss"] = "https://evil.example" },
		"wrong audience": func(c jwt.MapClaims) { c["aud"] = "anon" },
		"banned":         func(c jwt.MapClaims) { c["banned"] = true },
		"service role":   func(c jwt.MapClaims) { c["role"] = "service_role" },
		"sub not a uuid": func(c jwt.MapClaims) { c["sub"] = "admin" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e.problem(e.do("GET", "/v1/me", e.token(aliceID, mutate), nil), 401, "unauthenticated")
		})
	}
	// A symmetric token signed with any secret is rejected (no algorithm confusion).
	hs := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": issuer, "aud": audience, "sub": aliceID, "exp": time.Now().Add(time.Hour).Unix()})
	hs.Header["kid"] = "test-key"
	signed, _ := hs.SignedString([]byte("guess"))
	e.problem(e.do("GET", "/v1/me", signed, nil), 401, "unauthenticated")
	// A token signed by another key.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	forged := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{"iss": issuer, "aud": audience, "sub": aliceID, "exp": time.Now().Add(time.Hour).Unix()})
	forged.Header["kid"] = "test-key"
	fs, _ := forged.SignedString(other)
	e.problem(e.do("GET", "/v1/me", fs, nil), 401, "unauthenticated")
	// Another asymmetric algorithm is refused too: Neon Auth only signs EdDSA.
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	es := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"iss": issuer, "aud": audience, "sub": aliceID, "exp": time.Now().Add(time.Hour).Unix()})
	es.Header["kid"] = "test-key"
	ess, _ := es.SignedString(ec)
	e.problem(e.do("GET", "/v1/me", ess, nil), 401, "unauthenticated")

	// A valid token: the profile is created lazily from sub and email.
	r := e.do("GET", "/v1/me", e.token(aliceID, nil), nil)
	e.expect(r, 200, "Me")
	var me app.Me
	r.decode(t, &me)
	if me.ID != aliceID || me.Email == nil || *me.Email != "1111@example.com" {
		t.Errorf("me = %+v", me)
	}
}

func TestHealthCORSAndErrors(t *testing.T) {
	e := newEnv(t)
	e.expect(e.do("GET", "/health/live", "", nil), 200, "HealthStatus")
	e.expect(e.do("GET", "/health/ready", "", nil), 200, "HealthStatus")
	if r := e.do("GET", "/openapi.yaml", "", nil); r.status != 200 || !bytes.Contains(r.body, []byte("openapi: 3.1.0")) {
		t.Errorf("openapi.yaml: %d", r.status)
	}

	pre := e.do("OPTIONS", "/v1/batches", "", nil, "Origin", origin, "Access-Control-Request-Method", "POST")
	if pre.status != 204 || pre.header.Get("Access-Control-Allow-Origin") != origin || !strings.Contains(pre.header.Get("Access-Control-Allow-Headers"), "Idempotency-Key") {
		t.Errorf("preflight from the frontend: %d %v", pre.status, pre.header)
	}
	evil := e.do("OPTIONS", "/v1/batches", "", nil, "Origin", "https://evil.example", "Access-Control-Request-Method", "POST")
	if evil.header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("an unknown origin must not be allowed")
	}

	tok := e.token(aliceID, nil)
	e.problem(e.do("GET", "/v1/nope", tok, nil), 404, "not_found")
	e.problem(e.do("GET", "/v1/batches/not-a-uuid", tok, nil), 404, "not_found")
	e.problem(e.do("POST", "/v1/batches", tok, `{"fermentationTypeId":`), 400, "bad_request")
	e.problem(e.do("POST", "/v1/batches", tok, map[string]any{"fermentationTypeId": "kombucha", "volumeMl": 1000, "ingredients": []any{}, "params": map[string]any{}, "userId": bobID}), 422, "validation_failed")
	e.problem(e.do("POST", "/v1/batches", tok, map[string]any{"fermentationTypeId": "mead", "volumeMl": 0, "ingredients": []any{}, "params": map[string]any{}}), 422, "validation_failed")
	e.problem(e.do("PATCH", "/v1/me", tok, map[string]any{"plan": "pro"}), 422, "validation_failed")
	e.problem(e.do("GET", "/v1/batches?limit=1000", tok, nil), 400, "bad_request")
	e.problem(e.do("GET", "/v1/batches?cursor=forged.cursor", tok, nil), 400, "invalid_cursor")
}

// TestWorkflowOverHTTP runs the core loop through the real router and checks every
// response against the OpenAPI contract.
func TestWorkflowOverHTTP(t *testing.T) {
	e := newEnv(t)
	tok := e.token(aliceID, nil)

	e.expect(e.do("GET", "/v1/fermentation-types", tok, nil), 200, "")
	var types struct{ Data []app.FermentationType }
	r := e.do("GET", "/v1/fermentation-types", tok, nil)
	r.decode(t, &types)
	if len(types.Data) != 3 || len(types.Data[0].DefaultIngredients) == 0 {
		t.Fatalf("types = %+v", types)
	}
	for i := range types.Data {
		b, _ := json.Marshal(types.Data[i])
		e.conforms(resp{body: b}, "FermentationType")
	}
	e.expect(e.do("GET", "/v1/entitlements", tok, nil), 200, "Entitlements")
	defs := e.do("GET", "/v1/measurement-definitions", tok, nil)
	e.expect(defs, 200, "")

	create := map[string]any{
		"fermentationTypeId": "kombucha", "name": "Ginger kombucha", "volumeMl": 4200,
		"ingredients": types.Data[0].DefaultIngredients, "params": types.Data[0].DefaultParams,
	}
	r = e.do("POST", "/v1/batches", tok, create, "Idempotency-Key", "create-0000001")
	e.expect(r, 201, "Batch")
	var batch app.Batch
	r.decode(t, &batch)
	if r.header.Get("Location") != "/v1/batches/"+batch.ID {
		t.Errorf("Location = %q", r.header.Get("Location"))
	}
	replay := e.do("POST", "/v1/batches", tok, create, "Idempotency-Key", "create-0000001")
	if replay.status != 201 || replay.header.Get("Idempotent-Replayed") != "true" || !bytes.Equal(replay.body, r.body) {
		t.Errorf("replayed create: %d %q", replay.status, replay.header.Get("Idempotent-Replayed"))
	}

	e.expect(e.do("GET", "/v1/batches/"+batch.ID, tok, nil), 200, "Batch")
	e.expect(e.do("GET", "/v1/batches", tok, nil), 200, "BatchSummaryPage")
	e.expect(e.do("GET", "/v1/today", tok, nil), 200, "TodayResponse")
	e.problem(e.do("POST", "/v1/batches/"+batch.ID+"/logs", tok, map[string]any{"kind": "note"}), 422, "validation_failed")
	logBody := map[string]any{"kind": "taste", "note": "Bready", "measurements": []any{
		map[string]any{"definitionKey": "ph", "value": 3.2, "unit": "pH", "method": "strip"},
		map[string]any{"definitionKey": "temp", "value": 72.5, "unit": "°F", "method": nil},
	}}
	r = e.do("POST", "/v1/batches/"+batch.ID+"/logs", tok, logBody, "Idempotency-Key", "log-00000001")
	e.expect(r, 201, "CreateLogsResponse")
	e.expect(e.do("GET", "/v1/batches/"+batch.ID+"/logs", tok, nil), 200, "LogEntryPage")

	bottles := map[string]any{"bottles": []any{
		map[string]any{"label": "A", "flavorName": "Ginger juice", "volumeMl": 500, "additions": []any{map[string]any{"name": "Ginger", "form": "juice", "amount": 15, "unit": "ml"}}, "primingSugarG": 4, "headspaceCm": 2},
		map[string]any{"label": "B", "flavorName": "Ginger puree", "volumeMl": 500, "additions": []any{map[string]any{"name": "Ginger", "form": "puree", "amount": 30, "unit": "g"}}, "primingSugarG": 4, "headspaceCm": 2},
	}}
	r = e.do("POST", "/v1/batches/"+batch.ID+"/bottles", tok, bottles)
	e.expect(r, 201, "Batch")
	r.decode(t, &batch)
	a, b := batch.Bottles[0], batch.Bottles[1]

	rate := map[string]any{"rating": map[string]any{"overall": 5, "fizz": 4, "sweetTart": 3, "flavor": 5, "offNotes": []string{}, "wouldRepeat": true, "note": "Best one"}}
	e.expect(e.do("POST", "/v1/bottles/"+a.ID+"/finish", tok, rate), 200, "Bottle")
	rate["rating"].(map[string]any)["overall"] = 3
	e.expect(e.do("POST", "/v1/bottles/"+b.ID+"/finish", tok, rate), 200, "Bottle")
	e.expect(e.do("PATCH", "/v1/bottles/"+a.ID, tok, map[string]any{"isReference": true}), 200, "Bottle")

	r = e.do("POST", "/v1/compare", tok, map[string]any{"items": []any{map[string]any{"type": "bottle", "id": a.ID}, map[string]any{"type": "bottle", "id": b.ID}}, "onlyDifferences": true})
	e.expect(r, 200, "CompareResult")
	r = e.do("POST", "/v1/compare", tok, map[string]any{"items": []any{map[string]any{"type": "bottle", "id": a.ID}, map[string]any{"type": "bottle", "id": b.ID}, map[string]any{"type": "batch", "id": batch.ID}}})
	e.problem(r, 402, "entitlement_required")

	r = e.do("POST", "/v1/bottles/"+a.ID+"/save-as-recipe", tok, map[string]any{"name": "Ginger juice bottle"})
	e.expect(r, 201, "Recipe")
	var recipe app.Recipe
	r.decode(t, &recipe)
	e.expect(e.do("GET", "/v1/recipes", tok, nil), 200, "RecipePage")
	e.expect(e.do("GET", "/v1/recipes/"+recipe.ID, tok, nil), 200, "Recipe")
	e.expect(e.do("GET", "/v1/recipes/"+recipe.ID+"/versions", tok, nil), 200, "RecipeVersionPage")
	e.expect(e.do("GET", "/v1/recipes/"+recipe.ID+"/versions/1", tok, nil), 200, "RecipeVersion")
	e.expect(e.do("GET", "/v1/recipe-versions/"+recipe.LatestVersion.ID, tok, nil), 200, "RecipeVersionDetail")
	e.expect(e.do("GET", "/v1/recipes/"+recipe.ID+"/batches", tok, nil), 200, "BatchSummaryPage")

	draft := map[string]any{"label": "C", "flavorName": "Ginger puree", "volumeMl": 500, "additions": []any{map[string]any{"name": "Ginger", "form": "puree", "amount": 30, "unit": "g"}}, "primingSugarG": 4, "headspaceCm": 2}
	r = e.do("POST", "/v1/bottles/diff-preview", tok, map[string]any{"base": map[string]any{"type": "recipe_version", "id": recipe.LatestVersion.ID}, "draft": draft})
	e.expect(r, 200, "DiffResult")
	var diff app.DiffResult
	r.decode(t, &diff)
	if diff.WillCreateVersion == nil || !*diff.WillCreateVersion {
		t.Errorf("a changed bottle creates a version: %+v", diff)
	}

	e.expect(e.do("POST", "/v1/batches/"+batch.ID+"/finish", tok, rate), 200, "Batch")
	e.expect(e.do("GET", "/v1/batches/"+batch.ID+"/reference", tok, nil), 200, "ReferenceResponse")
	r = e.do("GET", "/v1/batches/"+batch.ID+"/brew-again", tok, nil)
	e.expect(r, 200, "BrewAgainResponse")
	var again app.BrewAgainDraft
	r.decode(t, &again)
	e.expect(e.do("POST", "/v1/batches/diff-preview", tok, map[string]any{"base": map[string]any{"type": "batch", "id": batch.ID}, "draft": again.Draft}), 200, "DiffResult")
	r = e.do("POST", "/v1/batches/"+batch.ID+"/save-as-recipe", tok, map[string]any{"name": "Ginger kombucha"})
	e.expect(r, 201, "Recipe")
	r = e.do("POST", "/v1/batches", tok, again.Draft)
	e.expect(r, 201, "Batch")
	var kb2 app.Batch
	r.decode(t, &kb2)
	e.expect(e.do("GET", "/v1/batches/"+kb2.ID+"/snapshot", tok, nil), 200, "BatchSnapshot")
	e.problem(e.do("POST", "/v1/batches/"+batch.ID+"/finish", tok, rate), 409, "invalid_state_transition")

	// Another user sees none of it: 404 for every id, never 403.
	bobTok := e.token(bobID, nil)
	for _, path := range []string{"/v1/batches/" + batch.ID, "/v1/recipes/" + recipe.ID, "/v1/recipe-versions/" + recipe.LatestVersion.ID, "/v1/batches/" + batch.ID + "/logs"} {
		e.problem(e.do("GET", path, bobTok, nil), 404, "not_found")
	}
	e.problem(e.do("PATCH", "/v1/bottles/"+a.ID, bobTok, map[string]any{"isReference": false}), 404, "not_found")
	e.problem(e.do("POST", "/v1/batches/"+kb2.ID+"/logs", bobTok, map[string]any{"kind": "note"}, "Idempotency-Key", "bob-00000001"), 404, "not_found")
	var bobList struct{ Data []any }
	e.do("GET", "/v1/batches", bobTok, nil).decode(t, &bobList)
	if len(bobList.Data) != 0 {
		t.Errorf("Bob sees %d batches", len(bobList.Data))
	}
}

// The frontend server sends the browser's time zone; the profile is created in it,
// and later requests with another zone do not change it.
func TestTimezoneHintCreatesProfileInUserZone(t *testing.T) {
	e := newEnv(t)
	const carolID = "33333333-3333-4333-8333-333333333333"
	tok := e.token(carolID, nil)
	var me app.Me
	r := e.do("GET", "/v1/me", tok, nil, api.TimezoneHintHeader, "Asia/Jakarta")
	e.expect(r, 200, "Me")
	r.decode(t, &me)
	if me.Timezone != "Asia/Jakarta" {
		t.Fatalf("profile created in %q, want Asia/Jakarta", me.Timezone)
	}
	r = e.do("PATCH", "/v1/me", tok, map[string]any{"timezone": "Not/AZone"})
	e.problem(r, 422, "validation_failed")
	r = e.do("PATCH", "/v1/me", tok, map[string]any{"timezone": "America/New_York"})
	e.expect(r, 200, "Me")
	r.decode(t, &me)
	if me.Timezone != "America/New_York" {
		t.Fatalf("PATCH timezone = %q", me.Timezone)
	}
}
