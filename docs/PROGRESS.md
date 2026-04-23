# ZK Drive — Progress

- **Project**: ZK Drive
- **License**: Proprietary — All Rights Reserved.
- **Status**: Phase 1 — Foundation (in progress)
- **Last updated**: 2026-04-23

This document is a phase-gated tracker. Each phase has an explicit
checklist and a decision gate. Do not skip to the next phase until
the current phase's gate has been met.

For the technical design, see [PROPOSAL.md](PROPOSAL.md). For the
architecture, see [ARCHITECTURE.md](ARCHITECTURE.md).

---

## Phase 1: Foundation (Weeks 1–4)

**Status**: `IN PROGRESS`

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
- [ ] File versioning: automatic version creation on re-upload.
      `internal/file/`.
- [ ] Direct-to-storage upload flow: presigned PUT URL generation via
      zk-object-fabric S3 API, upload confirmation endpoint, metadata
      recording. `api/drive/`.
- [ ] Direct-to-storage download flow: presigned GET URL generation
      with permission check. `api/drive/`.
- [ ] React frontend scaffold: Vite + React + TypeScript. Login /
      signup page, file browser page, upload component. `frontend/`.
- [ ] Basic permission model: workspace-level roles (admin, member).
      `internal/permission/`.
- [ ] Activity logging: record file / folder operations in
      `activity_log` table. `internal/workspace/`.
- [x] Soft delete (trash): deleted files / folders marked with
      `deleted_at`, recoverable for 30 days. `internal/file/`,
      `internal/folder/`.
- [x] Integration tests: API-level tests for folder CRUD, file upload
      / download, auth. `tests/integration/` (partial — auth,
      workspace, folder, file CRUD).
- [ ] Decision gate: confirm zk-object-fabric S3 API is stable enough
      for ZK Drive's upload / download flows. Validate presigned URL
      generation, multipart upload, and basic GET / PUT against a
      running zk-object-fabric instance.

**Decisions / Deferrals (2026-04-23)**:

- File versioning, upload/download flows, and permission model deferred
  to next batch.
- zk-object-fabric repo not yet accessible for S3 API validation;
  decision gate deferred.
- Activity logging deferred to next batch (will hook into existing CRUD
  operations).
- Soft delete implemented for folders and files (`deleted_at` column,
  excluded from listings).
- React frontend scaffold deferred to next batch.

---

## Phase 2: Sharing & Collaboration (Weeks 5–8)

**Status**: `NOT STARTED`

**Goal**: add sharing, guest access, client rooms, search, and the
async job pipeline for previews and virus scanning. This is the
phase that turns ZK Drive from "cloud storage for one team" into
"file collaboration with clients and partners."

Checklist:

- [ ] Per-file and per-folder sharing with roles (view / edit /
      admin). `internal/sharing/`, `api/sharing/`.
- [ ] Share links: token-based, optional password, optional expiry,
      optional max downloads. `internal/sharing/`.
- [ ] Guest invites: invite external users by email with scoped
      folder access and expiry. `internal/sharing/`.
- [ ] Client rooms: dedicated shared folders for external clients /
      partners with dropbox upload capability.
      `internal/sharing/`.
- [ ] NATS JetStream setup: job dispatch for preview, scan, index,
      retention. `cmd/worker/`.
- [ ] Preview worker: generate thumbnails / previews for images,
      PDFs, and office documents using LibreOffice / ImageMagick.
      Store previews as derived objects in zk-object-fabric.
      `internal/preview/`.
- [ ] Virus scan worker: async ClamAV scan on upload. Quarantine
      infected files. Notify admin. `internal/scan/`.
- [ ] Search: Postgres FTS over file names, folder names, and tags.
      `internal/search/`, `api/drive/`.
- [ ] Folder permission inheritance: child folders / files inherit
      parent permissions unless overridden. `internal/permission/`.
- [ ] Frontend: sharing dialogs, guest invite UI, file preview
      display, search bar. `frontend/`.
- [ ] Notification system: in-app notifications for shares, guest
      invites, and scan results. `internal/notification/`.
- [ ] Integration tests: sharing flows, guest access, search, preview
      generation. `tests/integration/`.
- [ ] Decision gate: validate end-to-end sharing flow — an internal
      user shares a folder with a guest, the guest uploads a file via
      dropbox, the file is scanned and previewed, and all activity is
      recorded in the audit log.

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
