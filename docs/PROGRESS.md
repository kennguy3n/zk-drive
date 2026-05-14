# ZK Drive — Changelog

**License**: Proprietary — All Rights Reserved.

This is a high-level changelog of the user-facing and operator-facing
capabilities that have shipped in ZK Drive, grouped by release theme.
For a higher-level narrative, see
[Release History](PHASES.md).

---

## Foundation

- Go service layout with separate API server and worker binaries.
- Postgres schema for workspaces, users, folders, files, file
  versions, permissions, and an activity log.
- Email and password signup, login, and JWT session management.
- Workspace, folder, and file CRUD with nested hierarchy and
  automatic file versioning on re-upload.
- Direct-to-storage upload and download flow via presigned PUT / GET
  URLs minted against the zk-object-fabric S3 endpoint; bytes never
  transit the API server.
- React + TypeScript frontend scaffold (Vite) with login, signup, and
  the file browser.
- Workspace and per-resource permission model (admin, member, viewer,
  editor).
- Async-safe activity logging and soft-delete (trash) for files and
  folders, recoverable for 30 days.

---

## Sharing & Collaboration

- Per-file and per-folder sharing with view, edit, and admin roles.
- Folder permission inheritance: child folders and files inherit
  parent grants unless explicitly overridden.
- Token-based share links with optional password, expiry, and
  atomically-enforced max-download caps.
- Guest invites scoped to a folder, with expiry and email-based
  acceptance.
- Client rooms (folder + share link bundle) for external
  collaboration.
- NATS JetStream async job pipeline with image preview generation,
  ClamAV INSTREAM virus scanning, and Postgres FTS indexing.
- In-app notifications for share link creation, guest invite sent and
  accepted, and virus-scan quarantine events.
- Frontend share dialog, guest invite UI, inline preview thumbnails,
  and a debounced search bar.

---

## Business Readiness

- OAuth2 PKCE SSO against Google and Microsoft, with short-lived
  state cookies and provider-aware user records.
- Security audit log separated from the user-facing activity log,
  recording login, permission changes, admin user management, and
  retention-policy edits with IP and User-Agent.
- Per-folder and per-workspace retention policies with archive and
  delete thresholds; a cold-archive worker gzips expired versions to
  a stable archive key.
- Admin API surface: users, storage usage, audit log, and retention
  policies, all gated behind an `AdminOnly` middleware.
- Billing scaffolding: workspace plans (tier with overridable
  limits), an append-only usage events ledger, and quota checkers for
  storage, users, and bandwidth.
- In-memory rate limiter keyed by `(workspace_id, user_id)`.
- Periodic guest-expiry sweep that revokes permissions on schedule.
- File tagging and bulk multi-select operations (move, copy, delete,
  and zip download capped at 100 files / 1 GiB).
- Playwright end-to-end suite covering signup, admin, and billing
  flows.
- Docker Compose stack and Kubernetes manifests under `deploy/`.

---

## Privacy & Differentiation

- Workspace-to-zk-object-fabric tenant provisioning, with stored
  credentials encrypted at rest via AES-256-GCM and a per-workspace
  storage client factory.
- Placement policy admin endpoint
  (`GET/PUT /api/admin/placement`), validated locally before being
  forwarded to the fabric console.
- Per-folder encryption mode (`managed_encrypted` / `strict_zk`),
  with cross-mode moves rejected as 409 Conflict.
- Strict-ZK behavioural guardrails: preview, virus-scan, index, and
  classification workers ack-and-skip strict-ZK files; the search
  service excludes strict-ZK rows on both file and folder branches.
- Customer-managed key (CMK) URI surface
  (`GET/PUT /api/admin/cmk`) supporting `arn:aws:kms:`, `kms://`,
  `vault://`, and `transit://` URIs.
- KChat integration API under `/api/kchat`: room-folder mapping,
  idempotent permission sync (preserving out-of-band guest grants),
  and attachment metadata flow.
- Content text extraction for managed-encrypted files, persisted to
  `files.content_text` and scored in the FTS expression alongside
  names and tags.
- Rule-based AI thread summary that refuses strict-ZK folders, plus a
  file classification worker on `drive.classify.file` that persists
  `files.classification`.
- Client-room templates for agency, accounting, legal, construction,
  and clinic verticals.
- Frontend admin pages for placement policy, encryption and CMK,
  KChat rooms, and the client-room template picker.

---

## Launch & Revenue

- Stripe webhook handler at `POST /api/webhooks/stripe` with
  signature verification and a 64 KiB request-body cap, handling
  `checkout.session.completed`, `customer.subscription.*`, and
  `invoice.*` events.
- Stripe Checkout and Customer Portal admin endpoints
  (`POST /api/admin/billing/checkout-session` and
  `POST /api/admin/billing/portal-session`) exposed in the frontend
  billing page.
- Progressive Web App shell: `vite-plugin-pwa`, web app manifest,
  Workbox-generated precaching service worker, and an "Install app"
  prompt component.
- Frontend code splitting (`React.lazy` + `Suspense` for admin
  pages); initial gzip bundle dropped well below the 150 kB target.
- Redis-backed session store and a sliding-window distributed rate
  limiter implemented as an atomic Lua script. Both fall back to
  in-memory implementations when `REDIS_URL` is unset.
- WebSocket real-time notifications: Hub with read and write pumps,
  Redis pub/sub fan-out for multi-replica broadcast, and a frontend
  `useNotifications` hook.
- Local-LLM-backed AI summaries via an Ollama-compatible client. The
  constructor refuses any non-loopback or non-RFC1918 endpoint, so
  prompts never leave the operator's network. Default model
  `qwen2.5:1.5b` (Apache-2.0).
- PDF preview support via a `pdftoppm` shell-out, integrated into the
  preview worker pipeline.
- Playwright end-to-end stabilisation: real guest-access and
  strict-ZK specs, no `continue-on-error` anywhere in the e2e
  workflow.
