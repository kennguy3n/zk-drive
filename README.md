# ZK Drive

> Privacy-conscious document management with per-folder choice of
> confidential managed storage (default, server-readable for preview /
> search / virus-scan) or strict zero-knowledge mode (opt-in, the
> server never sees plaintext). Powered by zk-object-fabric. Secure
> file collaboration for teams, clients, and partners.

## What it is

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

## Key capabilities

- **Folder and file management** — nested folders, file versioning,
  rename, move, copy, soft-delete (trash), restore.
- **Per-folder privacy mode** — each folder picks between
  **confidential managed storage** (default — server-readable for
  preview / search / virus-scan; gateway-side encryption at rest)
  and **strict zero-knowledge** (opt-in — end-to-end encrypted, server
  never sees plaintext, no previews / no full-text search / no virus
  scan for those folders). Delegated to zk-object-fabric. The trade-off
  is surfaced honestly in the UI at folder-creation time and on the
  in-app Privacy page.
- **Confidential managed storage (default)** — gateway-side encryption
  via zk-object-fabric `ManagedEncrypted`. The server can read
  plaintext in memory during request handling, which is what enables
  server-side previews, virus scanning, and full-text search. This is
  **not** zero-knowledge; we deliberately call it "confidential
  managed" so customers can tell which folders are which.
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
    reconciler/          # Out-of-band storage-counter reconciler (CronJob)
    orphan-gc/           # Out-of-band orphan-object GC (CronJob)
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
    gc/                  # Orphan-object GC reconciler (presigned-PUT reclaim)
    index/               # Content text extraction for FTS
    jobs/                # NATS JetStream job publisher
    kchat/               # KChat room-folder service and repository
    notification/        # In-app + Redis pub/sub notifications
    permission/          # Permission and role evaluation, inheritance
    preview/             # Preview generation (images + PDF)
    reconciler/          # Recompute denormalized counters (storage_used_bytes)
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

Browser security headers (CSP / HSTS / X-Frame-Options / etc.) are
emitted on every response by `api/middleware.SecurityHeaders`. The
defaults are safe for a same-origin SPA; these knobs are how operators
allow-list the storage gateway origin and stage CSP rollouts:

| Variable                              | Default  | Purpose                                                                                                       |
| ------------------------------------- | -------- | ------------------------------------------------------------------------------------------------------------- |
| `SECURITY_HEADERS_CSP_CONNECT_EXTRA`  | empty    | Comma-separated origins added to CSP `connect-src` on top of `'self'`. Put the **public** URL the browser sees for the fabric storage gateway here so direct-to-storage uploads / downloads land. The default deliberately omits bare `wss:` / `ws:` scheme sources (an XSS exfil vector); `'self'` already covers same-origin WebSocket upgrades, and cross-origin WebSocket gateways must be listed explicitly here. |
| `SECURITY_HEADERS_CSP_IMG_EXTRA`      | empty    | Comma-separated origins added to CSP `img-src` on top of `'self' data: blob:`. Same gateway origin if thumbnails are served from it. |
| `SECURITY_HEADERS_CSP_REPORT_ONLY`    | `false`  | When `true`, the policy emits under `Content-Security-Policy-Report-Only` instead of enforcing — browsers report violations but do not block. Use during the first rollout, then flip to `false`. |
| `SECURITY_HEADERS_CSP_REPORT_URI`     | empty    | When set, appended as `report-uri <value>` to the CSP value. Browsers POST violation reports there.            |
| `SECURITY_HEADERS_DISABLE_HSTS`       | `false`  | When `true`, skips `Strict-Transport-Security`. Use for local HTTP development only; keep `false` in production. |
| `WORKER_METRICS_ADDR`                 | `:9091`  | Listen address for the **worker** binary's dedicated `/metrics` + `/healthz` HTTP server. Set to `off` (or empty) to disable. The server binary serves `/metrics` on the main `LISTEN_ADDR` and ignores this. The reconciler binary is short-lived and does not export metrics — see [Observability](#observability) below. |
| `GC_INTERVAL_MINUTES`                 | `360`    | Cadence of the worker's in-process orphan-object GC loop (WS-18). Reclaims S3 objects whose presigned PUT completed but whose ConfirmUpload never landed (quota overage, suspended tenant, network drop). Set to `0` to disable the in-process loop — K8s deploys that prefer dedicated CronJob scheduling set this to 0 and run `/app/orphan-gc` externally. |
| `GC_PENDING_UPLOAD_TTL_HOURS`         | `168`    | Cooldown applied before a pending-upload row is considered an orphan. Default 7 days matches the trash / recycle-bin retention window. Tightening below the presigned-URL expiry (15 minutes) risks racing a still-uploading client. |

### Two-factor authentication (TOTP)

ZK Drive ships a built-in RFC 6238 TOTP second factor for every user
(WS-19). It is opt-in per user from the account settings page, and
opt-in per workspace via an admin toggle that forces every member to
enroll before completing login.

**Threat model.** WS-5 (bcrypt cost 12) and WS-1 (Redis-backed session
revocation) closed the password-related auth gaps, but a leaked DB
row, a phished password, or password reuse from another breach still
fully owns the account. TOTP adds a possession factor: an attacker
holding only the password cannot complete login without also holding
the device that owns the shared secret.

**Encryption at rest.** TOTP secrets are stored encrypted with the
same `internal/crypto.Codec` (AES-256-GCM keyed via
`CREDENTIAL_ENCRYPTION_KEY`) that protects per-tenant storage
credentials. Operators rotating the key already have the runbook
from WS-13.

**Recovery codes.** 10 codes are generated at enrollment finalize,
shown to the user exactly once, then bcrypt-hashed (cost 12) before
commit. Codes are normalised to lowercase and dash-separated on
input, so the user can type "XB-4Q-9Z-PM-TK", "xb4q9zpmtk", or
"xb 4q 9z pm tk" interchangeably. Burning a code marks
`used_at` (we never delete the row) so audit queries can prove a
recovery code was the second factor on a given session. If the user
runs low (≤ 2 remaining) the account-settings page warns them; the
only way to mint a fresh set is to Disable and re-enroll.

**Replay protection.** Each successful Verify stamps
`user_totp_credentials.last_used_at` with the accepted code's 30s
period boundary. The verifier rejects any subsequent code whose
period start is `<=` last_used_at, so a code observed by a MITM
within its 30s window cannot be replayed.

**Workspace policy.** Admins flip `workspaces.mfa_required` via
`PATCH /api/admin/workspace/mfa-policy`. The transition is audited
(`auth.mfa_policy_change`). When the policy is on, a user without
an enrolled credential receives a `purpose=mfa_enroll` token at
login that authorises ONLY the enrollment endpoints — the user
cannot reach any data-plane handler until enrollment is finalised.
Disabling the policy does NOT delete any user's enrolled credential;
that user remains protected by their second factor, but new users
are no longer forced to enroll.

**Audit trail.** Five events are logged with the standard audit
shape (workspace, actor, request IP / UA): `auth.mfa_enroll`,
`auth.mfa_verify`, `auth.mfa_recovery_use`, `auth.mfa_disable`,
`auth.mfa_policy_change`. The `auth.login` event also records
`factor=totp` or `factor=recovery_code` so an investigator can
distinguish the two paths.

**Lost authenticator.** The user should burn a recovery code on
their next login (it goes through the same Verify path as a TOTP
code). After signing in they can Disable 2FA from settings (the
disable endpoint re-verifies the password to prevent a stolen
session token from quietly downgrading their auth posture), then
re-enroll a new authenticator and receive a fresh recovery set. If
the user has lost both the authenticator and all recovery codes, a
workspace admin can use `PATCH /api/admin/users/{id}` to deactivate
and re-activate the account (manual identity-proofing process —
deliberately not a 1-click button on the admin page).

### Searchable file types

The search endpoint (`GET /api/search?q=...`) queries Postgres
full-text search over file names, tag lists, AND file body content
(`files.content_text`). The index worker (`drive.search.index`
subject) is responsible for populating `content_text` after each
successful upload. The extractor lives in `internal/index` and
supports the following mime types:

| Mime type                                                                   | Extractor          | Notes                                                                                                                                                                                                                              |
| --------------------------------------------------------------------------- | ------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `text/*`                                                                    | pass-through       | Body bytes are written verbatim (UTF-8 truncated to 4 MiB on a rune boundary).                                                                                                                                                     |
| `application/json`, `application/xml`                                       | pass-through       | Same as `text/*`.                                                                                                                                                                                                                  |
| `application/pdf`                                                           | `pdftotext`        | Shells out to `pdftotext` (part of `poppler-utils`, GPL — used as a subprocess, not linked). Same package the preview pipeline already requires for `pdftoppm`, so a host with PDF previews enabled also has PDF text extraction. |
| `application/vnd.openxmlformats-officedocument.wordprocessingml.document` (.docx) | pure Go            | `archive/zip` opens the .docx, `encoding/xml` streams `word/document.xml`, and visible `<w:t>` text runs are concatenated with `<w:tab>` / `<w:br>` rendered as `\t` / `\n` and `<w:p>` paragraphs separated by newlines.            |

Files of any other mime type (images, video, archives, binaries) are
still searchable by **name** and **tag** — the index worker simply
acks the message with no body content recorded. The same holds for
strict zero-knowledge folders: their bytes never reach the server,
so `content_text` stays empty regardless of mime type, and the
search index covers names + tags only.

**Graceful skip semantics.** If `pdftotext` is not installed on the
worker host (e.g. operators who strip `poppler-utils` from a custom
image), PDF extraction returns the same "unsupported mime" signal
as an image upload — the message is acked and the file remains
searchable by name. This is the same graceful-degrade pattern the
preview pipeline already uses for `pdftoppm`. The official ZK Drive
Dockerfile installs `poppler-utils` so both extractors and previews
work out of the box.

**Limits.** The extractor reads at most 4 MiB of text per file
(`MaxIndexBytes`), so very large PDFs / DOCX get truncated to the
first 4 MiB of extracted body. DOCX archives are also bounded at
64 MiB uncompressed XML (`docxMaxUncompressedBytes`) to defend
against zip-bomb inputs.

### Transactional email (guest-invite delivery)

ZK Drive sends a templated email to every guest invitee created via
`POST /api/guest-invites` (WS-21). Closes the historical gap where
external invitees were never told about the invite — the row was
written to the database, but the user only found out via Slack /
verbal handoff.

**Required env vars** (omit any one to leave email disabled — the
server boots cleanly in disabled mode and logs a single
`transactional email DISABLED` warning at startup so operators see
the gap at deploy time, not when the first invitee fails to arrive):

| Variable             | Purpose                                                                                                            |
| -------------------- | ------------------------------------------------------------------------------------------------------------------ |
| `PUBLIC_URL`         | Canonical externally-reachable base URL of the frontend (e.g. `https://drive.example.com`). Used to compose `/invites/{id}` links inside the email. Trailing slashes are normalised. |
| `SMTP_HOST`          | Hostname of the SMTP relay. Anything that speaks SMTP-AUTH works (Postmark, Mailgun, AWS SES, Gmail App Passwords, corporate Exchange).  |
| `SMTP_FROM_ADDRESS`  | Envelope sender (`MAIL FROM`) AND From-header address. Required when `SMTP_HOST` is set.                            |

Optional:

| Variable                            | Default     | Purpose                                                                                            |
| ----------------------------------- | ----------- | -------------------------------------------------------------------------------------------------- |
| `SMTP_PORT`                         | `587`       | TCP port. `465` = implicit TLS, `587` = STARTTLS, `25`/`2525` = plain (dev only).                  |
| `SMTP_USERNAME`                     | empty       | SMTP-AUTH username (PLAIN). Skipped when both username + password are empty (anonymous relay).     |
| `SMTP_PASSWORD`                     | empty       | SMTP-AUTH password.                                                                                |
| `SMTP_FROM_NAME`                    | empty       | Display name in From header (`"ZK Drive" <noreply@…>`).                                            |
| `SMTP_TLS_MODE`                     | `starttls`  | One of `implicit`, `starttls`, `none`. `none` is plain text — local dev only.                      |
| `SMTP_TLS_SERVER_NAME`              | `SMTP_HOST` | SNI / cert-verify hostname override. Set this when the relay is reachable by IP but presents a hostname certificate. |
| `SMTP_TLS_INSECURE_SKIP_VERIFY`     | `false`     | Disable certificate verification. Operators with self-signed dev relays only — keep `false` in production. |

**Delivery model.** Email send is **best-effort**: a relay outage
does NOT roll back the invite row. The HTTP response is the same
either way (`201 Created` with the new invite). The audit log
records the outcome (`ok`, `smtp_error`, `template_error`,
`address_invalid`, `disabled`) per invite under the
`sharing.guest_invite_emailed` action, so operators can join
"invite created → email delivered" on `resource_id` and surface
undelivered invites in compliance reports.

**Metrics.** `zkdrive_email_sent_total{template, outcome}` is
emitted on every `Send` attempt. Cardinality is bounded: one label
combination per (template, outcome) pair, no recipient/inviter
fields. Use this to alert on a sustained `smtp_error` rate or a
non-zero `template_error` rate (the latter is always a server
bug — should be 0 in steady state).

**Provider notes.**

- **Postmark**: `SMTP_HOST=smtp.postmarkapp.com`, `SMTP_PORT=587`, `SMTP_USERNAME` and `SMTP_PASSWORD` are both the server token. Standard STARTTLS.
- **AWS SES**: `SMTP_HOST=email-smtp.<region>.amazonaws.com`, `SMTP_PORT=587`. Credentials are SES-specific SMTP credentials (generated via the SES console), NOT IAM access keys.
- **Mailgun**: `SMTP_HOST=smtp.mailgun.org`, `SMTP_PORT=587`. Username is `postmaster@<domain>`, password is the SMTP password from the domain settings page.
- **Gmail App Passwords**: `SMTP_HOST=smtp.gmail.com`, `SMTP_PORT=587`. Username is the full Gmail address, password is the 16-char app password — works for low-volume internal tooling only, not customer-facing transactional mail.

**Pre-auth invite preview.** The email links recipients to
`{PUBLIC_URL}/invites/{invite_id}`. The frontend hits
`GET /api/guest-invites/{id}/preview` (public, no auth required —
same RLS-bypass posture as `/api/share-links/{token}`) to fetch
the display-safe projection (workspace name, folder name,
recipient email, role, expiry). Secrets such as the permission row
ID and the inviter user ID are NOT exposed; invite IDs are
UUIDv4 so guessing is infeasible.

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

ZK Drive ships seven binaries from a single container image:

- `/app/migrate` — applies pending SQL migrations to the database and exits.
- `/app/server` — the HTTP API server. Refuses to start if migrations are out
  of date (see `internal/database.MinRequiredMigrationVersion`).
- `/app/worker` — the JetStream consumer / job runner. Same migration
  precondition as the server. Also drives the in-process storage-counter
  reconciler on `RECONCILE_INTERVAL_MINUTES` (default 60) and the in-process
  orphan-object GC on `GC_INTERVAL_MINUTES` (default 360).
- `/app/reconciler` — one-shot CronJob for storage-counter reconciliation.
  Deploys that prefer dedicated CronJob scheduling set the worker's
  `RECONCILE_INTERVAL_MINUTES=0` and run `/app/reconciler` externally.
- `/app/orphan-gc` — one-shot CronJob for orphan-object reclaim. Reclaims
  S3 objects from presigned PUTs that completed but were never confirmed.
  Deploys that prefer dedicated CronJob scheduling set the worker's
  `GC_INTERVAL_MINUTES=0` and run `/app/orphan-gc` externally.
- `/app/audit-archiver` — one-shot CronJob for audit-log cold archival.
  Exports `audit_log` rows older than `AUDIT_LOG_RETENTION_DAYS` to S3 as
  compressed JSONL (one object per workspace per calendar month) and
  deletes them from the hot table. Opt-in via `AUDIT_LOG_ARCHIVE_ENABLED`.
  Has its own migration precondition distinct from the server/worker
  baseline: refuses to start unless migration `027_audit_log_archive_runs`
  has been applied (see
  `internal/database.MinRequiredMigrationVersionAuditArchiver`). Deploys
  enabling the archiver must therefore run `/app/migrate` to HEAD before
  scheduling the CronJob; deploys that do not enable the archiver are
  unaffected and can stay on the older baseline.
- `/app/audit-restore` — read-only CLI for the cold tier. Reads archived
  audit rows back for incident investigation and compliance "produce all
  admin actions in workspace X between two dates" requests. Run ad-hoc.
  Loads configuration via `config.LoadStorageOnly` so it does not require
  `DATABASE_URL` or `JWT_SECRET` — only the S3 group.

### Observability

Every long-running binary exposes a Prometheus scrape surface:

| Binary | Endpoint | Default address | Toggle |
| ------ | -------- | --------------- | ------ |
| `/app/server` | `/metrics` on the main HTTP port | `:8080` (via `LISTEN_ADDR`) | always on |
| `/app/worker` | `/metrics` on a dedicated port | `:9091` | `WORKER_METRICS_ADDR` (set to `off` or empty to disable) |
| `/app/reconciler` | _none — one-shot_ | n/a | K8s Job status is the alerting signal; the worker's in-process reconciler exports the same metric family |
| `/app/orphan-gc` | _none — one-shot_ | n/a | K8s Job status is the alerting signal; the worker's in-process GC loop exports the same metric family |

Exported series (under the `zkdrive_` prefix):

- `zkdrive_http_requests_total{method, route, status}` — counter
- `zkdrive_http_request_duration_seconds{method, route}` — histogram
- `zkdrive_http_in_flight_requests` — gauge
- `zkdrive_worker_jobs_total{subject, result}` — counter (`result` ∈ `ok|skip|error|dropped`)
- `zkdrive_worker_job_duration_seconds{subject}` — histogram
- `zkdrive_reconciler_runs_total{result}` — counter
- `zkdrive_reconciler_workspaces_scanned_total` — counter
- `zkdrive_reconciler_workspaces_updated_total` — counter
- `zkdrive_reconciler_drift_bytes_total` — counter
- `zkdrive_reconciler_workspace_errors_total` — counter
- `zkdrive_reconciler_run_duration_seconds` — histogram
- `zkdrive_gc_runs_total{result}` — counter (`result` ∈ `ok|error|cancelled`)
- `zkdrive_gc_workspaces_scanned_total` — counter
- `zkdrive_gc_orphans_found_total` — counter (orphan presigned uploads the scan returned)
- `zkdrive_gc_orphans_deleted_total` — counter (orphan rows reclaimed; diverges from `orphans_found` only on confirm-races, which are benign)
- `zkdrive_gc_objects_deleted_total` — counter (S3 objects deleted; diverges from `orphans_deleted` when per-workspace storage path is unconfigured or transiently failing)
- `zkdrive_gc_workspace_errors_total` — counter
- `zkdrive_gc_run_duration_seconds` — histogram
- `zkdrive_audit_archive_runs_total{result}` — counter (`result` ∈ `ok|partial|error|cancelled`)
- `zkdrive_audit_archive_buckets_total{result}` — counter (per `(workspace, year-month)` bucket; `result` ∈ `ok|partial|error`). `partial` means the bucket exceeded `AUDIT_LOG_ARCHIVE_MAX_ROWS_PER_BATCH` and split across multiple pages, of which some completed (S3 PUT + RecordRun + hot-tier DELETE) before a later page failed; the durably-committed pages' rows + bytes ARE included in `zkdrive_audit_archive_rows_total` / `_bytes_total`. The remaining rows are still in the hot tier and will be picked up by the next run.
- `zkdrive_audit_archive_rows_total` — counter (audit rows successfully moved to cold tier)
- `zkdrive_audit_archive_bytes_total` — counter (uncompressed JSONL bytes uploaded)
- `zkdrive_audit_archive_run_duration_seconds` — histogram
- `zkdrive_db_pool_*` — pgxpool live stats (total / acquired / idle / max / acquire count / acquire duration)
- `zkdrive_redis_pool_*` — go-redis client pool stats (server only — worker does not use Redis directly today)
- `go_*` / `process_*` — runtime + process collectors from `prometheus/client_golang`

Example Prometheus scrape config (drop into `prometheus.yml`):

```yaml
scrape_configs:
  - job_name: zk-drive-server
    metrics_path: /metrics
    static_configs:
      - targets: ['zk-drive-server:8080']
  - job_name: zk-drive-worker
    metrics_path: /metrics
    static_configs:
      - targets: ['zk-drive-worker:9091']
```

The `/metrics` endpoint is intentionally **unauthenticated**: the Go runtime
and pool collectors expose modest internal state appropriate for an operator
metrics network but not for the public internet. Production deployments MUST
firewall `/metrics` off via a Network Policy or ingress allow-list. The
server pod's `LISTEN_ADDR` is typically exposed publicly behind a TLS
ingress; if you don't split `/metrics` onto a separate listener, your
ingress controller must explicitly block `/metrics` from external traffic.
Splitting onto a separate port (e.g. by reverse-proxying everything except
`/metrics` from the public ingress) is the simpler posture and matches the
worker binary's default of `:9091`.

#### Distributed tracing

Both `/app/server` and `/app/worker` emit OpenTelemetry spans over
**OTLP/HTTP** to whatever collector / backend you point them at — Jaeger,
Grafana Tempo, Honeycomb, Datadog (via the OTLP gateway), New Relic,
SigNoz, or an in-cluster OTel Collector that fans out to multiple backends.
The instrumentation covers chi HTTP handlers (named by route pattern, not
raw path, so cardinality stays bounded), pgxpool queries, redis commands,
NATS JetStream publish + consume (parent-child linked across the message
boundary), and SMTP sends — i.e. every blocking dependency a request can
hit.

| Env var | Default | What it does |
| ------- | ------- | ------------ |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | _unset_ | Collector URL (e.g. `https://otlp.honeycomb.io:443` or `http://otel-collector.observability.svc:4318`). **Empty = tracing disabled (no-op provider).** Boot log announces the state explicitly. |
| `OTEL_EXPORTER_OTLP_HEADERS` | _unset_ | Comma-separated `key=value` pairs added to every export request (e.g. `x-honeycomb-team=<your-team-key>` for Honeycomb, `dd-api-key=<key>` for Datadog gateway). |
| `OTEL_EXPORTER_OTLP_INSECURE` | `false` | Set to `true` to skip TLS verification — local collectors only. |
| `OTEL_EXPORTER_OTLP_COMPRESSION` | `gzip` | `gzip` or `none`. `gzip` is the OTel default and roughly halves egress bytes for typical span shapes. |
| `OTEL_SERVICE_NAME` | `zk-drive` | `service.name` resource attribute. The server and worker share this so trace backends present them as one logical service; the `service.instance.id` (auto-generated UUID per process) distinguishes the two. |
| `OTEL_DEPLOYMENT_ENVIRONMENT` | _unset_ | `deployment.environment` resource attribute, e.g. `production` / `staging`. Omitted entirely when unset (rather than emitted as the empty string) so dashboards filtering on `env=""` don't bucket everything together. |
| `OTEL_TRACES_SAMPLER_ARG` | `0.1` | Root-span sample ratio. **10% is the production default.** Set `1.0` in dev / staging for full visibility; `0.0` keeps propagation working but stops root sampling (useful when you want correlation IDs but no exporter cost). Out-of-range values are clamped at SDK construction with a startup warning. |

The sampler is **parent-based**: if the request arrives carrying a sampled
W3C trace-context (i.e. a frontend or upstream load balancer made the
sample decision), zk-drive honours it. Root spans (no upstream context)
use `OTEL_TRACES_SAMPLER_ARG`. This is what lets a "force sample for this
specific user" header from your edge propagate through the whole stack.

**Trace ↔ log correlation.** When tracing is enabled, every access-log
line and every handler-emitted slog record carries `trace_id` and
`span_id` fields alongside the existing `request_id`. In any backend that
supports the link (Honeycomb, Datadog, Grafana Loki + Tempo, Splunk
Observability) you can click a slog record and jump straight to the
trace, or click a span and jump to the related log lines — without having
to manually correlate via `request_id`.

**Disabled-but-instrumented.** Leaving `OTEL_EXPORTER_OTLP_ENDPOINT`
unset installs a no-op tracer provider. All instrumented code paths
still compile and execute (Span creation, attribute setting, propagator
inject/extract are all silent no-ops on the no-op SDK), so flipping the
env var on a running pod is sufficient to start emitting spans on the
next request without any code changes — and the propagator is installed
either way, so distributed correlation IDs continue to flow through your
deployment even with the local exporter disabled. The no-op provider
also skips header injection on outgoing NATS messages (the propagator
no-ops when the span context is invalid), so the NATS 2.2+ requirement
below applies **only** when tracing is enabled.

**NATS server version requirement (tracing-enabled deployments).**
When tracing is enabled, the publisher (server) injects the W3C
`traceparent` (and optional `tracestate` / `baggage`) header onto every
JetStream message it publishes so the consumer (worker) can recreate
the parent-child span link. NATS server **2.2 or newer** is required
to accept messages with headers — older servers reject them at the
protocol level with `ErrHeadersNotSupported`, which would surface as a
publish failure on every async job dispatch. The JetStream features we
already use (subjects, durable consumers, ack policies) require 2.3+
in any case, so this is consistent with the existing minimum version.
The official `nats:2.10-alpine` image used in `deploy/docker-compose.yml`
and our Helm values satisfies this comfortably.

Quick example — point a local server at the all-in-one Grafana Tempo
container for end-to-end testing:

```
docker run -d --name=tempo \
  -p 3200:3200 -p 4318:4318 \
  grafana/tempo:latest \
  -config.file=/etc/tempo.yaml -target=all

OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 \
OTEL_TRACES_SAMPLER_ARG=1.0 \
OTEL_DEPLOYMENT_ENVIRONMENT=dev \
./server
```

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

### Audit log retention and cold archival

ZK Drive's `audit_log` table records security-sensitive events (login,
SSO link, MFA lifecycle, permission grant/revoke, admin user
management, workspace settings changes, retention policy changes,
billing changes, guest-invite delivery outcomes). For compliance
reporting (SOC2 Type II, HIPAA, GDPR) operators typically need a
retention window of 1 to 6 years — well beyond what the hot Postgres
table should carry.

The `audit-archiver` binary (Dockerfile builds it alongside the
server / worker / reconciler / orphan-gc) is a one-shot CronJob that:

1. Selects audit rows older than `AUDIT_LOG_RETENTION_DAYS`.
2. Groups them by `(workspace_id, year-month)`.
3. For each bucket, uploads the rows to S3 as compressed JSONL at
   the key `{prefix}{workspace_id}/{YYYY-MM}/{batch_id}.jsonl.gz`.
   A single (workspace, month) bucket that exceeds
   `AUDIT_LOG_ARCHIVE_MAX_ROWS_PER_BATCH` is split into multiple
   pages, each with its own `batch_id` UUID so pages within the
   same bucket never overwrite each other in S3.
4. Records each batch in `audit_log_archive_runs` (run_id, workspace_id,
   cutoff_time, year_month, archive_object_key, rows_archived,
   bytes_uploaded, started_at, completed_at, error_message).
5. Deletes the archived rows from the hot table.

The archiver is **idempotent**: a crash between steps 3-5 leaves the
S3 object + the `audit_log_archive_runs` row committed but the rows
still in the hot tier. The next run re-uploads the same rows under
a fresh `batch_id`-suffixed key; the cold tier may carry duplicate
objects which the restore CLI dedupes by row id.

#### Configuration

| Variable | Default | Purpose |
| -------- | ------- | ------- |
| `AUDIT_LOG_ARCHIVE_ENABLED` | `false` | **Opt-in safety floor.** The archiver refuses to delete rows when unset. Set to `true` after you've confirmed retention + bucket settings. |
| `AUDIT_LOG_RETENTION_DAYS` | `90` | Rows older than `now() - retention_days` are eligible for archival. Clamped to `[7, 3650]` (7 days – 10 years). Values below the 7-day safety floor are silently raised to 7 with a startup-log notice so a typo like `5` doesn't aggressively prune legitimately-recent audit history; `0`/empty falls back to `90` (the default). |
| `AUDIT_LOG_ARCHIVE_PREFIX` | `audit-archive/` | S3 key prefix. Normalised to a single trailing slash. |
| `AUDIT_LOG_ARCHIVE_BUCKET` | _(empty — use `S3_BUCKET`)_ | Optional dedicated bucket with Glacier transition / object-lock retention rules. |
| `AUDIT_LOG_ARCHIVE_MAX_ROWS_PER_BATCH` | `50000` | Batch size for the FetchBatch → encode → PUT loop. ~25 MB uncompressed, ~5 MB compressed per batch. |

Typical compliance windows:

- **SOC2 Type II** — 1 year minimum (`AUDIT_LOG_RETENTION_DAYS=365`).
- **HIPAA** — 6 years from the date of creation
  (`AUDIT_LOG_RETENTION_DAYS=2190`).
- **GDPR** — retain as long as the lawful basis applies; revisit on
  each policy change.

When the binary runs with `AUDIT_LOG_ARCHIVE_ENABLED=false` (the
default), it logs `audit-archiver disabled by AUDIT_LOG_ARCHIVE_ENABLED;
exiting zero` and the K8s Job completes successfully — no rows
deleted, no objects uploaded. This is the safe default for
deployments that haven't yet configured a cold-tier bucket.

The K8s CronJob ships in `deploy/k8s/audit-archiver-cronjob.yaml`
with a nightly schedule (`47 3 * * *`) staggered against the
reconciler (`17 */1 * * *`) and orphan-gc (`37 */6 * * *`) so
Postgres + S3 are not hit by all three background jobs at the same
minute. Skip a missed tick rather than overlapping
(`concurrencyPolicy: Forbid`); `activeDeadlineSeconds: 14400`
caps a single run at 4h to match the histogram's top bucket.

#### Restore workflow

The `audit-restore` binary reads archived rows back for incident
investigation. It is read-only — no S3 PUT, no Postgres DELETE — so
operators can run it freely against any historical period.

```
docker run --rm \
  -e S3_ENDPOINT=https://... \
  -e S3_ACCESS_KEY=... \
  -e S3_SECRET_KEY=... \
  -e S3_BUCKET=zk-drive-prod \
  -e AUDIT_LOG_ARCHIVE_BUCKET=zk-drive-audit-archive \
  -e AUDIT_LOG_ARCHIVE_PREFIX=audit-archive/ \
  ghcr.io/kennguy3n/zk-drive:<version> /app/audit-restore \
    --workspace 00000000-1111-2222-3333-444444444444 \
    --from 2024-01-01T00:00:00Z \
    --to   2024-03-31T23:59:59Z \
    --output /tmp/workspace-audit-q1-2024.jsonl
```

`audit-restore` is read-only against S3 and intentionally does NOT
require `DATABASE_URL` or `JWT_SECRET` — on-call engineers responding
to an incident or compliance request can run it with only S3
credentials in hand. (The archiver and server binaries still require
both, since they touch Postgres / the request lifecycle.)

The output is newline-delimited JSON, one row per audit event,
sorted chronologically (`created_at` ASC), deduplicated by row id.
Pipe into `jq` for ad-hoc analysis:

```
jq -r 'select(.action | startswith("admin.")) | [.created_at, .actor_id, .action] | @tsv' \
  workspace-audit-q1-2024.jsonl
```

#### S3 object layout

```
{AUDIT_LOG_ARCHIVE_PREFIX}{workspace_id}/{YYYY-MM}/{batch_id}.jsonl.gz

audit-archive/00000000-1111-2222-3333-444444444444/2024-01/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.jsonl.gz
audit-archive/00000000-1111-2222-3333-444444444444/2024-01/ffffffff-1111-2222-3333-444444444444.jsonl.gz   # duplicate from a crashed earlier run
audit-archive/00000000-1111-2222-3333-444444444444/2024-02/cccccccc-dddd-eeee-ffff-000000000000.jsonl.gz
```

Each object is GZIP-compressed JSONL. The `batch_id` UUID suffix
makes every PUT idempotent (the same row content uploaded twice
produces two distinct objects; the restore tool dedupes by row id).

### Outbound webhooks (WS-24)

Workspace admins can register HTTPS endpoints that receive
notifications when state changes inside the workspace. The
subscriber's URL is hit with a JSON `POST`; payloads are signed with
an HMAC-SHA256 so the subscriber can confirm the request came from
zk-drive. Subscriptions are stored in `webhook_subscriptions` (one
per (workspace, URL, event_type) triple) and every attempt is
recorded in `webhook_deliveries` for replay / debugging.

#### Event catalogue

Each event ships as a versioned envelope. The `data` payload differs
by event type, but the envelope is stable:

```
{
  "id": "<event uuid>",          // stable across retries; subscribers de-dupe on this
  "type": "file.upload.confirmed",
  "workspace_id": "<workspace uuid>",
  "actor_id": "<user uuid|null>",
  "created_at": "2026-05-21T23:24:11.123456Z",
  "data": { /* event-type-specific payload */ }
}
```

| Event type                  | When it fires                                                  | `data` fields                                                  |
| --------------------------- | -------------------------------------------------------------- | -------------------------------------------------------------- |
| `file.upload.confirmed`     | A presigned upload completes and the file row is committed     | `file_id`, `version_id`, `folder_id`, `name`, `mime_type`, `size_bytes` |
| `file.deleted`              | A file is soft-deleted via API or bulk-delete                  | `file_id`, `folder_id`, `name`                                 |
| `file.restored`             | A previously soft-deleted file is restored                     | `file_id`, `folder_id`, `name`                                 |
| `permission.granted`        | A file or folder permission is added                           | `resource_type`, `resource_id`, `grantee_id`, `role`           |
| `permission.revoked`        | A file or folder permission is removed                         | `resource_type`, `resource_id`, `grantee_id`, `role`           |
| `member.joined`             | An admin invites a new user to the workspace                   | `user_id`, `email`, `role`                                     |
| `member.removed`            | A user is deactivated from the workspace                       | `user_id`, `email`, `role`                                     |

#### HTTP headers on every request

| Header                  | Purpose                                                                                              |
| ----------------------- | ---------------------------------------------------------------------------------------------------- |
| `Content-Type`          | Always `application/json; charset=utf-8`.                                                            |
| `User-Agent`            | `zkdrive-webhooks/<version>` — useful for filtering test traffic in your reverse proxy.              |
| `X-ZkDrive-Signature`   | `t=<unix>,v1=<hex>` HMAC-SHA256 over `<unix>.<body>` keyed on the subscription secret. See below.     |
| `X-ZkDrive-Event-Id`    | Stable across retries. Subscribers de-dupe on this.                                                  |
| `X-ZkDrive-Event-Type`  | Same as `type` in the body — lets a subscriber route to a per-type handler without parsing the body. |
| `X-ZkDrive-Delivery-Id` | Unique per attempt. Useful for cross-referencing the per-attempt row in `/api/admin/webhooks/{id}/deliveries`. |

#### Verifying the signature

The signature is the canonical Stripe-style scheme. Pseudocode for a
subscriber:

```python
import hmac, hashlib, time

def verify(secret: str, header: str, body: bytes) -> bool:
    parts = dict(p.split("=", 1) for p in header.split(",") if "=" in p)
    ts = int(parts["t"])
    if abs(time.time() - ts) > 300:        # 5-minute window
        return False
    expected = hmac.new(secret.encode(), f"{ts}.".encode() + body,
                        hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, parts["v1"])
```

The 5-minute window matches Stripe's default and is enforced by the
reference `Verify` implementation in `internal/webhooks/signer.go`.

#### Reliability semantics

- **At-least-once.** Subscribers MUST be idempotent on
  `X-ZkDrive-Event-Id` — a network blip can produce a duplicate
  delivery. The signature does NOT prove uniqueness; the event id
  does.
- **Retries: 5 attempts total** (initial + 4) on the schedule
  `0s, 1s, 2s, 4s, 8s` for `2xx`-checking. Non-2xx responses,
  network errors, and `outcome=blocked` all retry; only 2xx is
  treated as success.
- **Auto-pause after 50 consecutive failures.** A persistently broken
  endpoint is paused (admin sees `auto_paused_at` on the
  subscription). Re-enable via
  `POST /api/admin/webhooks/{id}/resume` — the consecutive-failure
  counter resets to zero on resume.
- **Per-workspace cap: 20 active subscriptions.** A request that
  would push the workspace over the cap returns `409 Conflict`.
- **SSRF defence.** URLs are syntactically validated (HTTPS only in
  production, no userinfo) and the host is resolved at every
  delivery attempt to defend against DNS rebinding. Any IP in
  loopback, RFC1918, link-local, RFC6598, IPv6 ULA, multicast, or
  the documentation ranges is rejected with `outcome=blocked` and
  no request is sent.

#### Admin REST surface

All endpoints sit under `/api/admin/webhooks` and require the
`admin` role on the workspace.

| Method   | Path                                | Purpose                                                        |
| -------- | ----------------------------------- | -------------------------------------------------------------- |
| `POST`   | `/api/admin/webhooks`               | Create a subscription. Response includes the secret **once**.   |
| `GET`    | `/api/admin/webhooks`               | List subscriptions (secrets redacted).                          |
| `GET`    | `/api/admin/webhooks/{id}`          | Get a single subscription (secret redacted).                    |
| `DELETE` | `/api/admin/webhooks/{id}`          | Hard-delete a subscription.                                     |
| `GET`    | `/api/admin/webhooks/{id}/deliveries` | List recent attempt rows for the subscription.                |
| `POST`   | `/api/admin/webhooks/{id}/test`     | Enqueue a synthetic event to verify connectivity end-to-end.    |
| `POST`   | `/api/admin/webhooks/{id}/resume`   | Re-activate an auto-paused subscription.                        |

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
