# Facts & Voice — the canonical reference for all zk-drive docs and blogs

This is the single source of truth for everything we publish about zk-drive:
the reference docs in `docs/`, the blog posts in `docs/blog/`, and the
READMEs. Every other document defers to this one. If a claim isn't here (and
traceable to code), don't ship it.

It exists so that independent writers — human or Devin — produce one coherent,
accurate, evergreen story instead of drifting into stale or aspirational copy.

---

## 1. How to use this guide

- **Treat every fact below as verified against the code at the cited
  `path:line`.** When you write a capability, a number, a config key, or an
  endpoint, it must match this guide. If the code and this guide disagree, the
  code wins — fix the guide in the same change.
- **Re-verify before you rely on it.** Code moves. Open the cited file and
  confirm the line still says what's claimed before quoting a number.
- **Use the one demo narrative** (section 7) for every example, walkthrough,
  and screenshot. No second cast of characters, no `acmecorp`.
- **Screenshots come from the evidence pipelines** (section 8), never from
  hand-edited images or stale captures.

---

## 2. Voice & style (evergreen)

These rules are non-negotiable and apply to every doc and blog post.

1. **One current truth — no time framing.** Write as if the product has always
   worked the way it works today. Ban "phase", "version", "v2", "recently
   added", "new in", "now supports", "changelog", "migration from", "legacy",
   "coming soon", "roadmap", "will support". Describe what *is*, in present
   tense.
2. **Code-verified only.** Every capability, limit, default, endpoint, and
   number must trace to source. No aspirational features. If the code doesn't
   do it, we don't claim it.
3. **Honest trade-offs stay.** zk-drive's credibility rests on being candid
   about what each privacy mode gives up. Never imply `managed_encrypted`
   folders are zero-knowledge, and always state plainly that `strict_zk`
   disables server-side preview, search, and malware scanning. Keep the honest
   competitive framing the product is known for.
4. **Plain, operator-grade language.** The audience is an SME admin or
   knowledge worker with no training and no dedicated ops team. Lead with the
   job to be done. Prefer concrete steps and real numbers over adjectives.
5. **Consistent terminology** (section 9). Same word for the same concept
   everywhere.
6. **Show, with the demo tenant.** Ground examples in Northwind Trading and
   (for isolation) Lakeside Legal, using the exact names, folders, and numbers
   in section 7.

---

## 3. Product facts

### 3.1 Architecture

- **One server binary** (`cmd/server`) serves the HTTP API, the WebSocket
  collaboration relay, and the static SPA from a single process. The router is
  chi (`cmd/server/main.go:1406`); the SPA is served from `STATIC_DIR`.
- **A separate worker binary** (`cmd/worker`) drains the background job
  queue (scan, preview, search indexing).
- **PostgreSQL** holds all metadata; **S3-compatible object storage** holds the
  file bytes. File uploads use presigned PUT URLs minted by the server
  (`POST /api/files/upload-url`) and confirmed afterward
  (`POST /api/files/confirm-upload`) — bytes flow client→object store directly.
- **NATS JetStream** drives post-upload jobs (malware scan, preview/thumbnail
  generation, search indexing). Publishing is fire-and-forget async
  (`internal/jobs/publisher.go`).
- **Audit log is HMAC hash-chained** and independently verifiable: each entry
  carries a sequence number, the previous entry's hash, and an HMAC over the
  row (`internal/audit/audit.go:98-106`).

### 3.2 Per-folder privacy modes (the core differentiator)

Defined in `internal/folder/folder.go:14-17`:

| Mode | Value | Server can read? | Preview / search / scan |
| --- | --- | --- | --- |
| Managed encrypted | `managed_encrypted` | Yes (gateway manages keys) | Enabled |
| Strict zero-knowledge | `strict_zk` | No — ciphertext only | Disabled |

- `managed_encrypted` is the default. The server can generate previews and
  thumbnails, run malware scanning, and build the search index.
- `strict_zk` content is encrypted client-side; the server stores opaque
  ciphertext and **every server-side processing path is disabled**. This is the
  honest trade-off to state explicitly wherever strict_zk appears.
- The workspace default mode is configurable
  (`PUT /api/admin/workspace/default-encryption-mode`).

### 3.3 Collaborative documents

- Collab modes (`internal/document/document.go:25-28`): `markdown`, `rich`,
  `rich_presence`, `disabled`.
- `rich_presence` is the live multi-user editing experience (TipTap + Yjs over
  the WebSocket relay) with presence indicators. OnlyOffice is a supported
  editor integration (`internal/collab/onlyoffice.go`).

### 3.4 Sharing & external access

- **Share links** (`POST /api/share-links`) support an optional password,
  expiry (`expires_at`), and a download cap (`max_downloads`); they work on
  both files and folders.
- **Guest invites** (`POST /api/guest-invites`) are folder-scoped (the backend
  models them on a folder, see `ShareDialog.tsx` note and
  `internal/sharing/models.go`). Inviting on a file grants access to its
  parent folder.
- **Client rooms** can be provisioned from templates
  (`POST /api/client-rooms/from-template`).

### 3.5 Security & account protection

- **TOTP two-factor authentication** (`/api/auth/totp/...`,
  `internal/totp`). Enrollment shows a QR plus a manual secret.
- **OAuth / SSO**: Google, Microsoft, and an IAM Core OIDC provider are
  wired (`internal/config/config.go`, `cmd/server/main.go` oauth handler).
- **Rate limiting & auth-failure blocking** are configurable (section 4).

### 3.6 Administration

- **Members**: invite, assign roles (admin / member), and deactivate
  (`/api/admin/users`). Invited members set their own password on first
  sign-in.
- **Audit log**, **retention policies** (`max_versions`, `archive_after_days`;
  workspace-default or per-folder), **storage usage**, and a **health
  dashboard** are all in the admin console (`/api/admin/...`).
- **Data placement** policy (`provider`, `region`, `country`, `storage_class`)
  and **customer-managed keys (CMK)** (`PUT /api/admin/cmk`).
- **Outbound webhooks** per event type (e.g. `file.upload.confirmed`),
  `POST /api/admin/webhooks`.
- **KChat room mappings** (`POST /api/kchat/rooms`) auto-provision a backing
  folder.

### 3.7 Billing tiers

From `internal/billing/billing.go:24-120`. Tier identifiers are the canonical
strings used by both API and UI.

| Tier (`value`) | Storage | Users | Bandwidth / month |
| --- | --- | --- | --- |
| Free (`free`) | 5 GB | 5 | 10 GB |
| Starter (`starter`) | 250 GB | 25 | 100 GB |
| Business (`business`) | 1 TB | 250 | 1 TB |
| Secure Business (`secure_business`) | 5 TB | 1000 | 5 TB |

- Per-workspace overrides can replace any tier default
  (`workspace_plans` row; nil falls back to the tier default).
- A workspace with no plan row falls back to **Free** defaults.
- Exceeding a quota returns **402 Payment Required** (`ErrQuotaExceeded`,
  `internal/billing/billing.go:44-48`).

> Note: the doc-comment in `billing.go` near `TierDefaults` still describes an
> older "10 GB / user" Starter sizing; the **map values above are what the code
> enforces** (Starter = 250 GB). Always quote the map, not the comment.

---

## 4. Configuration keys

All configuration is read from environment variables in
`internal/config/config.go` (see the `Load()` block around
`internal/config/config.go:786`). The high-traffic keys docs should reference:

- **Core**: `DATABASE_URL`, `DATABASE_READ_URL`, `JWT_SECRET`,
  `JWT_ALGORITHM`, `STATIC_DIR`, `LISTEN_ADDR`.
- **Object storage**: `S3_ENDPOINT`, `S3_BUCKET`, `S3_ACCESS_KEY`,
  `S3_SECRET_KEY`.
- **Jobs / scanning**: `NATS_URL`, `CLAMAV_ADDRESS`.
- **Preview worker tuning**: `PREVIEW_BUDGET_PER_WORKSPACE_HOUR` (default 100),
  `PREVIEW_PRIORITY_WORKERS` (6), `PREVIEW_STANDARD_WORKERS` (2),
  `PREVIEW_LIGHTWEIGHT_WORKERS` (8), `PREVIEW_HEAVY_WORKERS` (4).
- **Auth hardening**: `RATE_LIMIT_PER_USER`, `RATE_LIMIT_PER_WORKSPACE`,
  `AUTH_FAILURE_THRESHOLD`, `AUTH_BLOCK_DURATION`, `TRUSTED_PROXY_DEPTH`.
- **SSO / OAuth**: `GOOGLE_CLIENT_ID/SECRET/REDIRECT_URL`,
  `MICROSOFT_CLIENT_ID/SECRET/REDIRECT_URL`,
  `IAM_CORE_ISSUER_URL/CLIENT_ID/CLIENT_SECRET/AUDIENCE/SCOPES/CALLBACK_URL`.
- **Billing**: `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`,
  `STRIPE_PRICE_TIER_MAP`.
- **Audit**: `AUDIT_HMAC_KEY` (falls back to a key derived from `JWT_SECRET`).
- **CSP knob used by ops & e2e**: `SECURITY_HEADERS_CSP_CONNECT_EXTRA`
  (allow-lists the storage gateway origin).

When documenting a config key, cite `internal/config/config.go` and quote the
default the code actually applies.

---

## 5. API surface

The API is mounted under `/api` on the chi router (`cmd/server/main.go:1406`).
Top-level groups (each with its own handler package under `internal/`):

`/api/auth` (incl. `/totp`, OAuth) · `/api/me` · `/api/folders` ·
`/api/files` · `/api/documents` · `/api/share-links` · `/api/guest-invites` ·
`/api/client-rooms` · `/api/search` · `/api/notifications` ·
`/api/permissions` · `/api/kchat` · `/api/setup` · `/api/features` ·
`/api/ws` (collab relay) · `/api/admin/*` (users, audit, retention, storage,
health, placement, cmk, billing/plan, webhooks).

Quote exact paths from the seed (`scripts/seed/seed.py`) — it exercises the
real endpoints, so any path it calls is guaranteed to exist.

---

## 6. Design system (KChat)

The UI follows the KChat look and feel. Tokens live in
`frontend/src/index.css` and `frontend/tailwind.config.js`.

- **Primary** indigo `#553BD8`; **accents** `#6549F2` / `#8578FF`;
  **lavender** `#BAB2FF`; **dark surface** `#191919`.
- **Fonts**: Mona Sans (UI) + Sono (mono), self-hosted.
- **Pill buttons** (`rounded-full`), generous spacing, full light + dark
  theme support.

Describe the product as visually consistent and themeable; do not narrate the
re-theme as an event ("now uses KChat") — per the no-time-framing rule.

---

## 7. The canonical demo narrative

One story, reproduced exactly by `scripts/seed/seed.py`. Use these names,
folders, and numbers verbatim. Demo password for every seeded account:
**`DemoPass!2026`**.

### 7.1 Northwind Trading — primary tenant (an import/distribution SME)

**People** (`seed.py:424-434`)

| Name | Email | Role | Status |
| --- | --- | --- | --- |
| Alice Chen | `alice@northwind.example` | Owner-admin | Active |
| Dave Patel | `dave@northwind.example` | Admin | Active |
| Bob Martinez | `bob@northwind.example` | Member | Active |
| Carol Nguyen | `carol@northwind.example` | Member | Active |
| Eve Thompson | `eve@northwind.example` | Member | Deactivated |

**Folders** (`seed.py:437-443`)

- `Engineering` — managed_encrypted
  - `Architecture` — managed_encrypted (subfolder)
  - `Releases` — managed_encrypted (subfolder)
- `Finance` — managed_encrypted
- `Marketing Assets` — managed_encrypted
- `Legal Contracts` — **strict_zk**
- `Client Vault` — **strict_zk**

**Files with real bytes** (`seed.py:446-511`, `engineering_files()` at
`seed.py:353`)

- Engineering (5): `architecture-overview.pdf`, `api-spec-v3.yaml`,
  `deployment-runbook.md`, `load-test-results.csv`, `security-audit-2026.pdf`
- Finance: `q1-2026-budget.xlsx`, `vendor-invoices.csv`
- Marketing Assets: `brand-guidelines.pdf`, `logo-primary.png`, `launch-plan.md`
- Legal Contracts (strict_zk): `msa-northwind-2026.pdf`, `nda-template.docx`
- Client Vault (strict_zk): `onboarding-packet.pdf`

**Collaboration** — a `rich_presence` document **"Q2 Planning Notes"** in
Engineering (`seed.py:513-519`).

**Sharing** (`seed.py:521-548`)

- File share link on the first Engineering file: password
  `Architecture#2026`, expires `2026-12-31`, `max_downloads` 25.
- Folder share link on Marketing Assets (viewer).
- Guest invite into Client Vault: `client@brightwave-partners.example`
  (viewer).
- Client room from template: **"Brightwave Partners"**.

**Governance & ops**

- Retention (`seed.py:561-573`): workspace default `max_versions` 10,
  `archive_after_days` 365; Legal Contracts `max_versions` 25.
- Billing (`seed.py:575-586`): **business** tier with explicit overrides —
  1 TB storage, 50 users, 5 TB/month bandwidth.
- Data placement (`seed.py:588-600`): provider `aws`, region `us-east-1`,
  country `US`, storage class `STANDARD`; encryption `managed`.
- CMK (`seed.py:601-606`):
  `arn:aws:kms:us-east-1:000000000000:key/northwind-demo-cmk`.
- Default workspace encryption mode: `managed_encrypted`.
- Outbound webhook (`seed.py:614-624`):
  `https://example.com/webhooks/zk-drive` on `file.upload.confirmed`.
- KChat room mapping (`seed.py:627`): `northwind-engineering`.

### 7.2 Lakeside Legal — isolation tenant

A second, fully independent workspace used only to demonstrate that one tenant
can never see another's data (`seed.py:653-668`).

- Owner-admin: **Morgan Reyes** (`morgan@lakeside.example`).
- Folders: `Client Matters` (strict_zk), `Case Briefs` (managed_encrypted)
  with `intake-checklist.pdf`.

### 7.3 Seeding it yourself

```
# A running server at :8080 (see docs OPERATIONS / DEVELOPMENT for the stack).
BASE_URL=http://localhost:8080 python3 scripts/seed/seed.py
```

The built-in demo password is used automatically against a local `BASE_URL`.
Seeding any non-local target requires an explicit `SEED_PASSWORD`, or the
script refuses to run, so demo accounts never get a publicly predictable
credential (`require_safe_password()` at `seed.py:70`).

The script is Python-stdlib-only and **idempotent**: a re-run detects the
existing Northwind owner, re-derives the full state (users, subfolders,
Lakeside), rewrites `scripts/seed/out/state.json`, and exits without creating
duplicates (`derive_existing()` at `seed.py:671`).

---

## 8. Evidence pipelines & screenshot inventory

Two Playwright specs generate every image we publish. **Never commit a
hand-made or stale screenshot.**

### 8.1 Mocked UI tour — `docs/screenshots/`

- Spec: `frontend/e2e/demo-screenshots.spec.ts`.
- No backend: all API calls are mocked from `frontend/e2e/demo-fixtures.ts`,
  so it's deterministic and **runs in CI** (`.github/workflows/e2e.yml`).
- Captures **21 screens in light + dark = 42 files** (light keeps the
  canonical unsuffixed name; dark adds a `-dark` suffix). Screens 01–21 cover
  login, signup, drive root, folder contents, create-folder (both modes),
  search, templates, the full admin suite, billing, placement, encryption,
  KChat rooms, admin health, setup wizard, privacy, documents list, and 2FA
  enrollment.
- Regenerate: `cd frontend && npx playwright test demo-screenshots.spec.ts`.

### 8.2 Live-backend evidence — `docs/blog/img/`

- Spec: `frontend/e2e/blog-evidence.spec.ts`.
- **Installs no mocks** — every image reflects genuine API responses, real
  uploaded bytes, real collab presence, and a real backend-generated TOTP
  secret. Requires the live stack + a seeded `scripts/seed/out/state.json`.
- The suite **skips automatically when no seed file is present** (so CI, which
  has no seed, never runs it — `blog-evidence.spec.ts:35`).
- Captures **26 screens in light + dark = 52 files**, across six journeys:
  public entry, admin owner (drive, folders, create-folder both modes, search,
  share, the admin tabs, billing, placement, encryption, KChat, setup),
  collaborative docs (real TipTap + Yjs editor), account security (real TOTP
  QR), compliance + ops (live hash-chained audit, strict_zk folder, health,
  guest invite), and a knowledge worker (Bob).
- Regenerate: seed first (section 7.3), then
  `cd frontend && npx playwright test blog-evidence.spec.ts`.

`scripts/seed/out/state.json` is git-ignored on purpose — it's a local capture
artifact, and committing it would make CI attempt the live suite and fail.

---

## 9. Terminology

Use the left column; avoid the right.

| Use | Avoid |
| --- | --- |
| workspace / tenant | org, account, team (when meaning the workspace) |
| managed_encrypted (folder) | "encrypted", "normal" (ambiguous) |
| strict_zk / strict zero-knowledge | "private mode", "secure mode" |
| member / admin / owner-admin | user (when role matters) |
| share link | public link |
| guest invite | external share (when meaning the folder invite) |
| collaborative document | doc, note (be specific) |
| object storage | "the cloud", "the bucket" (in user-facing copy) |
| Northwind Trading / Lakeside Legal | Acme, acmecorp, any other sample tenant |

---

_Keep this guide in lockstep with the code. A docs change that contradicts it
is a bug in one or the other — reconcile, don't paper over._
