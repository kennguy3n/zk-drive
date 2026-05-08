# ZK Drive — Phase Summary

**License**: Proprietary — All Rights Reserved.

> Status: Phase 5 — Launch & Revenue (complete).

This document is a high-level snapshot of the five build phases of
ZK Drive. Each phase has a one-paragraph goal and a bullet list of
the headline deliverables that landed in it. For sprint-level
detail, dated decision logs, and the full checklist of every task
that was completed or deferred, see
[PROGRESS.md](PROGRESS.md). For the technical design these phases
are executing against, see [PROPOSAL.md](PROPOSAL.md) and
[ARCHITECTURE.md](ARCHITECTURE.md).

---

## Phase 1: Foundation (Weeks 1–4) — COMPLETE

Stand up the core application layer — Go backend, Postgres schema,
React frontend scaffold, basic folder / file CRUD, direct-to-storage
uploads via zk-object-fabric presigned URLs, and user
authentication. No sharing, no previews, no async jobs yet. The
phase closes once the byte-path round-trip (presigned PUT through
zk-object-fabric back to a presigned GET) works end-to-end.

Key deliverables:

- Go project layout (`cmd/server/`, `cmd/worker/`, `api/`,
  `internal/`).
- Postgres schema for workspaces, users, folders, files, file
  versions, permissions, and an activity log (migrations 001–004).
- Email / password signup, login, and JWT session management.
- Workspace, folder, and file CRUD with nested hierarchy and
  automatic file versioning on re-upload.
- Direct-to-storage upload / download flow via presigned PUT / GET
  URLs minted against the zk-object-fabric S3 endpoint; bytes never
  transit the API server.
- React + TypeScript frontend scaffold (Vite) with login, signup,
  and the file browser.
- Workspace + per-resource permission model (admin / member /
  viewer / editor / admin).
- Async-safe activity logging and soft-delete (trash) for files
  and folders.

---

## Phase 2: Sharing & Collaboration (Weeks 5–8) — COMPLETE

Turn ZK Drive from "cloud storage for one team" into "file
collaboration with clients and partners". Add sharing, guest
access, client rooms, search, and the async job pipeline for
previews, virus scanning, and notifications.

Key deliverables:

- Per-file and per-folder sharing with view / edit / admin roles.
- Token-based share links with optional password, expiry, and
  atomically-enforced max-download caps.
- Guest invites scoped to a folder with expiry and email-based
  acceptance.
- Client rooms (folder + share-link bundle) for external
  collaboration.
- Folder permission inheritance: child folders / files inherit
  parent grants unless explicitly overridden.
- NATS JetStream skeleton + the first three workers: pure-Go image
  preview generator, ClamAV INSTREAM virus scan, and Postgres FTS
  index for file / folder names + tags.
- In-app notification service (share-link created, guest invite
  sent / accepted, scan quarantine).
- Frontend share dialog, guest invite UI, file preview thumbnails,
  and a debounced search bar.

---

## Phase 3: Business Readiness (Weeks 9–14) — COMPLETE

Make ZK Drive something a paying SME customer can rely on. Add
SSO, audit logging, retention policies, the admin surface, billing
metering, rate limiting, and the production deployment artifacts.

Key deliverables:

- OAuth2 PKCE SSO against Google and Microsoft, with
  short-lived state cookies and provider-aware user records.
- Security audit log separated from the user-facing activity log
  (login / logout, permission grant / revoke, admin user
  management, retention-policy changes).
- Per-folder and per-workspace retention policies with archive /
  delete thresholds, plus a cold-archive worker that gzips expired
  versions to a stable archive key.
- Admin API surface: users, storage usage, audit log, retention
  policies, all gated by an `AdminOnly` middleware.
- Billing scaffolding: `workspace_plans` (tier + overridable
  limits), `usage_events` ledger, and quota checkers
  (`CheckStorageQuota`, `CheckUserQuota`,
  `CheckBandwidthQuota`).
- In-memory rate limiter keyed by `(workspace_id, user_id)`.
- File tagging (join table) and bulk multi-select operations
  (move, copy, delete, zip download capped at 100 files / 1 GiB).
- Playwright e2e suite (signup / admin / billing flows) and the
  Docker Compose + Kubernetes manifests under `deploy/`.

---

## Phase 4: Privacy & Differentiation (Weeks 15–22) — COMPLETE

Justify the "privacy-preserving" positioning against Tresorit,
Proton Drive, and Infomaniak kDrive. Add strict-ZK private folders,
customer-managed keys, data residency controls, the KChat
integration API, and the AI / classification scaffolds for
managed-encrypted mode.

Key deliverables:

- Workspace → zk-object-fabric tenant provisioning and the
  per-workspace storage client factory.
- Placement policy admin endpoint (`GET/PUT /api/admin/placement`),
  validated locally before forwarding to the fabric console.
- Per-folder encryption mode (`managed_encrypted` /
  `strict_zk`), with cross-mode moves rejected as 409 Conflict.
- Strict-ZK behavioural guardrails: preview / scan / index /
  classify workers ack-and-skip strict-ZK files, and the search
  service excludes strict-ZK rows on both file and folder
  branches.
- AES-256-GCM credential encryption for stored fabric tenant
  secrets, plus the customer-managed key (CMK) URI surface
  (`GET/PUT /api/admin/cmk`) supporting AWS KMS, Vault, and
  Vault Transit.
- KChat integration API: room-folder mapping, idempotent
  permission sync, and attachment metadata flow under
  `/api/kchat`.
- Rule-based AI thread summary (`internal/ai/`) that refuses for
  strict-ZK folders, plus the file classification worker
  (`drive.classify.file`) persisting `files.classification`.
- Client-room templates for agency, accounting, legal,
  construction, and clinic verticals.
- Frontend admin pages for placement policy, encryption / CMK,
  KChat rooms, and the client-room template picker.
- Native-mobile evaluation recorded in
  [MOBILE_EVALUATION.md](MOBILE_EVALUATION.md); decision is
  PWA-first.

---

## Phase 5: Launch & Revenue (Weeks 23–30) — COMPLETE

Turn the feature-complete product into a revenue-generating,
production-grade SaaS. Wire Stripe for real payments, ship the PWA
shell for mobile, add Redis-backed sessions and WebSocket
notifications for multi-replica readiness, stabilise the e2e
suite, and upgrade the AI scaffold to a local LLM-backed summary.

Key deliverables:

- Stripe integration: webhook handler at `POST
  /api/webhooks/stripe` (signature-verified, 64 KiB cap), and
  Stripe Checkout + Customer Portal admin endpoints
  (`POST /api/admin/billing/checkout-session` /
  `POST /api/admin/billing/portal-session`). `BillingPage.tsx`
  exposes both behind upgrade / manage buttons.
- PWA shell: `vite-plugin-pwa`, `manifest.webmanifest`, Workbox
  precaching service worker, and an `InstallPrompt` component.
- Frontend code splitting (`React.lazy` + `Suspense` for admin
  pages) — initial gzip bundle dropped well below the 150 kB
  target.
- Redis-backed session store (`internal/session/redis.go`) and a
  sliding-window distributed rate limiter
  (`api/middleware/ratelimit_redis.go`). Both gracefully fall back
  to the in-memory implementation when `REDIS_URL` is unset.
- WebSocket real-time notifications: `api/ws/handler.go` Hub +
  read / write pumps, Redis pub/sub fan-out for multi-replica
  broadcast, frontend `useNotifications` hook.
- Local-LLM-backed AI summaries via an Ollama-compatible client
  (`internal/ai/llm.go`). The constructor refuses any non-loopback
  / non-RFC1918 endpoint, so prompts can never leave the operator's
  network. Default model: `qwen2.5:1.5b` (Apache-2.0).
- PDF preview support (`internal/preview/pdf.go`) shelling out to
  poppler-utils `pdftoppm`.
- Playwright e2e stabilisation: real guest-access and strict-ZK
  specs, no `continue-on-error` anywhere in
  `.github/workflows/e2e.yml`.
- `tests/integration/phase5_gate_test.go` (`TestPhase5Gate`) pins
  the full Phase 5 wiring contract in CI.

---

For the dated sprint logs, decision rationale, and the next-N task
queues that drove each phase, see [PROGRESS.md](PROGRESS.md).
