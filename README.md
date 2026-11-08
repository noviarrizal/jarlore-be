# jarlore-be

The Go API behind **Jarlore**, a personal fermentation workspace: log F1 batches, split them into F2 bottle experiments, rate them, save what worked as versioned recipes, brew again and compare.

Go 1.27 · net/http + chi · pgx/v5 · sqlc · goose · Neon PostgreSQL · Neon Auth (identity) · Google Cloud Run.

## Run it locally

You need Go 1.27+ and a PostgreSQL 15+ server (local, or a Neon dev branch).

```bash
# 1. Database set up like Neon: a non-superuser owner and a restricted API role
psql -U postgres -f scripts/local-db.sql
DIRECT_DATABASE_URL='postgres://jarlore_owner:jarlore_owner_local@localhost:5432/jarlore?sslmode=disable' go run ./cmd/migrate up
psql -U postgres -d jarlore -c 'GRANT jarlore_app TO jarlore_api'

# 2. Local token issuer (development and tests only; stands in for Neon Auth;
#    refuses non-loopback addresses and APP_ENV=production)
go run ./cmd/devauth                     # http://127.0.0.1:9999

# 3. The API
export DATABASE_URL='postgres://jarlore_api:jarlore_api_local@localhost:5432/jarlore?sslmode=disable'
export AUTH_JWKS_URL=http://127.0.0.1:9999/.well-known/jwks.json AUTH_ISSUER=http://127.0.0.1:9999
export FRONTEND_ORIGIN=http://localhost:3000
go run ./cmd/api                         # http://localhost:8080

# 4. Try it
TOKEN=$(curl -s -X POST 127.0.0.1:9999/token -d '{"user":"demo"}' | jq -r .access_token)
curl -s localhost:8080/v1/today -H "Authorization: Bearer $TOKEN"
```

Then run `jarlore-fe` with `JARLORE_API_URL=http://localhost:8080` and `JARLORE_DEV_AUTH_URL=http://127.0.0.1:9999`, and sign in as the demo brewer. All variables are described in [.env.example](.env.example).

## Tests

```bash
go test ./internal/domain/                 # pure diff engine and units, no database
JARLORE_TEST_DATABASE_URL='postgres://postgres@localhost:5432/postgres?sslmode=disable' go test ./...
JARLORE_TEST_EXEC_MODE=exec go test ./...  # same, with pgx's pooler fallback mode
```

CI (`.github/workflows/ci.yml`) runs all of the above on PostgreSQL 18 plus gofmt, vet, staticcheck, the race detector, `sqlc diff`, migrations applied twice, Redocly, and a container job that builds the image, checks it runs as non-root without secrets, refuses the owner role and shuts down gracefully. CI never touches Neon.

Database tests create a fresh database per package with the same roles as production (non-superuser owner, restricted API role) and drop it afterwards. Without `JARLORE_TEST_DATABASE_URL` they are skipped, not faked. What they cover: the end-to-end acceptance scenario, the version rule, concurrent version numbering, idempotency, measurement kinds and units, user isolation, RLS, triggers, composite keys, grants, JWT verification and response conformance to `api/openapi.yaml`.

## Changing things

| Change | Do |
|---|---|
| Schema | Add `db/migrations/0000N_*.sql` (goose, no Down), then `sqlc generate` |
| Queries | Edit `db/queries/*.sql`, then `sqlc generate` (sqlc v1.31+; generated code is committed) |
| Contract | Edit `api/openapi.yaml`, update `internal/app` types, run the tests, then `pnpm gen:api` in jarlore-fe |
| Rules | `internal/domain` (pure, unit-tested) or `internal/app` (use cases) — never in handlers |

## Environments

`.env` is for development (local PostgreSQL or the Neon `dev` branch); production credentials live in Secret Manager and, for running migrations, in a separate `.env.production`. The API refuses to start with an owner or BYPASSRLS role. See [ENVIRONMENT_STRATEGY](docs/deployment/ENVIRONMENT_STRATEGY.md).

## Deploy

Container: `docker build -t jarlore-api .` (static binary on distroless, non-root). Production setup, costs and limits: [Neon](docs/deployment/NEON_SETUP.md), [Cloud Run](docs/deployment/CLOUD_RUN.md). Nothing is deployed yet.

## Documentation

| Document | Contents |
|---|---|
| [SYSTEM_OVERVIEW](docs/architecture/SYSTEM_OVERVIEW.md) | Decisions, components, request path, environments, cost |
| [DATABASE_DESIGN](docs/architecture/DATABASE_DESIGN.md) | Tables, enforced relationships, immutability, roles, RLS, connections |
| [DOMAIN_MODEL](docs/architecture/DOMAIN_MODEL.md) | Entities, lifecycles, invariants, snapshot/version/diff semantics |
| [API_CONTRACT](docs/architecture/API_CONTRACT.md) and [api/openapi.yaml](api/openapi.yaml) | Conventions, implemented endpoints, the contract |
| [SECURITY](docs/architecture/SECURITY.md) | Authentication, isolation, validation, secrets, status |
| [SEQUENCES](docs/architecture/SEQUENCES.md) | Workflow sequence diagrams (design-time) |
| [ARCHITECTURE_DECISIONS](docs/architecture/ARCHITECTURE_DECISIONS.md) | ADRs 001–014 and resolved gaps |
| [IMPLEMENTATION_ROADMAP](docs/engineering/IMPLEMENTATION_ROADMAP.md) | What is done, what is next (P0/P1/P2) |
