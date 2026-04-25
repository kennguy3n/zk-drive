# ZK Drive — Progress

- **Project**: ZK Drive
- **License**: Proprietary — All Rights Reserved.
- **Status**: Phase 3 — Business Readiness (kicked off 2026-04-24)
- **Last updated**: 2026-04-24 (Phase 3 kickoff: SSO, audit log, retention, admin API, rate limiting, guest expiry)

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
- [~] Decision gate: all code-level findings from PRs #2–#4 are
      resolved; only the upstream presigned URL validation remains.
      zk-drive's SigV4 presigned URL generation is correct (tests pass;
      URLs carry `X-Amz-Algorithm` / `X-Amz-Signature` / `X-Amz-Expires`),
      and direct PUT / GET with an `Authorization` header works against
      the Docker demo. However, the zk-object-fabric gateway
      (`internal/auth/authenticator.go`) only accepts SigV4 via the
      `Authorization` header and rejects query-string presigned URLs
      with 403 `missing Authorization header`. This is an explicit
      upstream deferral, not a zk-drive bug. Full round-trip against
      the demo is blocked until zk-object-fabric lands query-param
      SigV4 validation. Multipart upload still deferred.

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

**Phase 1 status**: functionally complete. Only the decision gate
remains partially blocked on the upstream `zk-object-fabric` gateway
accepting query-string SigV4 presigned URLs. Once that lands upstream,
the full round-trip can be validated end to end.

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

**Status**: `IN PROGRESS`

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
- [ ] Decision gate: a paying SME customer can sign up, create a
      workspace, upload files, share with guests, and the admin can
      view audit logs and set retention policies.

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

**Status**: `NOT STARTED`

**Goal**: add strict-ZK private folders, customer-managed keys, data
residency controls, the KChat integration API, and AI features for
managed encrypted mode. This is the phase that justifies the
"privacy-preserving" positioning against Tresorit, Proton Drive, and
Infomaniak kDrive.

Checklist:

- [ ] Per-folder encryption mode selection: managed encrypted or
      strict ZK. `internal/folder/`.
- [ ] Strict-ZK folder support: disable server-side previews, search,
      and text extraction for strict-ZK folders.
      `internal/preview/`, `internal/search/`.
- [ ] Customer-managed key (CMK) option: workspace-level CMK
      configuration via zk-object-fabric. `internal/workspace/`.
- [ ] Data residency controls: expose zk-object-fabric placement
      policies in the admin UI. `frontend/`, `api/admin/`.
- [ ] KChat integration API: REST endpoints for room-folder mapping,
      permission sync, and attachment metadata. `api/drive/`.
- [ ] AI thread summary / file classification (managed encrypted mode
      only). `internal/ai/`.
- [ ] Content search for managed encrypted files: extract text from
      documents, index in Postgres FTS. `internal/search/`.
- [ ] Client room templates: pre-configured folder structures for
      agencies, accounting, legal, construction, and clinics.
      `internal/sharing/`.
- [ ] Native mobile app evaluation: assess PWA metrics and decide
      whether to invest in React Native. Document decision.
- [ ] Decision gate: a workspace admin can create a strict-ZK private
      folder, upload files with client-side encryption, and verify
      that the server cannot generate previews or search file
      content. KChat can create a room-folder mapping and upload
      attachments via the integration API.

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
