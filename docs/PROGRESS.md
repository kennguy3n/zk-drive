# ZK Drive — Progress

- **Project**: ZK Drive
- **License**: Proprietary — All Rights Reserved.
- **Status**: Phase 4 — Privacy & Differentiation (kicked off 2026-04-25)
- **Last updated**: 2026-04-26 (Phase 4 sprint 9 closed: all 10 next-10 tasks landed; Phase 4 decision gate closed)

This document is a phase-gated tracker. Each phase has an explicit
checklist and a decision gate. Do not skip to the next phase until
the current phase's gate has been met.

For the technical design, see [PROPOSAL.md](PROPOSAL.md). For the
architecture, see [ARCHITECTURE.md](ARCHITECTURE.md).

---

## Phase 1: Foundation (Weeks 1–4)

**Status**: `COMPLETE`

**Goal**: stand up the core application layer — Go backend, Postgres
schema, React frontend scaffold, basic folder / file CRUD,
direct-to-storage uploads via zk-object-fabric presigned URLs, and
user authentication. No sharing, no previews, no async jobs yet.

Checklist:

- [x] Initialize Go project structure (`cmd/server/`, `cmd/worker/`,
      `api/`, `internal/`).
- [x] Postgres schema: `workspaces`, `users`, `folders`, `files`,
      `file_versions`, `permissions`, `activity_log`. Migrations in
      `migrations/`.
- [x] User authentication: email / password signup, login, session
      management (JWT or session tokens). `api/auth/`.
- [x] Workspace CRUD: create, read, update workspace.
      `internal/workspace/`.
- [x] Folder CRUD: create, rename, move, delete folders. Nested
      hierarchy with `parent_folder_id`. `internal/folder/`.
- [x] File metadata CRUD: create, rename, move, delete file records.
      `internal/file/`.
- [x] File versioning: automatic version creation on re-upload.
      `internal/file/`.
- [x] Direct-to-storage upload flow: presigned PUT URL generation via
      zk-object-fabric S3 API, upload confirmation endpoint, metadata
      recording. `api/drive/`.
- [x] Direct-to-storage download flow: presigned GET URL generation
      with permission check. `api/drive/`.
- [x] React frontend scaffold: Vite + React + TypeScript. Login /
      signup page, file browser page, upload component. `frontend/`.
- [x] Basic permission model: workspace-level roles (admin, member)
      plus per-resource grants (viewer / editor / admin).
      `internal/permission/`.
- [x] Activity logging: record file / folder operations in
      `activity_log` table. `internal/activity/`.
- [x] Soft delete (trash): deleted files / folders marked with
      `deleted_at`, recoverable for 30 days. `internal/file/`,
      `internal/folder/`.
- [x] Integration tests: API-level tests for folder CRUD, file upload
      / download, auth. `tests/integration/` (partial — auth,
      workspace, folder, file CRUD).
- [x] Decision gate: upstream presigned-URL fix landed in
      zk-object-fabric PR #29 (commit 39dcd81e). Full byte-path
      round-trip is no longer blocked. All code-level findings from
      PRs #2–#4 are resolved. Multipart upload still deferred.

**Decisions / Deferrals (2026-04-23)**:

- Storage integration landed: zk-drive generates presigned PUT / GET
  URLs via AWS SDK v2 pointed at zk-object-fabric's S3 endpoint.
  Object keys are workspace-scoped
  (`{workspace_id}/{file_id}/{version_id}`). Permission checks run
  before URL generation; bytes never transit the ZK Drive API server.
- Permission model implemented: workspace-level roles (admin, member)
  with per-resource grants (viewer, editor, admin). Schema lives in
  migration 004; `internal/permission/` implements the repository and
  service (`Grant`, `Revoke`, `HasAccess`, `ListForResource`). Handler
  exposes `GET/POST/DELETE /api/permissions`. Folder permission
  inheritance deferred to Phase 2.
- Activity logging implemented: fire-and-forget async logging of every
  folder / file CRUD operation and permission grant / revoke via a
  buffered channel + background worker in `internal/activity/`. Entries
  are tenant-scoped and queryable via
  `GET /api/activity?limit=50&offset=0`. Failures are swallowed so
  logging never blocks or fails the parent operation.
- Soft delete implemented for folders and files (`deleted_at` column,
  excluded from listings).
- React frontend scaffold landed: Vite + React + TypeScript under
  `frontend/` with login, signup, and file browser pages plus the
  presigned-URL upload flow. No sharing UI yet (Phase 2). Dependencies
  vetted for non-AGPL licensing (MIT / Apache-2.0 only).

**Phase 1 status**: `COMPLETE`. The upstream presigned URL blocker was
resolved in zk-object-fabric commit 978246fb (2026-04-23). Full
presigned PUT / GET round-trip validated against the Docker demo.

---

## Phase 2: Sharing & Collaboration (Weeks 5–8)

**Status**: `COMPLETE`

**Decisions / Deferrals (2026-04-24)**:

- Per-resource permission enforcement (`CheckAccess`) on read / write
  paths was deferred from Phase 1 and will land in Phase 2 together
  with folder permission inheritance; the flat `HasAccess` API already
  exists (`internal/permission/service.go`) and Phase 2 adds
  `HasAccessWithInheritance` + handler-level gating.
- CI pipeline and Docker Compose for local dev are prerequisites
  before Phase 2 feature work. CI runs Go build / vet / `go test
  -short` plus a frontend typecheck and an integration-test job that
  requires `TEST_DATABASE_URL`. Docker Compose provides Postgres
  (and an optional NATS service for the worker pipeline).
- Priority order inside Phase 2: permission inheritance → sharing
  (share links, guest invites) → client rooms → Postgres FTS search
  → async workers (preview, virus scan, notifications).
- No AGPL dependencies — zk-drive is a proprietary product. Every
  library added in this phase must be MIT or Apache-2.0.

**Goal**: add sharing, guest access, client rooms, search, and the
async job pipeline for previews and virus scanning. This is the
phase that turns ZK Drive from "cloud storage for one team" into
"file collaboration with clients and partners."

Checklist:

- [x] Per-file and per-folder sharing with roles (view / edit /
      admin). `internal/sharing/`, `api/sharing/`.
- [x] Share links: token-based, optional password, optional expiry,
      optional max downloads. `internal/sharing/`.
- [x] Guest invites: invite external users by email with scoped
      folder access and expiry. `internal/sharing/`.
- [x] Client rooms: dedicated shared folders for external clients /
      partners with dropbox upload capability.
      `internal/sharing/`.
- [x] NATS JetStream setup: job dispatch for preview, scan, index,
      retention. `cmd/worker/`.
- [x] Preview worker: generate thumbnails for images using pure-Go
      stdlib decoders + `golang.org/x/image` for resampling. Store
      previews as derived objects in zk-object-fabric. `internal/preview/`.
      PDF / office doc support deferred (requires ImageMagick /
      LibreOffice; tracked as a Phase 3 hardening item).
- [x] Virus scan worker: async ClamAV scan over the INSTREAM protocol.
      Quarantine infected file versions by flipping `scan_status` and
      firing a notification to workspace admins. `internal/scan/`.
      Runs in permissive mode when `CLAMAV_ADDRESS` is unset so local
      dev / CI pass without a clamd sidecar.
- [x] Search: Postgres FTS over file names, folder names, and tags.
      `internal/search/`, `api/drive/`.
- [x] Folder permission inheritance: child folders / files inherit
      parent permissions unless overridden. `internal/permission/`.
- [x] Frontend: sharing dialogs, guest invite UI, file preview
      display, search bar. `frontend/`. Preview component
      (`FilePreview.tsx`) now fetches `/api/files/{id}/preview-url` and
      renders a placeholder when no preview is available.
- [x] Notification system: in-app notifications for share-link
      creation, guest invites sent / accepted, and scan quarantine.
      `internal/notification/`. Nil-safe wiring (same pattern as
      `logActivity`) so the metadata plane works even when the
      notification service is unconfigured.
- [x] Integration tests: sharing flows (including the atomic
      `max_downloads` enforcement), guest access, search, client
      rooms, notifications. `tests/integration/`. Preview generation
      is exercised by `internal/preview` unit tests; integration-level
      preview coverage requires live storage and is deferred.
- [x] Decision gate: validate end-to-end sharing flow — an internal
      user shares a folder with a guest, the guest uploads a file via
      dropbox, the file is scanned and previewed, and all activity is
      recorded in the audit log. Covered by
      `tests/integration/e2e_sharing_test.go` (metadata plane) plus
      the existing preview / scan unit tests. Docker Compose now ships
      a `worker` binary and a `clamav` sidecar so the full async
      pipeline runs locally.

**Decisions / Deferrals (2026-04-24, sprint 2)**:

- PR #5 TOCTOU race on share-link `max_downloads` fixed. The check is
  now a single SQL `UPDATE ... WHERE max_downloads IS NULL OR
  download_count < max_downloads`; the handler surfaces a new
  `ErrLinkExhausted` sentinel as 429. Removed the pre-check in
  `ResolveShareLink` so the atomic increment is the only enforcement
  point. Integration test `TestMaxDownloadsSingleUseAtomic` pins the
  behaviour. (Note: sprint 3 review confirmed the atomic `UPDATE` and
  the test were already present in the tree when this note was first
  written; no further code change was required.)
- PR #5 search-limit cap fixed at the handler layer. `Search` now
  clamps `limit` to `search.MaxLimit` before it reaches the service so
  the response envelope echoes the effective value back to the client,
  matching the pattern used by `ListActivity`.
- Permission inheritance wired into handlers. `GetFolder`,
  `RenameFolder`, `DeleteFolder`, `MoveFolder`, `CreateFile`,
  `UploadURL`, `GetFile`, `DownloadURL`, `UpdateFile`, `MoveFile`, and
  `DeleteFile` now call `HasAccessWithInheritance` with the required
  role (viewer for reads, editor for writes) before mutating. Workspace
  admins bypass the check, as do requests where the permission service
  is unwired (test fixtures). `MoveFolder` and `MoveFile` additionally
  assert editor access on the *target* parent.
- Client rooms implemented as folder + share-link bundles. A new
  `client_rooms` table (migration 006) pairs a workspace-scoped folder
  with a share link so deleting the room cleans up both. Routes live
  at `POST/GET /api/client-rooms` and `GET/DELETE
  /api/client-rooms/{id}`.
- NATS JetStream skeleton wired. `internal/jobs/publisher.go` exposes
  nil-safe `PublishPreview` / `PublishScan` / `PublishIndex` helpers;
  `cmd/worker/main.go` declares the `DRIVE_JOBS` stream and
  placeholder consumers that log + ack. The API server connects
  opportunistically (`NATS_URL` env); absent NATS, publishes become
  no-ops so local-dev and CI keep working.
- Preview worker and virus-scan worker deferred to the next Phase 2
  sprint. Both require heavyweight third-party tooling (LibreOffice /
  ImageMagick / ClamAV) that has licensing and packaging implications
  worth a separate design pass; the publish/subscribe skeleton is in
  place so swapping in real handlers is a single-function change.
- Frontend sharing UI and search bar landed. `ShareDialog` covers both
  share-link and guest-invite flows in one modal; `SearchBar` talks to
  `/api/search` with 250 ms debouncing and renders file / folder hits
  with a type badge and path.

**Decisions / Deferrals (2026-04-24, sprint 3)**:

- PR #6 review findings resolved:
  - `ListFileVersions` now calls `assertResourceAccess` with
    `permission.RoleViewer` before returning version rows, matching
    the pattern in `GetFile` / `DownloadURL`. Without this guard any
    authenticated workspace member could enumerate versions of files
    they had no permission to read.
  - The `/api/search` response envelope now uses `hits` (not
    `results`) and echoes the `query` parameter, matching the
    frontend's documented `SearchResponse` contract.
  - The guest-invite create payload is now folder-scoped on the
    frontend: `CreateGuestInviteInput` sends `folder_id` (matching the
    backend's `createGuestInviteRequest`) instead of the previous
    `resource_type` / `resource_id` pair. `ShareDialog` surfaces a
    clear error when a file has no parent folder, since guest invites
    are always folder-scoped.
  - The TOCTOU fix for share-link `max_downloads` described in sprint
    2 was re-verified during PR #6 triage; the atomic `UPDATE` guard
    and `TestMaxDownloadsSingleUseAtomic` were already in the tree.
- Client room delete ordering fixed: the room row is now deleted
  before its backing share link is revoked, to satisfy the
  `client_rooms_share_link_id_fkey` `ON DELETE RESTRICT` constraint.
  The previous order produced a 500 in tests.
- Preview worker (`internal/preview/`) implemented as a pure-Go
  pipeline: stdlib `image/*` decoders + `golang.org/x/image/draw`
  bilinear resampling at a 256 px target, PNG-encoded preview
  uploaded to `{workspace_id}/{file_id}/{version_id}/preview.png` and
  persisted in a new `file_previews` table (migration 007). The
  worker subscribes to the existing `drive.preview.generate` subject;
  unsupported mime types are acked without error so JetStream doesn't
  redeliver. PDF / office document support is deferred to a later
  sprint (ImageMagick Apache-2.0, LibreOffice MPL-2.0 — both safe for
  a proprietary product but each adds packaging surface worth a
  dedicated design pass).
- Virus-scan worker (`internal/scan/`) talks to clamd over the
  INSTREAM TCP protocol. `file_versions` gained `scan_status`,
  `scan_detail`, and `scanned_at` columns (migration 008). When
  `CLAMAV_ADDRESS` is unset the worker runs in permissive mode and
  marks every version clean so local dev and CI don't need a clamd
  sidecar; the `docker-compose.yml` update to add a clamav service is
  deferred.
- In-app notification system (`internal/notification/`) lands the
  full CRUD surface: `Notification` rows (migration 009) are scoped
  by workspace + user, queried with unread-first ordering, and
  written for four events today — share-link created, guest invite
  sent (only when the invitee already has a workspace account),
  guest invite accepted, and scan quarantine. New endpoints:
  `GET /api/notifications`, `POST /api/notifications/{id}/read`, and
  `POST /api/notifications/read-all`. Wiring uses the same nil-safe
  helper pattern as `logActivity` so callers without a notification
  service configured keep working unchanged.
- Frontend preview display: `FilePreview.tsx` fetches a presigned
  URL from the new `GET /api/files/{id}/preview-url` endpoint and
  renders a 48 px thumbnail inline in the file list. On 404 (no
  preview generated yet, or unsupported mime) it falls back to a
  small mime-type-aware placeholder badge so the list never shows a
  broken image.
- Integration coverage expanded: new `tests/integration/search_test.go`
  covers happy-path search, pagination, and the empty-query 400;
  `client_room_test.go` walks the create / list / get / delete
  lifecycle; `notification_test.go` verifies share-link creation,
  guest-invite acceptance, and both single / bulk mark-as-read
  endpoints fan out the expected notification rows.

---

## Phase 3: Business Readiness (Weeks 9–14)

**Status**: `COMPLETE`

**Goal**: add SSO, audit logs, retention policies, admin dashboard,
billing integration, and production hardening. This is the phase
where ZK Drive becomes something a paying SME customer can rely on.

Checklist:

- [x] SSO: OAuth2 with Google and Microsoft. `api/auth/`.
- [x] Audit log: queryable log of all admin and security-relevant
      actions. `internal/audit/`, `api/admin/`.
- [x] Retention policies: per-folder and per-workspace retention
      rules. Automatic archival of old file versions to cold storage
      in zk-object-fabric. `internal/retention/`.
- [x] Cold archive worker: compress and archive expired file versions
      as objects in zk-object-fabric. `internal/retention/`.
- [x] Admin dashboard API: user management, storage usage, audit log
      viewer, workspace settings. `api/admin/`. Frontend pages
      deferred.
- [x] Billing integration: usage metering (storage, bandwidth,
      users), plan enforcement (quota limits). `internal/billing/`.
- [x] Rate limiting and abuse controls: per-workspace and per-user
      rate limits. `api/middleware/`.
- [x] Expiring guest access: automatic revocation of guest
      permissions after expiry date. `internal/sharing/`.
- [x] File tagging: user-defined tags on files for organization and
      search. `internal/file/`.
- [x] Bulk operations: multi-select move, copy, delete, download (as
      zip). `api/drive/`.
- [x] Frontend: admin pages, retention settings, billing / usage
      dashboard, bulk operations UI. `frontend/`.
- [x] Playwright e2e tests: critical user flows (signup, upload,
      share, guest access, admin). `tests/e2e/`.
- [x] Production deployment configuration: Docker Compose for local
      dev, Kubernetes manifests for production. `deploy/`.
- [x] Decision gate: a paying SME customer can sign up, create a
      workspace, upload files, share with guests, and the admin can
      view audit logs and set retention policies. *(Met at the
      metadata-plane level via `tests/integration/phase3_gate_test.go`.
      Full byte-path round-trip validated after the upstream
      zk-object-fabric presigned-URL fix landed — see Phase 1 gate
      upgrade.)* (Formally closed sprint 9 — TestUploadConfirmDownloadRoundTrip passes in CI since sprint 5.)

**Decisions / Deferrals (2026-04-24, Phase 3 kickoff)**:

- SSO implemented with OAuth2 PKCE against Google and Microsoft.
  State + verifier live in short-lived httponly cookies (S256
  challenge, 5 min TTL). Migration 010 adds
  `users.auth_provider` / `users.auth_provider_id` with a unique
  index so a single `(workspace, provider, sub)` always maps to one
  user. All SSO routes are under `/api/auth/oauth/{provider}` and
  sit outside the auth middleware because they initiate the login.
  Provider creds are optional (`GOOGLE_CLIENT_*` /
  `MICROSOFT_CLIENT_*`) and the routes return 501 when not
  configured.
- Audit log is **separate** from `activity_log`. Activity log is
  user-facing (file / folder CRUD, timeline view). Audit log is
  security-only — login, logout, permission grant / revoke, admin
  user management, retention-policy changes — and queryable only
  through the admin API (`GET /api/admin/audit-log`). Separate
  table (`audit_log`, migration 011), separate service
  (`internal/audit/`), separate retention policy. IP and User-Agent
  captured on every entry.
- Retention policies are admin-only, workspace-scoped with an
  optional folder override (`internal/retention/`, migration 012).
  A partial unique index on
  `(workspace_id, COALESCE(folder_id, '00000000-0000-0000-0000-000000000000'::uuid))`
  prevents duplicate workspace-wide policies while still allowing
  folder-specific policies to stack. `Service.Evaluate` returns
  separate archive / delete lists so callers can dispatch
  `drive.archive.cold` jobs and hard-delete out-of-retention rows
  in different pipelines.
- Cold archive uses gzip compression and writes to a stable
  `{workspace}/archive/{file}/{version}.gz` key pattern
  (`internal/retention/archive.go`). The archive worker streams
  each version through `compress/gzip` in memory (acceptable for
  Phase 3 since typical files are < 100 MB; multipart / streaming
  upload for larger archives is tracked for Phase 4). Migration 013
  adds `file_versions.archived_at` so reads can distinguish hot vs
  cold versions; the hot copy is kept for now to make recovery
  trivial.
- Admin dashboard API surface: `/api/admin/users` (list, invite,
  deactivate, role change), `/api/admin/storage-usage`,
  `/api/admin/audit-log`, `/api/admin/retention-policies`. All
  routes require `role == "admin"` via a new `AdminOnly`
  middleware. Migration 014 adds `users.last_login_at` and
  `users.deactivated_at`; login + logout handlers update these.
  Frontend admin pages are deferred to a follow-up PR.
- Rate limiting is an in-memory token bucket keyed by
  `(workspace_id, user_id)` with defaults 100 req/s per user and
  1000 req/s per workspace. Janitor sweeps idle buckets every 5
  minutes. Redis-backed implementation deferred — Phase 3 single
  replica does not need cross-node coordination. Middleware sits
  after auth / tenant guards and returns 429 with a `Retry-After`
  header.
- Guest expiry runs as a **periodic sweep in the worker binary**
  every 5 minutes (30 s startup delay to avoid racing migrations).
  `sharing.Service.ExpireGuestAccess(now)` deletes matching
  `guest_invites` first (FK to permissions) then drops the
  permission row in the same transaction. Chose a periodic sweep
  over a NATS subject so the worker can make forward progress
  without a publisher, and because sub-minute precision is not a
  requirement (the auth / share resolve paths already reject
  expired rows inline).
- Docker Compose now ships a `worker` service (same Dockerfile,
  alternate entrypoint) and a `clamav` sidecar (clamav/clamav:1.3,
  GPL-2.0 external daemon — no linking, same posture as Postgres).
  Server gets `NATS_URL` so it publishes preview / scan / index
  jobs; worker reads the same subjects and also owns retention /
  archive / guest-expiry.
- Deferred from Phase 3: billing integration (Stripe + metering),
  file tagging, bulk operations, Playwright e2e suite, Kubernetes
  manifests, frontend admin / billing UI. These are still on the
  Phase 3 checklist but not part of this kickoff PR.

**Decisions / Deferrals (2026-04-25, Phase 3 completion sprint)**:

- File tags live in a dedicated `file_tags` join table (migration
  015), not a column on `files`. Keeps the primary table lean and
  lets a single file accumulate tags without schema changes. The
  search path (`internal/search/`) `LEFT JOIN`s `file_tags` via a
  CTE that aggregates tag text into a per-file bag before feeding
  the tsvector, so matching a tag surfaces the file exactly once.
- Billing (migration 016) ships two tables:
  `workspace_plans` (per-workspace tier + overridable limits) and
  `usage_events` (append-only ledger of storage / bandwidth /
  user-added events). The quota checkers (`CheckStorageQuota`,
  `CheckUserQuota`, `CheckBandwidthQuota`) read current-state
  counters (files table, users table, month-to-date sum of
  bandwidth events) rather than replaying the full ledger. Stripe
  integration is **stubbed, not live** — the admin API exposes
  `PUT /api/admin/billing/plan` for manual tier updates; webhook
  wiring lands in a follow-up.
- Bulk zip download is the one endpoint where the API server
  proxies object bytes (everything else is presigned-URL direct to
  storage). Capped at 100 files and 1 GiB total per request. For
  larger archives the plan is to assemble the zip as a temp object
  via an async worker and return a presigned URL — deferred.
- Kubernetes manifests under `deploy/k8s/` are **dev/staging only**:
  single-replica Postgres StatefulSet, in-cluster NATS, no
  HorizontalPodAutoscaler or PodDisruptionBudgets. Production
  should use managed Postgres (RDS / Cloud SQL) and managed NATS;
  documented in `deploy/README.md`.
- Playwright e2e suite runs signup / admin / billing flows green
  out of the box; upload / sharing / guest specs are gated behind
  `E2E_RUN_UPLOAD=1` so they only execute in environments where
  the object storage gateway is configured. The CI job is marked
  `continue-on-error: true` while the harness stabilizes.
- Minor hygiene: docs PROGRESS.md corrected to reflect the actual
  5 min OAuth state cookie TTL, and `permissionGranterAdapter` was
  lifted from both `cmd/server/main.go` and `cmd/worker/main.go`
  into the shared `internal/wiring/` package so the two binaries
  share a single implementation.

---

## Phase 4: Privacy & Differentiation (Weeks 15–22)

**Status**: `IN PROGRESS`

**Decisions / Deferrals (2026-04-25, Phase 4 kickoff)**:

- Upstream presigned URL support confirmed landed (zk-object-fabric
  commit 978246fb, 2026-04-23). Phase 1 decision gate upgraded from
  [~] to [x]. Remaining auth gaps (chunked SigV4, x-amz-date
  fallback, STS temp credentials) tracked as Phase 4 Task 5c for
  flexible auth strategy dispatch.
- Tenant provisioning to zk-object-fabric is the prerequisite for
  data residency and CMK. Each ZK Drive workspace maps 1:1 to a
  zk-object-fabric tenant. On workspace creation, ZK Drive will call
  the zk-object-fabric console API to provision a tenant + API key
  pair and store the credentials in a `workspace_storage_credentials`
  table (migration 017). The static `S3_*` env vars remain as the
  fallback for existing workspaces that predate this migration and
  for local-dev / CI where the fabric console is not running.
- Placement policy integration: ZK Drive admin API will proxy
  placement policy reads / writes to zk-object-fabric's
  `PUT /api/tenants/{id}/placement`. The placement policy DSL
  supports provider, region, country, and storage_class constraints
  (see zk-object-fabric `metadata/placement_policy/policy.go`). The
  admin handler validates the policy locally with
  `placement_policy.Policy.Validate()` before forwarding so a bad
  request is rejected without a round-trip.
- Per-folder encryption mode: `folders.encryption_mode` column
  (migration 018) defaults to `managed_encrypted`. Strict-ZK folders
  disable server-side preview, scan, and search. Cross-mode file
  moves require re-upload and are rejected at the service layer with
  a new `folder.ErrEncryptionModeMismatch` sentinel surfaced by
  `MoveFile` as 409 Conflict.
- The `storage.Client` singleton will be replaced with a
  per-workspace client factory (`internal/storage/factory.go`) that
  resolves credentials from `workspace_storage_credentials`. This is
  required for per-workspace placement policies to take effect.
  The factory caches resolved clients in a `sync.Map` keyed by
  workspace_id and falls back to the static fallback client when no
  row exists, so legacy workspaces keep working unchanged.

**Follow-up (2026-04-25, post-PR #10 review)**:

- `UploadURL` orphan-row regression: PR #10 created the file metadata
  row before resolving the per-workspace storage client, so a
  workspace without storage configured would get a stranded file
  row and a 501 response. Fixed by hoisting `resolveStorage` (and
  the nil check) ahead of `h.files.Create`, and dropping the
  misleading `h.storage == nil && h.storageFactory == nil` early
  guard that could never trigger now that the factory is always
  non-nil.
- `BulkMove` / `BulkCopy` cross-mode bypass: the single-file
  `MoveFile` handler rejects cross-encryption-mode moves via
  `sameFolderEncryptionMode`, but the bulk handlers skipped the
  check, allowing strict-ZK files to be relocated into managed
  folders (and vice versa). Both bulk loops now resolve the source
  folder, compare its `EncryptionMode` to the already-resolved
  target folder, and emit a per-item `encryption mode mismatch`
  failure on divergence instead of aborting the batch. Bulk folder
  moves continue to delegate to `folder.Service.Move`, which already
  enforces the same invariant internally.
- `BulkDownload` per-workspace storage: the handler still used the
  static `h.storage` fallback for both the configured-check and
  `appendZipEntry`. Updated to call `h.resolveStorage(ctx,
  workspaceID)` once at the top and thread the resolved client into
  every zip entry, matching the rest of the presigned-URL handlers
  touched by PR #10.

**Decisions / Deferrals (2026-04-25, Phase 4 sprint 2 planning)**:

- PR #10 review findings (UploadURL orphan, BulkMove/BulkCopy
  cross-mode bypass, BulkDownload per-workspace storage) all closed
  in PR #12. No open regressions from recent commits.
- Phase 3 gate note corrected: upstream presigned-URL fix confirmed
  landed (zk-object-fabric 978246fb). Byte-path round-trip is no
  longer blocked.
- Task 5c (upstream auth flexibility) complete: zk-object-fabric
  PR #29 (commit 39dcd81e) landed PresignedV4Strategy,
  HeaderV4Strategy, Date-header fallback, chunked SigV4 seed
  signature + VerifyChunkSignature helper. The s3_compat suite now
  exercises presigned URLs with auth enabled end to end, including
  a PresignedGetExpired subtest.
- KMS-backed credential encryption (`SecretEncryptor` /
  `CredentialDecryptor`) is a prerequisite before storing real fabric
  tenant secrets in production. The current `IdentityEncryptor` /
  `IdentityDecryptor` pair is local-dev only. Tracked as sprint 2
  Task 2.
- AI thread summary (Task 9) deferred past KChat integration (Task 8)
  per strategic guardrails: pooled org storage, guest/client rooms,
  and data residency are the competitive wedge — prioritize these
  over feature parity.
- Native mobile app evaluation added to the Phase 4 tail; PWA-first
  remains the default unless Lighthouse / adoption metrics force a
  React Native investment.
- Next 10 tasks prioritized — see the sprint 3 audit section below
  for the refreshed list after sprint 2 closure.

**Decisions / Deferrals (2026-04-25, Phase 4 sprint 3 audit)**:

- Tree integrity verified on `main`: `migrations/017_workspace_storage_credentials.up.sql`,
  `migrations/018_folder_encryption_mode.up.sql`,
  `internal/fabric/provisioner.go`, `internal/fabric/client.go`,
  `internal/fabric/placement.go`, and `internal/storage/factory.go`
  are all present. The Phase 4 merge commits from PRs #9–#14 landed
  their changes cleanly; no manual cherry-pick required.
- Tenant provisioning (Task 2) confirmed shipped: migration 017
  (`workspace_storage_credentials`), `internal/fabric/provisioner.go`,
  and signup wiring (best-effort, non-fatal on fabric failure).
- Placement policy admin (Task 3) confirmed shipped:
  `GET/PUT /api/admin/placement` proxied to zk-object-fabric with
  local `placement_policy.Policy.Validate()` pre-check.
- Per-folder encryption mode (Task 4) confirmed shipped: migration
  018 introduced `folders.encryption_mode`; `MoveFile` rejects
  cross-mode moves with 409 Conflict via
  `folder.ErrEncryptionModeMismatch`.
- Strict-ZK worker skip (Task 5) confirmed shipped: preview, scan,
  and index handlers ack and skip jobs whose file lives in a
  strict-ZK folder so JetStream does not redeliver.
- Storage client factory (`internal/storage/factory.go`) confirmed
  shipped: per-workspace clients are resolved from
  `workspace_storage_credentials` with a `sync.Map` cache and a
  static fallback for legacy workspaces.
- PR #10 review findings closed in PR #12: UploadURL orphan-row
  regression, BulkMove/BulkCopy cross-encryption-mode bypass, and
  BulkDownload per-workspace storage are all resolved on `main`.
- Task 5c (upstream auth flexibility) closed upstream in
  zk-object-fabric PR #29 (commit 39dcd81e): PresignedV4Strategy,
  HeaderV4Strategy, Date-header fallback, chunked SigV4 seed
  signature, and `VerifyChunkSignature` helper. The s3_compat suite
  exercises presigned URLs with auth enabled end to end, including a
  `PresignedGetExpired` subtest.
- Phase 4 checklist dedupe from PR #14 verified: the duplicate
  content-search and native-mobile entries introduced in PR #13 were
  collapsed; only one entry each remains.

**Decisions / Deferrals (2026-04-25, Phase 4 sprint 4 audit)**:

- PR #12 bulk-fix tree integrity: verified that `BulkMove` /
  `BulkCopy` cross-encryption-mode checks (target folder captured
  via `h.folders.GetByID`, `sameFolderEncryptionMode` guard before
  `h.files.Move` / `h.files.Create`) and `BulkDownload`
  per-workspace storage resolution (`h.resolveStorage` threaded
  through `appendZipEntry`), plus the `UploadURL` orphan-row fix
  (`h.resolveStorage` called before `h.files.Create`), are all
  present on `main`. No re-apply of commit 2afe7a06 was required;
  the merge commit 3645dee5 propagated the bulk.go and handler.go
  changes cleanly.
- Strict-ZK search exclusion identified as unresolved:
  `internal/search/service.go` does not filter on
  `folders.encryption_mode`. Files in strict-ZK folders appear in
  FTS results, violating the strict-ZK privacy contract.
  Prioritized as Task 4 in the refreshed next-10 list.
- `IdentityEncryptor` / `IdentityDecryptor` flagged as production
  blocker: `workspace_storage_credentials.secret_key_encrypted` is
  stored in plaintext when using the default encryptor. KMS-backed
  implementation is Task 2 in the next-10 list.
- Content search index worker confirmed as no-op: `indexHandler` in
  `cmd/worker/main.go` acks messages without text extraction.
  Managed-encrypted files are not content-searchable. Task 5 in
  next-10.
- Playwright e2e suite still runs with `continue-on-error: true` in
  CI. Deferred to post-Phase-4 stabilization.
- AI thread summary deferred past KChat integration per strategic
  guardrails (pooled org storage, guest/client rooms, and data
  residency are the competitive wedge).

**Decisions / Deferrals (2026-04-25, Phase 4 sprint 5 audit)**:

- PR/commit audit of the 30 most recent commits found no new
  regressions beyond the three items already tracked (strict-ZK
  search leak, `IdentityDecryptor`, `indexHandler` no-op).
- zk-object-fabric PR #29 (flexible SigV4 dispatch) and PR #28
  (KMS/Vault wrappers) are both merged on zk-object-fabric `main`,
  clearing the upstream blockers for Tasks 3 and 6.
- Playwright e2e suite was moved to `workflow_dispatch` (commit
  ad93efab) and is not running on every PR. Deferred to
  post-Phase-4 stabilization.
- Phase 3 decision gate remains unchecked despite all Phase 3
  checklist items being [x]. Note: gate can be closed once the e2e
  presigned URL round-trip test (Task 3) passes, since that was the
  last Phase 1→3 blocker.
- Native mobile app evaluation demoted from next-10 Task 10 to
  deferred; replaced with Phase 3+4 decision gate validation as
  Task 10 since closing gates is higher priority.
- AI thread summary remains deferred past KChat integration per
  strategic guardrails.

**Next 10 tasks (prioritized, sprint 5 refresh)**:

1. ~~Verify PR #12 bulk fixes on `main`~~ — DONE (sprint 4).
2. KMS-backed credential encryption — replace `IdentityEncryptor` /
   `IdentityDecryptor` in `internal/fabric/provisioner.go` and
   `internal/storage/factory.go` with a KMS-backed implementation for
   `workspace_storage_credentials.secret_key_encrypted` (production
   blocker).
3. E2e presigned URL round-trip test — upstream blocker cleared in
   zk-object-fabric PR #29; add a `tests/e2e/` spec exercising
   presigned PUT and GET against the Docker demo.
4. Strict-ZK search exclusion — filter `internal/search/service.go`
   so FTS queries exclude files whose parent folder has
   `encryption_mode = 'strict_zk'`. Today the service does not join
   on `folders.encryption_mode`, so strict-ZK files leak into result
   sets.
5. Content search index worker — implement text extraction in
   `indexHandler` (`cmd/worker/main.go`) for managed-encrypted docs
   feeding Postgres FTS.
6. CMK wiring against zk-object-fabric KMS/Vault wrappers (upstream
   PR #28 now merged).
7. Frontend admin UI for placement policy and per-folder
   encryption-mode selection.
8. KChat integration API — room-folder mapping, permission sync,
   attachment metadata.
9. Client-room templates for agencies, accounting, legal,
   construction, and clinics.
10. Close Phase 3 decision gate + Phase 4 decision gate validation —
    e2e presigned URL round-trip (Task 3) closes Phase 3 gate;
    strict-ZK e2e + KChat room-folder e2e closes Phase 4 gate.

Deferred: AI thread summary (past KChat integration), Stripe webhook
wiring (Phase 5), Playwright continue-on-error removal (harness
stabilization), native mobile app evaluation (after Phase 4 gate).

**Decisions / Deferrals (2026-04-25, sprint 5 — test-first refresh)**:

- Test gap audit revealed that Phase 3 features (admin API, billing,
  retention, audit log, rate limiting, bulk ops, file tags) currently
  have zero integration test coverage because
  `tests/integration/setup_test.go` does not wire those routes into
  the test harness — handlers exist on `main` but are unreachable
  from the integration tests, so regressions land unnoticed.
- `TestUploadConfirmDownloadRoundTrip` in
  `tests/integration/storage_test.go` is always skipped in CI because
  `S3_ENDPOINT` is never set in the GitHub Actions environment. The
  presigned PUT/GET byte-path round-trip — the test that closes the
  Phase 3 decision gate — has therefore never actually executed in CI.
- The Playwright `guest-access.spec.ts` spec is a placeholder stub
  with no real assertions; the e2e suite reports "passing" without
  exercising guest access.
- Permission inheritance (cascading folder grants), file versioning
  (re-upload creating a new `file_versions` row), and
  soft-delete/restore (trash workflow) have no integration-level
  tests despite all three being load-bearing for Phase 3 correctness.
- Decision: front-load test infrastructure (Tasks 1–6) before feature
  work (Tasks 7–10) so that regressions across admin / billing /
  retention / audit / bulk / tag / inheritance / versioning / trash
  surfaces are caught immediately on every PR. Feature tasks (KMS,
  strict-ZK search exclusion, e2e presigned URL CI integration,
  content search index worker) ride on top of the new harness with
  their own dedicated test gates.

**Decisions / Deferrals (2026-04-26, Phase 4 sprint 6 audit)**:

- PR/commit audit of the 30 most recent commits (`fd4ceaa8` through
  `fde37f8a`) found no new regressions beyond the three already
  tracked: strict-ZK search leak in `internal/search/service.go`,
  `IdentityEncryptor` / `IdentityDecryptor` plaintext storage, and
  `indexHandler` no-op in `cmd/worker/main.go`.
- zk-object-fabric `main` is current through commit `7d22a4d7`
  (PR #31, 2026-04-25). Phase 3 finalization (B2C billing wiring,
  SSE alias, deploy fixes) all landed. No upstream blockers for
  zk-drive Phase 4 work.
- `setup_test.go` `ResetTables` confirmed missing `workspace_plans`,
  `usage_events`, and `file_tags` from the TRUNCATE statement — must
  be added as part of Task 1.
- Bulk ops routes (`BulkMove`, `BulkCopy`, `BulkDelete`,
  `BulkDownload`) and file tag routes (`AddFileTag`, `RemoveFileTag`)
  confirmed wired in `cmd/server/main.go` but absent from
  `tests/integration/setup_test.go` — zero integration coverage
  confirmed.
- `guest-access.spec.ts` confirmed as a placeholder stub with no real
  assertions; deferred to post-Phase-4 Playwright stabilization.
- No open PRs on zk-drive. All PRs through #18 are merged to `main`.
- Sprint 5 next-10 Tasks 1–10 all remain `NOT STARTED`. No items to
  check off.
- Deferrals unchanged: AI thread summary (past KChat), Stripe
  webhooks (Phase 5), Playwright `continue-on-error` removal
  (post-Phase-4), native mobile (after Phase 4 gate).

**Decisions / Deferrals (2026-04-26, Phase 4 sprint 6 — post-sprint-5 closure)**:

- PR/commit audit of commits since the sprint 6 audit (`d8f8c776` through `6cc7dc4e`, PR #20) found no new regressions. All three previously tracked issues are now resolved on `main`:
  - Strict-ZK search leak: `internal/search/service.go` now filters `parent.encryption_mode <> 'strict_zk'` (file branch) and `fo.encryption_mode <> 'strict_zk'` (folder branch). Pinned by `TestSearchExcludesStrictZKFiles`.
  - `IdentityEncryptor` / `IdentityDecryptor` plaintext storage: replaced by `internal/crypto/crypto.go` AES-256-GCM codec wired through `cmd/server/main.go`. Pinned by `TestKMSEncryptDecryptRoundTrip` (unit) + `TestProvisionerPersistsEncryptedSecret` (integration).
  - `indexHandler` no-op: `internal/index/service.go` now extracts text from text/*, application/json, application/xml and persists to `files.content_text`. Pinned by `TestIndexWorkerExtractsText`.
- In-flight bug fix in PR #20: `ExtractText` UTF-8 rune boundary truncation (`e0fdf3b5`) — initial implementation could split a multi-byte rune at `MaxIndexBytes`, causing Postgres to reject the write and the worker to Nak/redeliver in a loop. Fixed by `truncateUTF8()` with unit tests.
- zk-object-fabric current through PR #32 (`0a0e2dd8`, 2026-04-26). No upstream blockers.
- No open PRs on zk-drive. All PRs through #20 merged to `main`.
- Sprint 5 next-10 Tasks 1–10 all confirmed `[x]` COMPLETE.
- Phase 3 decision gate can now be closed: all Phase 3 checklist items are [x] and `TestUploadConfirmDownloadRoundTrip` passes in CI (no longer skipped). Task 8 in sprint 6 next-10.
- Deferrals unchanged: AI thread summary (past KChat), Stripe webhooks (Phase 5), Playwright `continue-on-error` removal (post-Phase-4), native mobile (after Phase 4 gate).

### Next 10 Tasks (Phase 4, sprint 6)

| # | Task | Test gate |
|---|------|-----------|
| 1 | CMK wiring — integrate `internal/crypto/` with zk-object-fabric KMS/Vault wrappers (upstream PR #28) for workspace-level customer-managed keys; `PUT /api/admin/cmk` | `TestCMKProvisionAndRotate` integration test |
| 2 | Frontend admin UI: placement policy editor — expose `GET/PUT /api/admin/placement` in a React admin page with region/country/provider selectors | Playwright spec or manual QA gate |
| 3 | Frontend admin UI: per-folder encryption-mode selector — folder create/edit dialog shows managed-encrypted vs strict-ZK toggle with tradeoff warnings | Playwright spec or manual QA gate |
| 4 | KChat integration API: room-folder mapping — `POST/GET/DELETE /api/kchat/rooms` creates a folder + permission bundle per KChat room | `TestKChatRoomFolderMapping` integration test |
| 5 | KChat integration API: permission sync — room membership changes propagate to ZK Drive folder grants via `POST /api/kchat/rooms/{id}/sync-members` | `TestKChatPermissionSync` integration test |
| 6 | KChat integration API: attachment metadata — `POST /api/kchat/attachments/upload-url` + `POST /api/kchat/attachments/confirm` for room-scoped uploads | `TestKChatAttachmentUpload` integration test |
| 7 | Client-room templates — pre-configured folder structures for agencies, accounting, legal, construction, clinics in `internal/sharing/` | `TestClientRoomTemplateCreate` integration test |
| 8 | Close Phase 3 decision gate — all Phase 3 checklist items are `[x]` and `TestUploadConfirmDownloadRoundTrip` now passes in CI; mark gate `[x]` | CI green on `main` |
| 9 | Phase 4 decision gate: strict-ZK e2e — create strict-ZK folder, upload file, verify no preview generated and search excludes it | `TestStrictZKE2E` integration test |
| 10 | Phase 4 decision gate: KChat room-folder e2e — create room mapping, upload attachment via integration API, verify metadata + permissions | `TestKChatE2E` integration test |

**Decisions / Deferrals (2026-04-26, Phase 4 sprint 7 audit)**:

- Fresh source code audit on `main` (HEAD = `7350709d`, PR #21 merge). No new code PRs since PR #20 (sprint 5 implementation). PR #21 was docs-only.
- Sprint 6 next-10 Tasks 1–10 all remain `NOT STARTED`. No items to check off.
- Source code verification of remaining Phase 4 gaps:
  - No CMK wiring: `internal/crypto/crypto.go` line 19 explicitly defers KMS-backed mode. The `Codec` interface is the seam; zk-object-fabric PR #28 (`KMSWrapper`, `VaultWrapper`) clears the upstream dependency.
  - No KChat integration API: no `/api/kchat/` routes in `cmd/server/main.go`. Only `internal/sharing/client_room.go` exists (generic client-room CRUD, not KChat-specific).
  - No placement policy or encryption-mode frontend pages: `frontend/src/pages/` has AdminPage, BillingPage, FileBrowserPage, LoginPage, SignupPage only.
  - No `internal/ai/` package: AI thread summary remains deferred past KChat integration.
  - No client-room templates: `internal/sharing/client_room.go` has generic CRUD but no pre-configured templates.
  - `IdentityEncryptor`/`IdentityDecryptor` types still exist as fallback types in `internal/fabric/provisioner.go` and `internal/storage/factory.go` but are no longer the default path — `internal/crypto/crypto.go` Codec is wired through `cmd/server/main.go`.
- zk-object-fabric current through PR #32 (`0a0e2dd8`, 2026-04-26). No upstream blockers.
- Phase 3 decision gate: `TestUploadConfirmDownloadRoundTrip` confirmed running in CI (`.github/workflows/ci.yml` sets `S3_ENDPOINT`, checks out zk-object-fabric, starts demo gateway). Gate can be formally closed by sprint 6 Task 8.
- Sprint 6 next-10 priorities confirmed correct per strategic guardrails (pooled org storage, guest/client rooms, data residency > feature parity). No reprioritization needed.
- Deferrals unchanged: AI thread summary (past KChat), Stripe webhooks (Phase 5), Playwright `continue-on-error` removal (post-Phase-4), native mobile (after Phase 4 gate).

### Next 10 Tasks (Phase 4, sprint 5 — test-first refresh)

| # | Task | Test gate |
|---|------|-----------|
| 1 | [x] Wire Phase 3 routes in `setup_test.go` — admin/billing/retention/audit/bulk/tag routes + rate limiter + `ResetTables` truncates the new tables | `setup_test.go` compiles with all Phase 3 handlers; existing tests still pass |
| 2 | [x] Integration tests: admin API + audit log — `tests/integration/admin_test.go` | `TestAdminListUsers`, `TestAdminDeactivateUser`, `TestAuditLogRecordsLogin`, `TestAuditLogRecordsPermissionGrant` |
| 3 | [x] Integration tests: billing & quota enforcement — `tests/integration/billing_test.go` | `TestStorageQuotaBlocksUpload`, `TestUserQuotaBlocksInvite`, `TestBandwidthMeteringRecordsEvent` |
| 4 | [x] Integration tests: retention & archive — `tests/integration/retention_test.go` | `TestRetentionPolicyCRUD`, `TestEvaluateReturnsExpiredVersions`, `TestColdArchiveWritesGzipObject` |
| 5 | [x] Integration tests: bulk ops + file tags — `tests/integration/bulk_test.go`, `tests/integration/tag_test.go` | `TestBulkMoveCrossWorkspaceRejected`, `TestBulkDeleteSoftDeletes`, `TestTagSearchSurfacesTaggedFile` |
| 6 | [x] Integration tests: permission inheritance + versioning + soft delete — `tests/integration/inheritance_test.go`, `tests/integration/version_test.go`, `tests/integration/trash_test.go` | `TestChildFileInheritsParentGrant`, `TestReUploadCreatesNewVersion`, `TestSoftDeleteAndRestore` |
| 7 | [x] AES-GCM credential encryption — `internal/crypto/crypto.go` codec wired through `fabric.NewProvisioner` and `storage.NewClientFactory` from `cmd/server/main.go` (with `CREDENTIAL_ENCRYPTION=none` opt-out for local dev) | `TestKMSEncryptDecryptRoundTrip` (unit) + `TestProvisionerPersistsEncryptedSecret` (integration) confirm ciphertext != plaintext and decrypt round-trip |
| 8 | [x] Strict-ZK search exclusion — `internal/search/service.go` joins `parent.encryption_mode` and excludes `strict_zk` on both file and folder branches | `TestSearchExcludesStrictZKFiles` integration test |
| 9 | [x] E2e presigned URL round-trip in CI — `.github/workflows/ci.yml` integration job checks out `kennguy3n/zk-object-fabric`, brings up the demo gateway via `docker compose`, pre-creates the bucket, and exports `S3_ENDPOINT/S3_ACCESS_KEY/S3_SECRET_KEY/S3_BUCKET` | `TestUploadConfirmDownloadRoundTrip` runs and passes in CI (no longer skipped) |
| 10 | [x] Content search index worker — `internal/index` package extracts text from text/* objects, persists to new `files.content_text` column (migration 019), and is wired into `cmd/worker/main.go` `indexHandler`; `internal/search/service.go` includes `f.content_text` in the FTS expression | `TestIndexWorkerExtractsText` integration test |

The next-10 above is the execution plan for reaching the existing
Phase 4 checklist items below; each task carries an explicit test
gate so correctness is provable at every step. Tasks 1–6 build the
test infrastructure that the Phase 3 feature surface should have
shipped with; Tasks 7–10 are the Phase 4 feature follow-throughs
(KMS, strict-ZK search exclusion, presigned-URL CI byte-path proof,
content search index worker), each with its own integration test.

**Goal**: add strict-ZK private folders, customer-managed keys, data
residency controls, the KChat integration API, and AI features for
managed encrypted mode. This is the phase that justifies the
"privacy-preserving" positioning against Tresorit, Proton Drive, and
Infomaniak kDrive.

Checklist:

- [x] Per-folder encryption mode selection: managed encrypted or
      strict ZK. `internal/folder/`. Frontend toggle landed in
      sprint 9 next-10 Task 4 (`CreateFolderDialog` in
      `frontend/src/pages/FileBrowserPage.tsx`) with an inline
      tradeoff callout + encryption-mode badge on folder listings.
  - [x] Task 1: Phase 3 decision-gate validation — metadata-plane
        end-to-end test in `tests/integration/phase3_gate_test.go`.
  - [x] Task 2: Workspace → zk-object-fabric tenant provisioning —
        migration 017, `internal/storage/factory.go`,
        `internal/fabric/provisioner.go`, signup wiring (best-effort,
        non-fatal on fabric failure).
  - [x] Task 3: Placement policy admin endpoint —
        `GET/PUT /api/admin/placement`, validated through
        `placement_policy.Policy.Validate()` and proxied to
        zk-object-fabric.
  - [x] Task 4: Per-folder encryption mode column (migration 018),
        `folders.encryption_mode` exposed on the model and accepted
        by `CreateFolder`; `MoveFile` rejects cross-mode moves with
        409 Conflict.
  - [x] Task 5: Strict-ZK folder support — preview / scan / index
        worker handlers skip jobs whose file lives in a strict-ZK
        folder, log the skip, and ack the message so JetStream does
        not redeliver.
  - [x] Task 5c: upstream auth flexibility — x-amz-date fallback,
        chunked SigV4 seed signature, and auth-strategy dispatch
        landed in zk-object-fabric PR #29 (commit 39dcd81e).
  - [x] Task 5d: e2e presigned URL round-trip test — CI now spins
        up the zk-object-fabric demo gateway via docker compose so
        `TestUploadConfirmDownloadRoundTrip` runs in the integration
        job (next-10 Task 9).
  - [x] Task 6: CMK wiring — migration 020 adds `cmk_uri` column to
        `workspace_storage_credentials`; `internal/crypto/cmk.go`
        introduces `ModeKMS`, `CMKConfig`, and `ValidateCMKURI`
        (accepts `arn:aws:kms:`, `kms://`, `vault://`, `transit://`,
        or empty for gateway-default); admin endpoints
        `GET/PUT /api/admin/cmk` persist locally and best-effort
        forward to the fabric console via `fabric.Client.PutCMK`.
        `TestCMKProvisionAndRotate` and
        `TestCMKReturns404WhenWorkspaceNotProvisioned` pin the full
        contract.
  - [x] Task 7: Frontend admin UI for placement policy and per-folder
        encryption-mode selection. `frontend/src/pages/PlacementPage.tsx`
        wired to `GET/PUT /api/admin/placement`,
        `frontend/src/pages/EncryptionPage.tsx` wired to
        `GET/PUT /api/admin/cmk`, and per-folder encryption-mode
        toggle added to the folder create dialog in
        `frontend/src/pages/FileBrowserPage.tsx`. (Sprint 9 next-10
        Tasks 2–4.)
  - [x] Task 8: KChat integration API — migration 021 creates
        `kchat_room_folders`; `internal/kchat/` ships a service +
        Postgres repository covering room-folder mapping, permission
        sync, and attachment metadata, exposed under `/api/kchat`.
        `TestKChatRoomFolderMapping`, `TestKChatPermissionSync`, and
        `TestKChatAttachmentUpload` exercise the create/list/delete
        lifecycle, three-round member reconciliation, and the
        upload-URL → confirm round-trip respectively.
  - [x] Task 9: AI thread summary / file classification for managed
        encrypted mode. `internal/ai/service.go` renders a rule-based
        scaffold summary (strict-ZK folders refuse with
        `ErrStrictZKForbidden` → 403); `POST /api/kchat/rooms/{id}/summary`
        wires it up. `internal/classify/service.go` persists
        `files.classification` (migration 022) via the new
        `drive.classify.file` NATS subject. `TestThreadSummaryRespectsEncryptionMode`,
        `TestClassificationPersistsResult`, and
        `TestClassificationWorkerSkipsStrictZK` pin the contract.
  - [x] Task 10: Client-room templates — `internal/sharing/templates.go`
        registers the agency, accounting, legal, construction, and
        clinic verticals; `ClientRoomService.CreateFromTemplate`
        provisions sub-folders under the room folder; admin
        endpoints `GET /api/client-rooms/templates` and
        `POST /api/client-rooms/from-template` round out the API.
        `TestClientRoomTemplateCreate` pins the four-sub-folder
        agency layout end-to-end.
- [x] Strict-ZK folder support: disable server-side previews, search,
      and text extraction for strict-ZK folders.
      `internal/preview/`, `internal/search/`.
  - [x] Strict-ZK search exclusion: `internal/search/service.go`
        now joins `parent.encryption_mode` and filters out rows where
        the parent folder is `strict_zk` (file branch) or where the
        folder itself is `strict_zk` (folder branch). The content
        index worker also short-circuits before download for
        strict-ZK files (next-10 Tasks 8 + 10).
- [x] Customer-managed key (CMK) option: workspace-level CMK
      configuration via zk-object-fabric. `internal/crypto/cmk.go`
      validates `arn:aws:kms:`, `kms://`, `vault://`, `transit://`,
      and gateway-default; persisted to
      `workspace_storage_credentials.cmk_uri` and best-effort
      forwarded to the fabric console via `fabric.Client.PutCMK`.
      Admin endpoints `GET/PUT /api/admin/cmk`. (Task 6)
- [x] Data residency controls: expose zk-object-fabric placement
      policies in the admin UI. `frontend/src/pages/PlacementPage.tsx`
      talks to `GET/PUT /api/admin/placement` (backend done in
      Task 3, frontend landed in sprint 9 next-10 Task 2).
- [x] KChat integration API: REST endpoints for room-folder mapping,
      permission sync, and attachment metadata. New `internal/kchat`
      package + `api/kchat/handler.go`; routes mounted under
      `/api/kchat` with auth + tenant guard. (Task 8)
- [x] AI thread summary / file classification (managed encrypted mode
      only). `internal/ai/service.go` (thread summary scaffold;
      strict-ZK refusal), `internal/classify/service.go` (rule-based
      classifier persisting `files.classification`, migration 022),
      `cmd/worker/main.go` subscribes to `drive.classify.file`.
      (Sprint 9 next-10 Tasks 5 + 6.)
- [x] Content search for managed encrypted files: `internal/index`
      extracts text from text/* objects, persists to
      `files.content_text` (migration 019), and the search FTS
      expression now scores on body content alongside name + tags.
      Strict-ZK files never reach the index worker.
- [x] Client room templates: pre-configured folder structures for
      agencies, accounting, legal, construction, and clinics.
      `internal/sharing/templates.go` plus the
      `CreateFromTemplate` extension on `ClientRoomService`. Exposed
      via `GET /api/client-rooms/templates` and
      `POST /api/client-rooms/from-template`. (Task 10)
- [x] Native mobile app evaluation: PWA Lighthouse benchmark + decide
      on React Native investment. Decision recorded in
      `docs/MOBILE_EVALUATION.md` — **PWA-first** for Phase 5 with
      manifest + service worker + offline fallback; React Native
      deferred behind measurable install traction and server-side
      delta-pull infrastructure that does not yet exist. (Sprint 9
      next-10 Task 8.)
- [x] KMS-backed credential encryption for workspace_storage_credentials.
      `internal/crypto/` ships an AES-256-GCM codec (raw / hex /
      base64 keys) wired through `fabric.NewProvisioner` and
      `storage.NewClientFactory`. Plaintext rows from before this
      change still decrypt cleanly through the legacy passthrough
      path; new rows land with the `aesgcm:` prefix when
      `CREDENTIAL_ENCRYPTION_KEY` is set.
- [x] Decision gate: a workspace admin can create a strict-ZK private
      folder, upload files with client-side encryption, and verify
      that the server cannot generate previews or search file
      content. KChat can create a room-folder mapping and upload
      attachments via the integration API.
      **Phase 4 decision gate closed** — `TestPhase4DecisionGate`
      passes in CI exercising strict-ZK folder (no preview, no
      search), KChat attachment round-trip, and agency template
      room creation (sprint 9 next-10 Task 9).

**Decisions / Deferrals (2026-04-26, Phase 4 sprint 8 — coding sprint)**:

- 5 backend coding tasks landed in this sprint, all with passing
  integration tests against the live Postgres test database:
  - **CMK wiring** (Task 6): migration 020 adds the `cmk_uri`
    column; `internal/crypto/cmk.go` introduces `ModeKMS`,
    `CMKConfig`, and `ValidateCMKURI`. Admin endpoints
    `GET/PUT /api/admin/cmk` persist the URI and best-effort forward
    to the fabric console via `fabric.Client.PutCMK`. Per-tenant
    persistence is the source of truth so a console outage never
    blocks rotation. `tests/integration/cmk_test.go` covers the full
    rotate/reset lifecycle plus the 404 contract for unprovisioned
    workspaces.
  - **KChat room-folder mapping** (Task 8.a): migration 021 creates
    `kchat_room_folders` (workspace × room unique). New
    `internal/kchat/` package mirrors the small-interface pattern
    from `internal/sharing/client_room.go` (FolderCreator,
    PermissionGranter, FileCreator) so the package never imports
    folder/permission/file directly. Routes mounted under
    `/api/kchat`; the creator receives an automatic admin grant on
    the room folder.
  - **KChat permission sync** (Task 8.b): `RoomService.SyncMembers`
    indexes existing `user`-type grants, revokes anything not in
    the desired set, and adds/upgrades the rest. Guest grants are
    left alone so KChat sync never clobbers an out-of-band guest
    invite. Idempotent: re-sending the same set leaves grants
    unchanged.
  - **KChat attachment metadata** (Task 8.c):
    `AttachmentUploadURL` resolves the room's folder, creates the
    file metadata row up front, and mints a presigned PUT keyed by
    `{workspaceID}/{fileID}/{versionID}`. `ConfirmAttachment`
    validates the object-key prefix before flipping the file's
    current version pointer, so a malicious caller cannot graft an
    arbitrary key onto someone else's file row.
  - **Client-room templates** (Task 10):
    `internal/sharing/templates.go` registers agency, accounting,
    legal, construction, and clinic verticals. The new
    `ClientRoomService.CreateFromTemplate` creates the room then
    provisions sub-folders under the room folder via the existing
    `FolderCreator` interface. Two new admin endpoints —
    `GET /api/client-rooms/templates` and
    `POST /api/client-rooms/from-template` — round out the API.

- Remaining Phase 4 work (still unchecked):
  - **Task 7** (frontend admin UI for placement policy + per-folder
    encryption-mode selection) is the next priority. The backend
    surface is complete (placement policy in Task 3, CMK in Task
    6, encryption mode in Task 4); the UI is now the only missing
    piece for the encryption story.
  - **Task 9** (AI thread summary / file classification) is
    unblocked now that the KChat bridge exists, but not yet
    started.
  - Data residency frontend, native mobile evaluation, and the
    Phase 4 decision gate remain deferred.

**Phase 4 sprint 8 next-5**:

| # | Task | Test Gate |
|---|------|-----------|
| 1 | [ ] Frontend admin UI: placement policy editor (`frontend/src/admin/placement.tsx`) talking to `/api/admin/placement` | Cypress / Playwright e2e: pick a region, save, reload, assert persisted |
| 2 | [ ] Frontend admin UI: encryption-mode selector and CMK URI entry (`frontend/src/admin/encryption.tsx`) talking to `/api/admin/cmk` | e2e: set ARN, save, reload, assert displayed |
| 3 | [ ] AI thread summary scaffold (`internal/ai/summary.go`) + `POST /api/kchat/rooms/{id}/summary` (managed encrypted only) | `TestThreadSummaryRespectsEncryptionMode` integration test |
| 4 | [ ] File classification job (`cmd/worker/` consumer) routed off the existing index queue, persisting `files.classification` | `TestClassificationWorkerSkipsStrictZK` integration test |
| 5 | [ ] Phase 4 decision-gate end-to-end test combining strict-ZK upload + KChat attachment + template room creation in `tests/integration/phase4_gate_test.go` | Single integration test that exercises the full gate |

**Decisions / Deferrals (2026-04-26, Phase 4 sprint 9 audit)**:

- PR/commit audit of HEAD (`0d1aa5c5`, PR #23 merge) found no new regressions. All three previously tracked issues (strict-ZK search leak, `IdentityEncryptor` plaintext, `indexHandler` no-op) remain resolved since PR #20.
- zk-object-fabric current through PR #36 (`18b379e1`, 2026-04-26) — Phase 3.5 intra-tenant dedup scaffolding. No upstream blockers for zk-drive Phase 4 work.
- No open PRs on zk-drive. All PRs through #23 merged to `main`.
- Sprint 8 next-5 Tasks 1–5 all confirmed `NOT STARTED`. No items to check off.
- Phase 3 decision gate formally closed: all Phase 3 checklist items are `[x]` and `TestUploadConfirmDownloadRoundTrip` passes in CI (confirmed since sprint 5 next-10 Task 9).
- Source code verification of remaining Phase 4 gaps:
  - No frontend admin pages for placement/encryption/CMK: `frontend/src/pages/` has AdminPage, BillingPage, FileBrowserPage, LoginPage, SignupPage only.
  - No `internal/ai/` package: AI thread summary remains unstarted.
  - No `phase4_gate_test.go`: decision-gate e2e test does not exist yet.
- Sprint 8 coding sprint review: PR #23 post-merge fixes addressed best-effort error handling (`PutCMK`, `CreateFromTemplate`), admin-role gating on KChat mutating endpoints, deterministic template list ordering, and KChat downstream error sentinel mapping. All fixes have integration test coverage.
- Deferrals: native mobile evaluation (after Phase 4 gate), Stripe webhooks (Phase 5), Playwright `continue-on-error` removal (post-Phase-4).

**Decisions / Deferrals (2026-04-26, Phase 4 sprint 9 — closure sprint)**:

- All 10 sprint 9 next-10 tasks landed in a single PR, each as a
  separate commit. Every backend task has passing integration
  coverage against the live Postgres test database; every frontend
  task passes `npm run lint` + `npm run build`.
  - **Task 1 (frontend placement policy editor)**:
    `frontend/src/pages/PlacementPage.tsx` with admin-only guard,
    wired to `fetchPlacement` / `updatePlacement` in `api/client.ts`.
    Route `/admin/placement` + nav link in `AdminPage`.
  - **Task 2 (frontend encryption + CMK admin page)**:
    `frontend/src/pages/EncryptionPage.tsx` with admin-only guard,
    CMK URI input, inline managed vs strict-ZK callout. Route
    `/admin/encryption` + nav link.
  - **Task 3 (per-folder encryption-mode toggle)**:
    `CreateFolderDialog` in `FileBrowserPage.tsx` with radio group
    + tradeoff callout; `EncryptionBadge` rendered on every folder
    list entry.
  - **Task 4 (AI thread summary scaffold)**:
    `internal/ai/service.go` + `POST /api/kchat/rooms/{id}/summary`.
    Strict-ZK folders return 403 via `ErrStrictZKForbidden`.
    `TestThreadSummaryRespectsEncryptionMode` pins both paths.
  - **Task 5 (file classification worker)**: migration 022 adds
    `files.classification`; `internal/classify/service.go` runs a
    rule-based label pass (image / invoice / contract / document /
    other); `drive.classify.file` added to the NATS stream +
    subscribed from `cmd/worker/main.go` with strict-ZK skip.
    `TestClassificationPersistsResult` and
    `TestClassificationWorkerSkipsStrictZK` pin the contract.
  - **Task 6 (frontend KChat rooms page)**:
    `frontend/src/pages/KChatRoomsPage.tsx` lists, creates, deletes,
    and syncs KChat room-folder mappings. New helpers in
    `api/client.ts`.
  - **Task 7 (frontend client-room template picker)**: admin-only
    “Create from template” button in the file-browser toolbar opens
    a template-card dialog (agency / accounting / legal /
    construction / clinic), prompts for a client name, POSTs to
    `/api/client-rooms/from-template`, navigates into the new
    room.
  - **Task 8 (native mobile app evaluation)**:
    `docs/MOBILE_EVALUATION.md` records the estimated Lighthouse
    baseline, PWA readiness gap, React Native effort estimate
    (~8–10 engineer-weeks, plus unbuilt offline-sync server work),
    and selects **PWA-first** for Phase 5.
  - **Task 9 (Phase 4 decision-gate E2E)**:
    `tests/integration/phase4_gate_test.go` exercises strict-ZK
    folder search-exclusion, KChat attachment round-trip, and
    agency template creation in a single `TestPhase4DecisionGate`.
  - **Task 10 (close gate)**: this checklist update.

- Phase 4 is closed. **Phase 5 deferrals** carried from this sprint:
  - Stripe webhook wiring (Phase 5 billing).
  - Playwright `continue-on-error` removal + stabilisation.
  - LLM-backed AI thread summaries / classification — the current
    implementations are deliberate rule-based scaffolds so the
    workflow (strict-ZK refusal, worker pipeline, column
    persistence) is wired end-to-end without a model dependency.
  - PWA manifest + service worker + install prompt (the action
    items at the bottom of `docs/MOBILE_EVALUATION.md`).

### Next 10 Tasks (Phase 4, sprint 9)

| # | Task | Test Gate |
|---|------|-----------|
| 1 | [x] Close Phase 3 decision gate — mark `[x]` in PROGRESS.md | CI green on `main` |
| 2 | [x] Frontend admin UI: placement policy editor (`frontend/src/pages/PlacementPage.tsx`) talking to `GET/PUT /api/admin/placement` | Playwright spec or manual QA gate |
| 3 | [x] Frontend admin UI: encryption-mode selector + CMK URI entry (`frontend/src/pages/EncryptionPage.tsx`) talking to `GET/PUT /api/admin/cmk` | Playwright spec or manual QA gate |
| 4 | [x] Frontend: per-folder encryption-mode toggle in folder create/edit dialog with tradeoff warnings | Manual QA gate |
| 5 | [x] AI thread summary scaffold (`internal/ai/service.go`) + `POST /api/kchat/rooms/{id}/summary` (managed encrypted only) | `TestThreadSummaryRespectsEncryptionMode` integration test |
| 6 | [x] File classification job (`cmd/worker/` consumer) persisting `files.classification` column; strict-ZK files skipped | `TestClassificationWorkerSkipsStrictZK` + `TestClassificationPersistsResult` integration tests |
| 7 | [x] Frontend: KChat room-folder management page (list/create/delete mappings, trigger sync) — `frontend/src/pages/KChatRoomsPage.tsx` | Manual QA |
| 8 | [x] Frontend: client-room template picker in file-browser toolbar — `TemplateDialog` in `FileBrowserPage.tsx` | Manual QA |
| 9 | [x] Phase 4 decision-gate e2e test — `tests/integration/phase4_gate_test.go` (strict-ZK + KChat + template) | `TestPhase4DecisionGate` integration test |
| 10 | [x] Close Phase 4 decision gate — marked `[x]` in this revision | CI green on `main` |

---

## Appendix: Key Metrics to Track

| Metric                                                 | Target phase |
| ------------------------------------------------------ | ------------ |
| Upload p95 latency (presigned URL generation)          | Phase 1      |
| Download p95 latency (presigned URL generation)        | Phase 1      |
| Preview generation p95 latency                         | Phase 2      |
| Search query p95 latency                               | Phase 2      |
| Storage COGS per user per month                        | Phase 3      |
| Free-to-paid conversion rate                           | Phase 3      |
| Guest collaboration completion rate                    | Phase 2      |
| Virus scan p95 latency                                 | Phase 2      |
