# ZK Drive

> Privacy-preserving document management with zero-knowledge storage,
> powered by zk-object-fabric. Secure file collaboration for teams,
> clients, and partners.

## What it is

ZK Drive is a document management and file collaboration system — a
privacy-first alternative to Google Drive, OneDrive, and Dropbox —
built on top of the [ZK Object Fabric](https://github.com/kennguy3n/zk-object-fabric)
encrypted storage layer. It provides a familiar drive UI (folders,
files, sharing, previews) while ensuring data privacy through
client-side encryption and provider-neutral object storage.

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
  can read customer files at rest. ZK Drive is zero-knowledge by
  default in strict-ZK mode and confidentially managed in managed
  encrypted mode.
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

## Key capabilities

- **Folder and file management** — nested folders, file versioning,
  rename, move, copy, soft-delete (trash), restore.
- **Zero-knowledge encryption** — per-folder selection of managed
  encrypted mode (default) or strict ZK mode (opt-in), delegated to
  zk-object-fabric.
- **Managed encrypted mode** — gateway-side encryption via
  zk-object-fabric `ManagedEncrypted`. Enables server-side previews,
  virus scanning, and full-text search.
- **Sharing and permissions** — per-file and per-folder sharing with
  view / edit / admin roles. Folder permissions inherit to children
  unless overridden.
- **Guest access and client rooms** — invite external users by email
  with scoped folder access, expiry, and dropbox upload.
- **Expiring and password-protected share links** — token-based links
  with optional password, expiry, and max-download limits.
- **File versioning** — automatic version creation on re-upload, with
  restore and version list.
- **Previews** — thumbnails and previews for images, PDFs, and office
  documents (managed encrypted mode only).
- **Virus scanning** — async ClamAV scan on upload, quarantine on
  detection, admin notification.
- **Retention policies** — per-folder and per-workspace retention
  rules with automatic archival of old versions to cold storage.
- **Pooled org storage** — storage quota pooled across a workspace,
  not a fixed per-seat allocation.
- **Data residency** — workspace-level placement policies exposed
  through the admin UI, backed by zk-object-fabric placement.
- **S3-compatible backend** — all file content lives in
  zk-object-fabric, accessed via its S3-compatible API.
- **Direct-to-storage uploads** — clients upload directly to
  zk-object-fabric via presigned URLs; ZK Drive never proxies file
  bytes.

## Relationship to zk-object-fabric

ZK Drive is an application layer on top of zk-object-fabric. It does
**not** reimplement encryption, caching, placement, provider
migration, or S3 compatibility. Those concerns are owned by
zk-object-fabric and consumed through its S3 API.

```mermaid
flowchart TD
    Clients["Web UI / Mobile / KChat"]
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
- Activity log, retention rules, admin surface.
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
[docs/PROPOSAL.md §4](docs/PROPOSAL.md) for the KChat integration
design.

## Tech stack

- **Backend**: Go. Drive API, async workers, permission evaluation,
  sharing, retention.
- **Frontend**: React + TypeScript (Vite). Drive UI, sharing dialogs,
  admin pages, settings.
- **Metadata DB**: Postgres (partitioned by workspace).
- **Cache / sessions**: Redis / Valkey.
- **Object storage**: zk-object-fabric S3 API (all file content).
- **Async jobs**: NATS JetStream (preview, scan, index, retention,
  archive).
- **Search**: Postgres full-text search by default; OpenSearch or
  Meilisearch is layered on top only when query volume or corpus size
  exceeds what Postgres FTS can serve.

## Repository Structure

```
zk-drive/
  cmd/
    server/              # Main application server
    worker/              # Async job workers (preview, scan, classify, archive)
  api/
    admin/               # Admin API handlers (users, audit, billing, placement, CMK)
    auth/                # Authentication, session management, OAuth2 SSO
    drive/               # Drive HTTP API handlers (files, folders, bulk ops)
    kchat/               # KChat integration API (rooms, sync, attachments)
    middleware/          # Auth, tenant guard, rate limiting (in-memory + Redis)
    ws/                  # WebSocket real-time notifications
  internal/
    activity/            # User-facing activity log
    ai/                  # AI thread summary (rule-based + Ollama LLM)
    audit/               # Security audit log
    billing/             # Billing, quota enforcement, Stripe webhooks
    classify/            # File classification worker
    config/              # Application configuration
    crypto/              # AES-256-GCM credential encryption, CMK validation
    database/            # Database connection and helpers
    fabric/              # zk-object-fabric tenant provisioning and placement
    file/                # File metadata and versioning
    folder/              # Folder tree and hierarchy
    index/               # Content text extraction for FTS
    jobs/                # NATS JetStream job publisher
    kchat/               # KChat room-folder service and repository
    notification/        # In-app + Redis pub/sub notifications
    permission/          # Permission and role evaluation, inheritance
    preview/             # Preview generation (images + PDF)
    retention/           # Retention policy evaluation and cold archival
    scan/                # Virus scanning (ClamAV INSTREAM)
    search/              # Full-text search (Postgres FTS)
    session/             # Redis-backed session store
    sharing/             # Share links, guest invites, client rooms, templates
    storage/             # S3 client and per-workspace client factory
    user/                # User management
    wiring/              # Shared dependency wiring helpers
    workspace/           # Workspace and organization logic
  frontend/
    e2e/                 # Frontend Playwright specs
    src/
      api/               # API client
      components/        # File browser, upload, preview, sharing, search, PWA
      hooks/             # React hooks (useNotifications, etc.)
      pages/             # Drive UI, admin, billing, encryption, placement, KChat
  migrations/            # Postgres schema migrations
  tests/
    integration/         # Go integration tests
    e2e/                 # Playwright browser tests + presigned roundtrip
  deploy/
    k8s/                 # Kubernetes manifests (dev/staging)
    docker-compose.prod.yml
    README.md
  docs/
    PROPOSAL.md          # Product overview
    ARCHITECTURE.md      # System architecture
    PHASES.md            # Release history
    PROGRESS.md          # Changelog
    MOBILE_EVALUATION.md # Mobile strategy evaluation
```

## Status

Production-ready. ZK Drive ships as a standalone product and is also
used as the storage backbone for KChat.

See [docs/PROPOSAL.md](docs/PROPOSAL.md) for the product overview and
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the system
architecture.

## Configuration

ZK Drive is configured entirely via environment variables. The server
reads them at startup from `internal/config`.

Required:

| Variable       | Purpose                                                  |
| -------------- | -------------------------------------------------------- |
| `DATABASE_URL` | Postgres DSN (pgx-style).                                |
| `JWT_SECRET`   | HS256 signing secret for session tokens.                 |

Optional:

| Variable         | Default        | Purpose                                                     |
| ---------------- | -------------- | ----------------------------------------------------------- |
| `LISTEN_ADDR`    | `:8080`        | HTTP listen address.                                        |
| `MIGRATIONS_DIR` | `migrations`   | Path to SQL migrations applied by the `migrate` binary (read-only by `server` / `worker`). |

Storage (zk-object-fabric S3 gateway) — all four are required together:

| Variable         | Purpose                                                  |
| ---------------- | -------------------------------------------------------- |
| `S3_ENDPOINT`    | zk-object-fabric gateway base URL (e.g. `http://localhost:8080`). |
| `S3_BUCKET`      | Bucket to store all file versions under.                 |
| `S3_ACCESS_KEY`  | Gateway access key.                                      |
| `S3_SECRET_KEY`  | Gateway secret key.                                      |

If `S3_ENDPOINT` is unset, ZK Drive still boots and serves
metadata-only endpoints, but `/api/files/upload-url`,
`/api/files/confirm-upload`, and `/api/files/{id}/download-url` respond
with `501 Not Implemented`. If `S3_ENDPOINT` is set, the bucket, access
key, and secret key must also be set; otherwise startup fails.

### Quick start with the zk-object-fabric Docker demo

When running alongside zk-object-fabric's Docker demo, point ZK Drive
at the local gateway:

```
export S3_ENDPOINT=http://localhost:8080
export S3_BUCKET=mybucket
export S3_ACCESS_KEY=demo-access-key
export S3_SECRET_KEY=demo-secret-key
```

ZK Drive then generates presigned PUT / GET URLs that clients use to
move bytes directly to zk-object-fabric — the ZK Drive API server never
proxies file content.

## Deploying

ZK Drive ships three binaries from a single container image:

- `/app/migrate` — applies pending SQL migrations to the database and exits.
- `/app/server` — the HTTP API server. Refuses to start if migrations are out
  of date (see `internal/database.MinRequiredMigrationVersion`).
- `/app/worker` — the JetStream consumer / job runner. Same migration
  precondition as the server.

The migrate binary must run **before** the server / worker pods are rolled out.
On Kubernetes this is wired as a Job (see `deploy/k8s/migrate-job.yaml`) and
on Compose as a `service_completed_successfully` dependency (see
`deploy/docker-compose.prod.yml`). The migrate binary acquires a Postgres
advisory lock keyed on a fixed 64-bit constant so concurrent invocations
(e.g. two Job pods during a blue/green deploy) serialise safely.

For a manual one-off:

```
docker run --rm \
  -e DATABASE_URL=postgres://zkdrive:...@host:5432/zkdrive \
  -e JWT_SECRET=unused-but-required \
  ghcr.io/kennguy3n/zk-drive:<version> /app/migrate
```

## Running tests

### Go unit tests

```
go test -short ./...
```

### Integration tests (requires Postgres)

```
docker compose up -d postgres
export DATABASE_URL=postgres://zkdrive:zkdrive@localhost:5432/zk-drive?sslmode=disable
export JWT_SECRET=dev-secret
go test ./tests/integration/ -v
```

### Integration tests with storage (requires zk-object-fabric)

```
export S3_ENDPOINT=http://localhost:8080
export S3_BUCKET=mybucket
export S3_ACCESS_KEY=demo-access-key
export S3_SECRET_KEY=demo-secret-key
go test ./tests/integration/ -v
```

### Frontend lint and build

```
cd frontend && npm install && npm run lint && npm run build
```

### Playwright e2e tests

```
cd frontend && npx playwright test
```

## Documentation

- [Product Overview](docs/PROPOSAL.md)
- [Architecture](docs/ARCHITECTURE.md)
- [Release History](docs/PHASES.md)
- [Changelog](docs/PROGRESS.md)
- [Mobile Strategy Evaluation](docs/MOBILE_EVALUATION.md)

## License

Proprietary — All Rights Reserved. See [`LICENSE`](LICENSE).
