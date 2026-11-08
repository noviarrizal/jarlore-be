# Cloud Run deployment

Facts checked on 2026-11-06 against cloud.google.com pricing and Cloud Run docs. Prices and allowances change; re-check before relying on them. **Nothing has been deployed.** Deploying creates billable resources and needs the founder's go-ahead and a billing account.

## 1. What runs where

| Piece | Setting | Why |
|---|---|---|
| Region | `asia-southeast1` (Singapore) | Same city as Neon `aws-ap-southeast-1`. Jakarta (`asia-southeast2`) exists for Cloud Run but Neon has no Jakarta region, and API↔DB latency dominates |
| Billing | Request-based (`--cpu-throttling`, the default) | CPU and memory are billed only while a request is being handled |
| Instances | min 0, max 2 | Scale to zero; a hard ceiling on cost and on DB connections (2 × `DB_MAX_CONNS` 4 = 8) |
| Size | 1 vCPU, 512 MiB | 512 MiB is the gen2 minimum; Go uses a fraction of it. Under 1 vCPU forces concurrency 1 |
| Concurrency | 20 | Requests share an instance and queue briefly for one of its 4 DB connections |
| Timeout | 30 s (the API's own deadline is 15 s) | Covers a Neon compute resuming |
| Probes | Startup and liveness on `/health/live` | Never touches the database, so a sleeping Neon compute can't get instances killed |
| Image | Multi-stage Dockerfile → `distroless/static-debian13:nonroot`, ~15 MB | Small, non-root, CA certificates included, no shell |
| Shutdown | SIGTERM → 8 s graceful drain → exit | Cloud Run sends SIGKILL 10 s after SIGTERM |

The service definition is [`deploy/cloudrun.service.yaml`](../../deploy/cloudrun.service.yaml).

### Dockerfile or buildpacks?

**Dockerfile.** `gcloud run deploy --source .` works either way (with a Dockerfile present, Cloud Build builds it). The Dockerfile pins the Go toolchain, produces a static non-root distroless image of ~15 MB, and builds the same way locally and in CI. Go buildpacks need no Dockerfile but pick "the latest stable Go" unless pinned with `GOOGLE_GO_VERSION`, produce a larger image on a fuller base, and are harder to reproduce locally.

## 2. Free allowance and what it means

Cloud Run request-based billing, free each month **per billing account**: 180,000 vCPU-seconds, 360,000 GiB-seconds, 2 million requests. The allowance is credited at Tier 1 prices; Singapore is a **Tier 2** region, so the same allowance covers fewer seconds there than in a Tier 1 region. Free outbound data (1 GiB a month) applies to North America only.

Rough reading for Jarlore: with 1 vCPU / 0.5 GiB and request-based billing, the vCPU-seconds run out first. A typical API request takes tens of milliseconds of billed time, plus a few seconds per cold start, so a few thousand requests a day stay far inside the allowance. Beyond it you pay per vCPU-second, GiB-second and million requests at Tier 2 prices (see the pricing page).

Other things that can cost money:

| Service | Free | Note |
|---|---|---|
| Artifact Registry | 0.5 GB storage | Keep few image versions; set a cleanup policy |
| Cloud Build | 2,500 build-minutes a month (Google calls this promotional) | `--source` deploys use it |
| Secret Manager | Small free tier of active secret versions and access operations | Two secrets here; disable old versions |
| Cloud Logging | Free ingestion allowance per project | The API logs one line per request; keep `LOG_LEVEL=info` |
| Network egress | Not free outside North America | Responses are small JSON |

**A billing account is required** to use Cloud Run at all, even inside the free allowance.

## 3. Deploy (when approved)

```bash
PROJECT_ID=...            # your project
REGION=asia-southeast1
gcloud config set project $PROJECT_ID
gcloud services enable run.googleapis.com artifactregistry.googleapis.com cloudbuild.googleapis.com secretmanager.googleapis.com

# Service account with no roles except reading its two secrets
gcloud iam service-accounts create jarlore-api
SA=jarlore-api@$PROJECT_ID.iam.gserviceaccount.com

# Secrets (paste values; they never go in files or images)
printf '%s' 'postgres://jarlore_api:...@ep-xxx-pooler.ap-southeast-1.aws.neon.tech/neondb?sslmode=verify-full' | gcloud secrets create jarlore-database-url --data-file=-
openssl rand -base64 48 | tr -d '\n' | gcloud secrets create jarlore-cursor-secret --data-file=-
for s in jarlore-database-url jarlore-cursor-secret; do
  gcloud secrets add-iam-policy-binding $s --member=serviceAccount:$SA --role=roles/secretmanager.secretAccessor
done

# Image
gcloud artifacts repositories create jarlore --repository-format=docker --location=$REGION
gcloud builds submit --tag $REGION-docker.pkg.dev/$PROJECT_ID/jarlore/jarlore-api:$(git rev-parse --short HEAD)

# Service: edit PROJECT_ID, VERSION, FRONTEND_ORIGIN and NEON_AUTH_HOST in deploy/cloudrun.service.yaml, then
gcloud run services replace deploy/cloudrun.service.yaml --region $REGION
gcloud run services add-iam-policy-binding jarlore-api --region $REGION --member=allUsers --role=roles/run.invoker
```

The API is public at the network level (`allUsers` invoker) because every `/v1` route requires a valid user token; health and `/openapi.yaml` are the only unauthenticated routes.

Equivalent one-off command without the YAML:

```bash
gcloud run deploy jarlore-api --source . --region $REGION --service-account $SA \
  --cpu 1 --memory 512Mi --concurrency 20 --min-instances 0 --max-instances 2 --timeout 30 --cpu-throttling \
  --set-env-vars APP_ENV=production,DB_MAX_CONNS=4,FRONTEND_ORIGIN=https://app.jarlore.example,AUTH_ISSUER=https://NEON_AUTH_HOST,AUTH_JWKS_URL=https://NEON_AUTH_HOST/neondb/auth/.well-known/jwks.json \
  --set-secrets DATABASE_URL=jarlore-database-url:latest,CURSOR_SIGNING_SECRET=jarlore-cursor-secret:latest \
  --startup-probe httpGet.path=/health/live,periodSeconds=2,failureThreshold=5,timeoutSeconds=1 \
  --liveness-probe httpGet.path=/health/live,periodSeconds=60,failureThreshold=3,timeoutSeconds=2
```

Run migrations from your machine (or a one-off Cloud Run job later) **before** deploying an API version that needs them: `DIRECT_DATABASE_URL=... go run ./cmd/migrate up`.

Smoke test:

```bash
URL=$(gcloud run services describe jarlore-api --region $REGION --format 'value(status.url)')
curl $URL/health/live; curl $URL/health/ready   # ready wakes Neon
```

Then set `JARLORE_API_URL=$URL` in the frontend's environment.

## 4. Cold starts

Two can stack after a quiet period:
1. Cloud Run starts an instance: the Go binary is ready in milliseconds; image pull and sandbox start dominate (roughly a second, an estimate to measure after the first deploy).
2. Neon resumes the suspended compute on the first query (usually well under a second).

The first request after idle is therefore noticeably slower; later ones are fast. Keeping `min-instances 1` would remove (1) but bills an idle instance around the clock: don't, unless real users complain and the cost is accepted.

## 5. Watching cost

- **Budget alert:** Billing → Budgets & alerts → create a budget for the project (e.g. USD 5) with alerts at 50/90/100 %. Alerts email you; **they do not stop spending.** To cap hard you would have to disable billing on alert (Pub/Sub + a function), which also shuts the service down.
- Cloud Run → service → Metrics: request count, billable instance time, instance count. Billable instance time is what consumes the allowance.
- Billing → Reports, filtered by service (Cloud Run, Artifact Registry, Cloud Build, Secret Manager).
- Keep `max-instances` low; that is the most effective guard. Delete old image versions.

## 6. Hardening still to do

- Restrict CORS to the real frontend origin (`FRONTEND_ORIGIN`); the frontend calls the API server-side, so CORS only matters for future browser calls.
- Rate limits are not implemented yet (P1); `max-instances` and the per-instance DB pool bound the blast radius meanwhile.
- Consider Cloud Armor or an API gateway only if abuse appears; both cost money.
