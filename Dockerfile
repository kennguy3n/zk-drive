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
# Compact supervisor binary. Used ONLY by the combined image (the
# split server/worker images never run it) to drive the single-command
# SME deployment (deploy/docker-compose.compact.yml): it embeds a NATS
# JetStream server in-process, auto-migrates the schema, and supervises
# the /app/server and /app/worker child processes that this same image
# already ships — keeping every all-in-one entrypoint in one tag.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X github.com/kennguy3n/zk-drive/internal/version.Version=${APP_VERSION}" -o /out/compact ./cmd/compact
# HTTP liveness-probe binary. debian:bookworm-slim ships no wget/curl, so
# container-level health checks (ECS task definitions, docker-compose
# `healthcheck:`) invoke `/app/healthcheck` instead of shelling out. Same
# one-image-many-entrypoints pattern as the binaries above.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X github.com/kennguy3n/zk-drive/internal/version.Version=${APP_VERSION}" -o /out/healthcheck ./cmd/healthcheck

# ---- SBOM stage ----
# Generate an SPDX-format Software Bill of Materials for the artifacts
# that actually ship in the runtime image, so every release carries an
# auditable supply-chain manifest a tenant security team can ingest
# (Grype, Dependency-Track, `cosign attach sbom`, etc.).
#
# We scan BOTH inputs because they catalog different things:
#   - the compiled binaries under /out — syft's go-module-binary
#     cataloger reads the module versions Go embeds in each binary,
#     which is the authoritative list of what is linked into the
#     shipped executables (build tags, replaces and all);
#   - go.mod/go.sum — pins the full resolved graph including modules
#     that a given binary may not reference, so the SBOM is a superset
#     rather than only the call-graph-reachable subset.
#
# The syft image is distroless (no shell), so the scan runs via exec
# form. The tag is pinned (not :latest) so a syft default-format
# change can't silently alter the SBOM shape between builds.
FROM anchore/syft:v1.18.1 AS sbom
COPY --from=builder /src/go.mod /src/go.sum /scan/src/
COPY --from=builder /out /scan/bin
RUN ["/syft", "scan", "dir:/scan", "--source-name", "zk-drive", "-o", "spdx-json=/sbom.spdx.json"]

# ---- Runtime stage ----
# debian:bookworm-slim (instead of distroless static) so the worker
# can shell out to external preview tools. Each tool's licence is
# called out here so the proprietary-build implications are auditable
# from this single Dockerfile rather than scattered across the
# Go handler files:
#
#   poppler-utils (pdftoppm)     GPL — subprocess, not linked
#   libreoffice                  MPL-2.0 — subprocess, not linked
#   ffmpeg                       LGPL — subprocess, not linked
#   librsvg2-bin (rsvg-convert)  LGPL — subprocess, not linked
#   imagemagick                  ImageMagick Licence (Apache-2.0-style)
#                                 — subprocess, not linked
#   audiowaveform (optional)     not installed in the default image
#                                 because the upstream Debian package
#                                 is not available everywhere; the
#                                 audio handler transparently falls
#                                 back to ffmpeg's showwavespic when
#                                 audiowaveform is missing. Bake it
#                                 in per-deployment if you want the
#                                 BBC-quality output.
#
# All shelled-out — none of these are linked into the Go binaries —
# so they do not affect the proprietary licence of the binaries copied
# in below.
FROM debian:bookworm-slim AS runtime
WORKDIR /app

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        poppler-utils \
        libreoffice-core \
        libreoffice-writer \
        libreoffice-calc \
        libreoffice-impress \
        ffmpeg \
        librsvg2-bin \
        imagemagick \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 65532 nonroot \
    && useradd --system --uid 65532 --gid 65532 --home-dir /app nonroot \
    && sed -i 's@<policy domain="coder" rights="none" pattern="PS"/>@<policy domain="coder" rights="read" pattern="PS"/>@' /etc/ImageMagick-6/policy.xml \
    && sed -i 's@<policy domain="coder" rights="none" pattern="EPS"/>@<policy domain="coder" rights="read" pattern="EPS"/>@' /etc/ImageMagick-6/policy.xml \
    && sed -i 's@<policy domain="coder" rights="none" pattern="PDF"/>@<policy domain="coder" rights="read" pattern="PDF"/>@' /etc/ImageMagick-6/policy.xml

COPY --from=builder /out/server /app/server
COPY --from=builder /out/worker /app/worker
COPY --from=builder /out/migrate /app/migrate
COPY --from=builder /out/reconciler /app/reconciler
COPY --from=builder /out/orphan-gc /app/orphan-gc
COPY --from=builder /out/audit-archiver /app/audit-archiver
COPY --from=builder /out/audit-restore /app/audit-restore
COPY --from=builder /out/compact /app/compact
COPY --from=builder /out/healthcheck /app/healthcheck
COPY --from=builder /src/migrations /app/migrations
# SPDX SBOM shipped at a stable, documented path so a
# running container can serve its own bill of materials to a scanner
# or a compliance export without rebuilding.
COPY --from=sbom /sbom.spdx.json /usr/share/sbom/zk-drive.spdx.json

# Writable JetStream store for the embedded NATS broker the /app/compact
# supervisor runs (deploy/docker-compose.compact.yml sets
# ZKDRIVE_NATS_STORE_DIR here and mounts a volume on it). It must be owned
# by the nonroot runtime user: Docker initialises a fresh empty named
# volume with the ownership/permissions of this directory in the image, so
# creating it nonroot-owned here is what makes the mounted volume writable
# without an init container or running as root. Unused by the server-only
# default entrypoint; harmless when no volume is mounted.
RUN mkdir -p /var/lib/zk-drive/nats \
    && chown -R 65532:65532 /var/lib/zk-drive

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/app/server"]
