# jarlore-be

Backend for **Jarlore**, a personal fermentation workspace for home kombucha and water kefir brewers (Log, Rate, Save as Recipe, Brew Again, Compare).

This repository will hold the HTTP JSON API that the Next.js frontend (`jarlore-fe`) calls. The API owns ownership checks, recipe version integrity, batch snapshots, validation, entitlements, diffs and history. Data lives in PostgreSQL.

**Status: documentation only.** No implementation exists yet, and no language or framework has been chosen (see ADR-002).

## Documentation

| Document | Contents |
|---|---|
| [docs/architecture/SYSTEM_OVERVIEW.md](docs/architecture/SYSTEM_OVERVIEW.md) | Context and container diagrams, FE/BE split, jobs, integrations, deployment, cost |
| [docs/architecture/DOMAIN_MODEL.md](docs/architecture/DOMAIN_MODEL.md) | Entities, ER diagram, lifecycles, invariants, snapshot and diff semantics, table mapping |
| [docs/architecture/API_CONTRACT.md](docs/architecture/API_CONTRACT.md) | API conventions and endpoint overview |
| [docs/api/openapi.yaml](docs/api/openapi.yaml) | OpenAPI 3.1 contract (source of truth for shapes) |
| [docs/architecture/SEQUENCES.md](docs/architecture/SEQUENCES.md) | Sequence diagrams for the core workflows |
| [docs/architecture/SECURITY.md](docs/architecture/SECURITY.md) | AuthN/AuthZ, isolation, webhooks, secrets, deletion, audit, food-safety constraints |
| [docs/architecture/ARCHITECTURE_DECISIONS.md](docs/architecture/ARCHITECTURE_DECISIONS.md) | ADRs and unresolved questions |

Configuration variable names are listed in [.env.example](.env.example).
