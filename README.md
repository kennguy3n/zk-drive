# ZK Drive

> Privacy-conscious document management with per-folder choice of
> confidential managed storage (default, server-readable for preview /
> search / virus-scan) or strict zero-knowledge mode (opt-in, the
> server never sees plaintext). Powered by zk-object-fabric. Secure
> file collaboration for teams, clients, and partners.

ZK Drive is a document management and file collaboration system — a
privacy-first alternative to Google Drive, OneDrive, and Dropbox —
built on top of the [ZK Object Fabric](https://github.com/kennguy3n/zk-object-fabric)
encrypted storage layer. It provides a familiar drive UI (folders,
files, sharing, previews) on provider-neutral, encrypted-at-rest
object storage, with an opt-in per-folder strict-zero-knowledge mode
for content that must never be readable by the server.

ZK Drive serves two roles:

1. **Standalone secure file storage and sharing product** for SMEs,
   agencies, consultancies, professional-services firms, and any
   organization that needs governed file collaboration with privacy,
   data residency, and predictable cost.
2. **Storage backbone for KChat** (the B2B team chat product). Every
   KChat room maps to a ZK Drive folder; chat attachments, voice notes,
   call recordings, and cold message archives all live in ZK Drive.

ZK Drive is a consumer of zk-object-fabric, **not a fork**. It uses
zk-object-fabric's S3-compatible API as its stable storage contract.
Encryption, caching, placement, and backend migration are delegated
entirely to zk-object-fabric. ZK Drive owns the application layer:
users, workspaces, folders, permissions, sharing, retention, and
previews.

## Why it exists

The file-storage market leaves a clear gap for privacy-conscious SMEs:

- **Privacy gap** — most providers (Google Drive, OneDrive, Dropbox)
  can read customer files at rest with no honest disclosure. ZK Drive
  defaults to **confidential managed storage** (server-readable in
  memory during request handling — this is the right default for SMEs
  because it enables previews, full-text search, virus scanning, and
  admin recovery, but it is **not** zero-knowledge and we say so).
  Folders that need strict zero-knowledge can opt in on a per-folder
  basis, in which case the server cannot decrypt the contents (and
  loses preview / search / virus-scan for those folders as the honest
  trade-off).
- **Data residency gap** — most providers do not let customers pin
  data to a specific country, DC, or rack. ZK Drive inherits
  zk-object-fabric's customer-controlled placement.
- **Predictable cost gap** — "unlimited storage" plans hide egress
  and per-seat costs. ZK Drive separates storage and bandwidth pricing
  explicitly, with no fair-use surprises.
- **B2B file collaboration gap** — SMEs need guest access, expiring
  links, client dropboxes, and retention policies without enterprise
  complexity or Box-tier pricing.
- **Chat-native storage gap** — when paired with KChat, every chat
  room gets a room folder, every attachment gets virus scanning and
  previews, and every call recording goes to governed cold storage.

## Capabilities

**Drive surface.** Nested folders, file versioning, rename, move,
copy, soft-delete (trash), restore, and full-text search across file
names, tags, and document body content (PDF, DOCX, plain text).

**Privacy modes.** Each folder picks between **confidential managed
storage** (default — server-readable for preview, search, and virus
scan; gateway-side encryption at rest) and **strict zero-knowledge**
(opt-in — end-to-end encrypted, no previews, no full-text search,
no virus scan). The choice is surfaced honestly in the UI.

**Sharing and external collaboration.** Per-file and per-folder
sharing with view / edit / admin roles. Token-based share links with
optional password, expiry, and max-download caps. Guest invites
scoped to a folder, with email delivery and acceptance flow. Client
rooms bundle a folder, a share link, and a dropbox upload surface
into a single collaboration space for an external client.

**Async pipelines.** NATS JetStream powers preview generation
(thumbnails for images, PDFs, office documents), ClamAV INSTREAM
virus scanning with quarantine on detection, full-text indexing, and
retention archival.

**Security and governance.** TOTP-based two-factor authentication
with recovery codes and a workspace-wide MFA enforcement policy.
Tenant isolation via Postgres row-level security. Per-folder and
per-workspace retention policies with automatic archival of old
versions to cold storage. A workspace-scoped audit log of
security-sensitive events, with optional cold-tier archival to S3
for SOC2 / HIPAA / GDPR retention windows.

**Outbound webhooks.** Workspace admins can register HTTPS endpoints
that receive HMAC-signed JSON notifications on file, permission, and
membership events. At-least-once delivery with exponential backoff,
automatic pause after sustained failures, and SSRF defence.

**Observability.** Prometheus metrics on `/metrics` for the server
and worker; OpenTelemetry distributed tracing over OTLP/HTTP with
parent-based sampling and JetStream span linkage; structured JSON
logs with `request_id`, `trace_id`, and `span_id` correlation.

**Storage characteristics.** Pooled per-workspace quotas (not
per-seat), workspace-level data residency through zk-object-fabric
placement policies, and direct-to-storage uploads via presigned PUT /
GET URLs — ZK Drive never proxies file bytes.

## Relationship to zk-object-fabric

ZK Drive is an application layer on top of zk-object-fabric. It does
**not** reimplement encryption, caching, placement, provider
migration, or S3 compatibility. Those concerns are owned by
zk-object-fabric and consumed through its S3 API.

```mermaid
flowchart TD
    Clients["Web UI / PWA / KChat"]
    Drive["ZK Drive<br/>(Application Layer)"]
    PG["Postgres<br/>(users, workspaces, folders,<br/>permissions, metadata)"]
    Redis["Redis / Valkey<br/>(sessions, cache)"]
    Fabric["zk-object-fabric<br/>(S3-compatible API)"]

    Clients --> Drive
    Drive --> PG
    Drive --> Redis
    Drive -->|"presigned PUT / GET URLs"| Fabric
    Clients -->|"direct upload / download"| Fabric
```

What ZK Drive owns:

- Users, workspaces, folder trees, file metadata.
- Permissions, sharing, guest invites, share links.
- Activity log, audit log, retention rules, admin surface.
- Preview, scan, index, retention, and archive workers.

What zk-object-fabric owns:

- Encrypted file storage (per-object DEKs, encrypted manifests).
- Versioned objects.
- Presigned URL generation and validation.
- Customer-controlled placement policies (country / DC / rack).
- Backend migration (Wasabi → local DC) without changing the S3 API.
- Hot object cache and egress accounting.

## Relationship to KChat

KChat is a separate B2B team chat product that uses ZK Drive as its
file layer. The integration is one-directional: KChat depends on ZK
Drive, but **ZK Drive does not depend on KChat**. ZK Drive ships and
sells as a standalone product.

- Every KChat room maps to a ZK Drive folder (the "room folder").
- Chat attachments upload directly to ZK Drive via presigned URLs.
- Voice notes and call recordings are stored as files in the room
  folder.
- Cold message archives (old chat history) are compressed and stored
  as JSONL / Parquet objects in ZK Drive.
- Exports and eDiscovery output land in a dedicated export bucket.

KChat is a separate repository. The integration surface is the ZK
Drive REST API plus the shared zk-object-fabric S3 API. See
[`docs/PRODUCT.md`](docs/PRODUCT.md) for the integration design.

## Tech stack

- **Backend**: Go. Drive API, async workers, permission evaluation,
  sharing, retention, archival.
- **Frontend**: React + TypeScript (Vite), packaged as an installable
  PWA.
- **Metadata DB**: Postgres (with row-level security per workspace).
- **Cache / sessions**: Redis / Valkey.
- **Object storage**: zk-object-fabric S3 API (all file content).
- **Async jobs**: NATS JetStream.
- **Search**: Postgres full-text search by default; OpenSearch or
  Meilisearch is layered on top only when query volume or corpus size
  exceeds what Postgres FTS can serve.

## Repository structure

```
zk-drive/
  cmd/
    server/              # HTTP API server
    worker/              # JetStream consumer / job runner
    migrate/             # Database migrator (run before server / worker)
    reconciler/          # Out-of-band storage-counter reconciler (CronJob)
    orphan-gc/           # Out-of-band orphan-object reclaim (CronJob)
    audit-archiver/      # Audit-log cold archival (CronJob)
    audit-restore/       # Read-only restore CLI for archived audit rows
  api/
    admin/               # Admin surface (users, billing, placement, CMK)
    auth/                # Authentication, sessions, OAuth2 SSO, TOTP
    drive/               # Files, folders, bulk operations, sharing
    kchat/               # KChat integration API
    middleware/          # Auth, tenant guard, rate limiting, security headers
    webhooks/            # Outbound webhook subscription admin
    ws/                  # WebSocket real-time notifications
  internal/
    activity/            # User-facing activity log
    audit/               # Security audit log + cold archival
    billing/             # Quota enforcement, Stripe webhooks
    config/              # Application configuration
    crypto/              # AES-256-GCM credential encryption, CMK validation
    database/            # Database connection and helpers
    fabric/              # zk-object-fabric tenant provisioning and placement
    file/                # File metadata and versioning
    folder/              # Folder tree and hierarchy
    gc/                  # Orphan-object reclaim
    index/               # Content text extraction for FTS (PDF, DOCX, text)
    jobs/                # NATS JetStream job publisher
    kchat/               # KChat room-folder service and repository
    logging/             # Structured slog with chi correlation
    metrics/             # Prometheus collectors
    notification/        # In-app + Redis pub/sub notifications
    permission/          # Permission and role evaluation
    preview/             # Preview generation
    reconciler/          # Recompute denormalised counters
    retention/           # Retention policy evaluation and cold archival
    scan/                # Virus scanning (ClamAV INSTREAM)
    search/              # Postgres FTS
    session/             # Redis-backed session store
    sharing/             # Share links, guest invites, client rooms
    storage/             # S3 client and per-workspace client factory
    tracing/             # OpenTelemetry OTLP/HTTP integration
    user/                # User management
    webhooks/            # Outbound webhook delivery and signing
    workspace/           # Workspace and organisation logic
  frontend/              # React + TypeScript (Vite) PWA
  migrations/            # Postgres SQL migrations
  tests/
    integration/         # Go integration tests
    e2e/                 # Playwright browser tests
  deploy/
    k8s/                 # Kubernetes manifests (dev / staging)
    docker-compose.prod.yml
  docs/                  # Product, architecture, configuration, operations
```

## Quick start

ZK Drive expects a running zk-object-fabric S3 gateway. The simplest
local setup is to point ZK Drive at the upstream Docker demo:

```
export DATABASE_URL=postgres://zkdrive:zkdrive@localhost:5432/zk-drive?sslmode=disable
export JWT_SECRET=dev-secret

export S3_ENDPOINT=http://localhost:8080
export S3_BUCKET=mybucket
export S3_ACCESS_KEY=demo-access-key
export S3_SECRET_KEY=demo-secret-key

docker compose up -d postgres nats clamav
./migrate
./server &
./worker &
```

ZK Drive then generates presigned PUT / GET URLs that clients use to
move bytes directly to zk-object-fabric — the ZK Drive API server
never proxies file content.

Without `S3_ENDPOINT`, ZK Drive still boots and serves metadata-only
endpoints, but `/api/files/upload-url`, `/api/files/confirm-upload`,
and `/api/files/{id}/download-url` respond `501 Not Implemented`.

## Documentation

- [`docs/PRODUCT.md`](docs/PRODUCT.md) — product positioning, market,
  feature set, pricing tiers, KChat integration design.
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — system
  architecture, data model, API surface, async pipelines, encryption
  integration, PWA, deployment topology.
- [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md) — exhaustive
  environment-variable reference for every binary.
- [`docs/OPERATIONS.md`](docs/OPERATIONS.md) — deployment ordering,
  observability and tracing, audit-log cold archival, outbound
  webhook signing and delivery semantics, TOTP recovery flows, SMTP
  provider notes.
- [`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md) — running the test
  suites, frontend lint and build, Playwright e2e.
- [`deploy/README.md`](deploy/README.md) — Kubernetes and Docker
  Compose deployment paths.

## Status

Production-ready. ZK Drive ships as a standalone product and is also
used as the storage backbone for KChat.

## License

Proprietary — All Rights Reserved. See [`LICENSE`](LICENSE).
