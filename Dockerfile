# syntax=docker/dockerfile:1.7

# ---- Builder stage ----
FROM golang:1.25-alpine AS builder
WORKDIR /src

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG APP_VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X github.com/kennguy3n/zk-drive/internal/version.Version=${APP_VERSION}" -o /out/server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X github.com/kennguy3n/zk-drive/internal/version.Version=${APP_VERSION}" -o /out/worker ./cmd/worker
# Standalone migrate binary, shipped in the same image so deploys
# can run a K8s Job (or Compose service) that does `entrypoint:
# ["/app/migrate"]` before the server / worker pods come up. Keeping
# every entrypoint in one image avoids a separate image tag / build
# pipeline.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X github.com/kennguy3n/zk-drive/internal/version.Version=${APP_VERSION}" -o /out/migrate ./cmd/migrate
# Standalone reconciler binary, shipped in the same image so
# deploys can run a K8s CronJob (or Compose service) that does
# `entrypoint: ["/app/reconciler"]` to refresh the denormalized
# storage_used_bytes counter on the workspaces table. Same
# one-image-many-entrypoints pattern as the migrate binary above.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X github.com/kennguy3n/zk-drive/internal/version.Version=${APP_VERSION}" -o /out/reconciler ./cmd/reconciler
# Standalone orphan-object GC binary, same pattern as the
# reconciler. Deploys that prefer a dedicated K8s CronJob (over the
# in-process loop the worker runs by default) schedule
# `entrypoint: ["/app/orphan-gc"]` and set GC_INTERVAL_MINUTES=0 on
# the worker to avoid duplicate runs.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X github.com/kennguy3n/zk-drive/internal/version.Version=${APP_VERSION}" -o /out/orphan-gc ./cmd/orphan-gc
# Audit-log archiver binary. Same one-image-many-entrypoints
# pattern as the other CronJob binaries. Deployed as a nightly K8s
# CronJob that exports audit_log rows older than retention to S3
# cold archive and then deletes them from the hot table.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X github.com/kennguy3n/zk-drive/internal/version.Version=${APP_VERSION}" -o /out/audit-archiver ./cmd/audit-archiver
# Audit-log restore CLI. Read-only counterpart that reads
# archived rows back from S3 for incident investigation /
# compliance "produce all admin actions in workspace X between
# two dates" requests. Operators run it ad-hoc, not on a schedule.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X github.com/kennguy3n/zk-drive/internal/version.Version=${APP_VERSION}" -o /out/audit-restore ./cmd/audit-restore

# ---- Runtime stage ----
# debian:bookworm-slim (instead of distroless static) so the worker
# can shell out to pdftoppm (poppler-utils) for PDF preview rendering.
# poppler-utils is GPL but used only as an external subprocess by the
# worker, so it does not affect the proprietary licence of the Go
# binaries copied in below.
FROM debian:bookworm-slim AS runtime
WORKDIR /app

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        poppler-utils \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 65532 nonroot \
    && useradd --system --uid 65532 --gid 65532 --home-dir /app nonroot

COPY --from=builder /out/server /app/server
COPY --from=builder /out/worker /app/worker
COPY --from=builder /out/migrate /app/migrate
COPY --from=builder /out/reconciler /app/reconciler
COPY --from=builder /out/orphan-gc /app/orphan-gc
COPY --from=builder /out/audit-archiver /app/audit-archiver
COPY --from=builder /out/audit-restore /app/audit-restore
COPY --from=builder /src/migrations /app/migrations

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/app/server"]
