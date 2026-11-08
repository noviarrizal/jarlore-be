# syntax=docker/dockerfile:1
# Jarlore API for Cloud Run: a static Go binary on distroless, running as non-root.
# Build:  docker build -t jarlore-api .
# The image holds no secrets; configuration comes from environment variables.

FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
# CGO off: a static binary with no libc dependency. -trimpath and no VCS stamping
# keep builds reproducible from the same source.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/api ./cmd/api && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/migrate ./cmd/migrate

# distroless/static has CA certificates (TLS to Neon and the JWKS URL); time zone data is
# embedded in the binary (time/tzdata). Pin the image digest for fully reproducible builds.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/api /out/migrate /usr/local/bin/
USER nonroot:nonroot
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/api"]
