# ZK Drive — Progress

- **Project**: ZK Drive
- **License**: Proprietary — All Rights Reserved.
- **Status**: Phase 2 — Sharing & Collaboration (kicked off 2026-04-24)
- **Last updated**: 2026-04-24 (sharing, search, client rooms, NATS skeleton)

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

**Status**: `IN PROGRESS`

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
- [ ] Preview worker: generate thumbnails / previews for images,
      PDFs, and office documents using LibreOffice / ImageMagick.
      Store previews as derived objects in zk-object-fabric.
      `internal/preview/`. **Deferred to next Phase 2 sprint.**
- [ ] Virus scan worker: async ClamAV scan on upload. Quarantine
      infected files. Notify admin. `internal/scan/`.
      **Deferred to next Phase 2 sprint.**
- [x] Search: Postgres FTS over file names, folder names, and tags.
      `internal/search/`, `api/drive/`.
- [x] Folder permission inheritance: child folders / files inherit
      parent permissions unless overridden. `internal/permission/`.
- [x] Frontend: sharing dialogs, guest invite UI, file preview
      display, search bar. `frontend/`. (Preview display waits on the
      preview worker; dialogs and search bar landed.)
- [ ] Notification system: in-app notifications for shares, guest
      invites, and scan results. `internal/notification/`.
- [ ] Integration tests: sharing flows, guest access, search, preview
      generation. `tests/integration/` (partial — sharing cap and
      resolve covered; preview generation blocked on worker).
- [ ] Decision gate: validate end-to-end sharing flow — an internal
      user shares a folder with a guest, the guest uploads a file via
      dropbox, the file is scanned and previewed, and all activity is
      recorded in the audit log.

**Decisions / Deferrals (2026-04-24, sprint 2)**:

- PR #5 TOCTOU race on share-link `max_downloads` fixed. The check is
  now a single SQL `UPDATE ... WHERE max_downloads IS NULL OR
  download_count < max_downloads`; the handler surfaces a new
  `ErrLinkExhausted` sentinel as 429. Removed the pre-check in
  `ResolveShareLink` so the atomic increment is the only enforcement
  point. Integration test `TestMaxDownloadsSingleUseAtomic` pins the
  behaviour.
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

---

## Phase 3: Business Readiness (Weeks 9–14)

**Status**: `NOT STARTED`

**Goal**: add SSO, audit logs, retention policies, admin dashboard,
billing integration, and production hardening. This is the phase
where ZK Drive becomes something a paying SME customer can rely on.

Checklist:

- [ ] SSO: OAuth2 with Google and Microsoft. `api/auth/`.
- [ ] Audit log: queryable log of all admin and security-relevant
      actions. `internal/workspace/`, `api/admin/`.
- [ ] Retention policies: per-folder and per-workspace retention
      rules. Automatic archival of old file versions to cold storage
      in zk-object-fabric. `internal/retention/`.
- [ ] Cold archive worker: compress and archive expired file versions
      as objects in zk-object-fabric. `internal/retention/`.
- [ ] Admin dashboard: user management, storage usage, audit log
      viewer, workspace settings. `frontend/`, `api/admin/`.
- [ ] Billing integration: usage metering (storage, bandwidth,
      users), plan enforcement (quota limits). `internal/billing/`.
- [ ] Rate limiting and abuse controls: per-workspace and per-user
      rate limits. `api/middleware/`.
- [ ] Expiring guest access: automatic revocation of guest
      permissions after expiry date. `internal/sharing/`.
- [ ] File tagging: user-defined tags on files for organization and
      search. `internal/file/`.
- [ ] Bulk operations: multi-select move, copy, delete, download (as
      zip). `api/drive/`.
- [ ] Frontend: admin pages, retention settings, billing / usage
      dashboard, bulk operations UI. `frontend/`.
- [ ] Playwright e2e tests: critical user flows (signup, upload,
      share, guest access, admin). `tests/e2e/`.
- [ ] Production deployment configuration: Docker Compose for local
      dev, Kubernetes manifests for production. `deploy/`.
- [ ] Decision gate: a paying SME customer can sign up, create a
      workspace, upload files, share with guests, and the admin can
      view audit logs and set retention policies.

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
