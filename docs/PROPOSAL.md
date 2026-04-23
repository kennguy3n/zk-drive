# ZK Drive — Technical Proposal

**License**: Proprietary — All Rights Reserved.

> Status: Phase 1 — Foundation (not started). This document defines the
> target architecture and product strategy, not the current
> implementation. See [PROGRESS.md](PROGRESS.md) for build status and
> [ARCHITECTURE.md](ARCHITECTURE.md) for the system architecture.

---

## 1. Executive Summary

- **What**: ZK Drive is a privacy-preserving document management and
  file collaboration platform built on top of [zk-object-fabric](https://github.com/kennguy3n/zk-object-fabric).
  It exposes a familiar drive UI (folders, files, sharing, previews,
  guest rooms) while ensuring data privacy through zero-knowledge
  encryption and provider-neutral object storage.
- **Why**: The market leaves a clear gap between consumer-grade
  bundled suites (Google Drive, OneDrive, iCloud, Dropbox) and
  enterprise-grade secure file management (Box, Tresorit, SharePoint,
  Egnyte). SMEs, agencies, consultancies, and professional-services
  firms need **governed file collaboration with privacy, data
  residency, and predictable cost** — without enterprise complexity
  and without "unlimited" plans that hide egress fees.
- **How**: Go backend, React + TypeScript frontend, Postgres for
  metadata, zk-object-fabric S3 API for all file content. Encryption,
  caching, placement, and backend migration are delegated entirely to
  zk-object-fabric. ZK Drive owns the application layer only.
- **For whom**:
  - **SME teams** that want secure internal file storage with
    privacy-respecting defaults.
  - **Agencies, consultancies, and professional-services firms**
    that need client rooms, guest dropboxes, expiring links, and
    audit logs for external collaboration.
  - **KChat** (the B2B team chat product) uses ZK Drive as its
    storage backbone. Every room maps to a folder; every attachment
    lives in ZK Drive.
- **License**: Proprietary.
- **Key strategic insight**: ZK Drive is an **application layer**,
  not a storage layer. It does not reimplement encryption, caching,
  placement, provider migration, or S3 compatibility. It rides on
  zk-object-fabric's phase-invariant S3 API and inherits its cost
  curve — which is what makes the ZK Drive business model work.

### 1.1 Tech stack

- **Backend: Go.** Drive API, permission evaluation, sharing, retention,
  and async workers. Go is chosen for operational scalability, developer
  velocity, and consistency with the rest of the KChat / zk-object-fabric
  stack.
- **Frontend: React + TypeScript (Vite).** Drive UI, sharing dialogs,
  preview viewers, admin pages, and settings.
- **Metadata DB: Postgres** (partitioned by workspace).
- **Cache / sessions: Redis / Valkey.**
- **Object storage: zk-object-fabric S3 API.** All file content,
  versions, previews, and archives.
- **Async jobs: NATS JetStream.** Preview, scan, index, retention,
  and archive workers.
- **Search: Postgres FTS first.** OpenSearch or Meilisearch layered
  on top only when corpus size or query volume exceeds Postgres FTS
  capabilities.

---

## 2. Market Analysis

### 2.1 Competitive landscape

| Segment                         | Examples                                      | Strengths                                          | Weaknesses for ZK Drive's ICP                                         |
| ------------------------------- | --------------------------------------------- | -------------------------------------------------- | --------------------------------------------------------------------- |
| Bundled productivity suites     | Google Drive, OneDrive, iCloud Drive          | Ubiquitous, deep office-suite integration          | Provider can read files; minimal data-residency control               |
| Enterprise file management      | Box, SharePoint, Egnyte                       | Strong governance, DLP, compliance                 | Expensive (~$15–$47 / user / mo), complex, over-featured for SMEs     |
| Encrypted storage               | Tresorit, Proton Drive, SpiderOak             | True client-side encryption                        | Weak collaboration, no client rooms, limited admin tooling            |
| Low-cost storage                | Dropbox, pCloud, MEGA, Nextcloud (self-host)  | Cheap, wide device support                         | Not privacy-first; self-host requires ops capacity SMEs lack          |
| Swiss / sovereign               | Infomaniak kDrive, Tresorit                   | Strong data-residency story                        | Smaller ecosystems; kDrive branding collision risk                    |

### 2.2 Pricing benchmarks

| Product                          | Price (USD / user / month)                 | Notes                                                      |
| -------------------------------- | ------------------------------------------ | ---------------------------------------------------------- |
| Google Workspace                 | $7 – $22                                   | Bundled productivity, 30 GB – 5 TB per user                |
| Dropbox Business                 | $15 – $24                                  | "Advanced" and "Enterprise" tiers; egress unlimited        |
| Box Business                     | $15 – $47                                  | Heavy compliance focus, unlimited storage on higher tiers  |
| Tresorit Business                | €12 – €16                                  | Zero-knowledge, Swiss / EU residency                       |
| Infomaniak kDrive                | from CHF 1.76                              | Low price, Swiss-hosted; limited collaboration primitives  |
| Nextcloud hosted                 | €3 – €9                                    | Self-host variants exist                                   |
| Proton Drive                     | €4 – €10                                   | Zero-knowledge, consumer-first UX                          |

### 2.3 Pricing strategy

Storage cost and bandwidth cost are **always separated** in ZK Drive
pricing. There is **no "unlimited storage"** tier. The fair-use
economics of the underlying zk-object-fabric backends (Wasabi's
90-day minimum, fair-use egress, etc.) make "unlimited" a dishonest
promise.

| Tier              | Price (USD)                    | Seats / Storage                                                    | Target                                      |
| ----------------- | ------------------------------ | ------------------------------------------------------------------ | ------------------------------------------- |
| Free              | $0                             | Up to 5 users, 5 GB workspace pool                                 | Personal and micro-team trial               |
| Starter           | $2 – $3 / user / mo            | 10 GB / user pooled, 2 guests / workspace                          | Small teams, freelancers                    |
| Business          | $4 – $6 / user / mo            | 50 GB / user pooled, SSO (Google / Microsoft), audit log           | SMEs, agencies, consultancies               |
| Secure Business   | $6 – $9 / user / mo + storage  | CMK, strict-ZK folders, data residency controls                    | Regulated SMEs, legal, healthcare-adjacent  |
| Dedicated / BYOC  | Platform fee + usage           | Dedicated cell via zk-object-fabric, customer-managed placement    | Sovereign customers, PB-scale footprints    |

### 2.4 Storage economics

ZK Drive **inherits zk-object-fabric's cost curve**:

- **Phase 1** — file content lands on Wasabi via the zk-object-fabric
  Linode data plane at ~$6.99 / TB-mo, fair-use egress ≤ 1× stored.
- **Phase 2+** — local DC cells (Ceph RGW) bring the effective cost
  below $3 / TB-mo while keeping the S3 API identical.
- **Pooled org storage** — quota is pooled across the workspace, not
  fixed per seat. A 10-seat Business workspace with 50 GB / user sees
  500 GB of shared pool; nobody hits a personal cap while the
  workspace is under its aggregate limit.
- **Egress is metered transparently** — no hidden fair-use cliff.
  Heavy sharing / download patterns flow into the Secure Business or
  Dedicated / BYOC tier rather than silently degrading.

### 2.5 Key insight: the KChat + ZK Drive bundle

The most valuable pricing insight is the **bundle**:

> **KChat Business + ZK Drive** — team chat, client rooms, secure
> file rooms, pooled storage, retention, audit, and guest
> collaboration for **$3 – $4 / user / month**.

At that price, ZK Drive + KChat is cheaper than Google Workspace,
cheaper than Dropbox Business, cheaper than Box, and carries the
privacy / residency differentiation of Tresorit or Proton Drive. The
bundle is what makes the ZK Drive product line commercially
interesting; the standalone SKU is a trial lane into the bundle for
companies that need file storage first and chat later.

---

## 3. Product Design

### 3.1 Positioning

> **ZK Drive** — secure file storage and collaboration with
> zero-knowledge encryption, data residency, and governed sharing for
> teams, clients, and partners.

The positioning is deliberately narrow. ZK Drive is **not** a Google
Workspace competitor; it does not ship an office suite or real-time
collaborative editor. It is a **file collaboration** product for
organizations that already have (or do not need) a productivity
suite, and that care about privacy, residency, and governance.

### 3.2 Core feature set

| Feature                   | Phase 1  | Phase 2 | Phase 3 | Phase 4 |
| ------------------------- | -------- | ------- | ------- | ------- |
| Folder / file CRUD        | Yes      | Yes     | Yes     | Yes     |
| File versioning           | Yes      | Yes     | Yes     | Yes     |
| Internal sharing          | Basic    | Full    | Full    | Full    |
| External sharing (links)  | —        | Yes     | Yes     | Yes     |
| Guest access              | —        | Yes     | Yes     | Yes     |
| Client rooms / dropbox    | —        | Yes     | Yes     | Yes     |
| Previews                  | —        | Yes     | Yes     | Yes     |
| Full-text search          | —        | Yes     | Yes     | Yes     |
| Virus scanning            | —        | Yes     | Yes     | Yes     |
| Trash / soft delete       | Yes      | Yes     | Yes     | Yes     |
| Activity log              | Basic    | Full    | Full    | Full    |
| Retention policies        | —        | —       | Yes     | Yes     |
| Admin dashboard           | —        | —       | Yes     | Yes     |
| SSO (Google / Microsoft)  | —        | —       | Yes     | Yes     |
| CMK / strict-ZK folders   | —        | —       | —       | Yes     |
| Data residency controls   | —        | —       | —       | Yes     |
| KChat integration API     | —        | —       | —       | Yes     |
| Offline access            | —        | —       | —       | —       |

### 3.3 Encryption modes

ZK Drive does **not** implement its own encryption. It selects a
zk-object-fabric mode per folder and stores that selection in
metadata. Each mode carries an honest tradeoff table:

| Product name            | zk-object-fabric mode | Who holds keys                  | Server previews | Server search | Admin recovery |
| ----------------------- | --------------------- | ------------------------------- | --------------- | ------------- | -------------- |
| Business Secure (default) | `ManagedEncrypted`  | Gateway / workspace             | Yes             | Yes           | Yes            |
| Private Folders (opt-in)| `StrictZK`            | Customer client SDK             | No              | Metadata only | No             |
| Customer Key Control    | `StrictZK` + CMK      | Customer KMS                    | No              | Metadata only | No             |

Tradeoffs to document and enforce in the UI:

- **Business Secure** is **not** strict zero-knowledge. The
  zk-object-fabric gateway can read plaintext in memory during
  request handling. This is the right default for most SME use cases
  because it enables previews, search, virus scanning, and admin
  password reset — but it must be called "confidential managed
  storage," not "zero-knowledge," in customer-facing UI.
- **Private Folders** lose previews, thumbnail generation, server-
  side text extraction, and server-side full-text search. Clients
  can still search file and folder **names** (metadata is not
  encrypted at the application layer) but not **contents**.
- **Customer Key Control** is strict ZK plus customer-held KMS.
  Losing the key loses the data. No admin recovery path exists.

### 3.4 Differentiating features

| Feature                                   | Why it differentiates                                                                     |
| ----------------------------------------- | ----------------------------------------------------------------------------------------- |
| Room = folder + chat + tasks (with KChat) | One surface for file collab + team chat + lightweight task tracking                        |
| Client rooms with dropbox uploads         | External clients can upload without an account; scoped, expiring, audited                  |
| Pooled org storage                        | No per-seat cap wasted; storage moves to whoever needs it inside the workspace             |
| Privacy mode per folder                   | Managed encrypted by default; strict ZK available per folder without migrating the tenant  |
| Data residency in the admin UI            | Expose zk-object-fabric placement policies as first-class admin choices                    |
| Transparent archive pricing               | Cold-archived versions are priced at the archive rate, visibly, with no surprise egress    |
| Direct-to-storage uploads                 | Client uploads bypass the ZK Drive API for bytes, cutting CPU and bandwidth on the control plane |

### 3.5 Features to avoid first

| Feature                          | Why to defer                                                                            |
| -------------------------------- | --------------------------------------------------------------------------------------- |
| Full office suite                | Google / Microsoft own this; integrate via viewer/preview, do not rebuild               |
| Real-time collaborative editing  | Huge build cost; integrate OnlyOffice or Collabora in Phase 4+ if demand appears        |
| Desktop sync client              | Native sync is an ops tax and a security surface; PWA + browser first                    |
| Video / image editing            | Out of ICP; use previews only                                                           |
| Unlimited storage tiers          | Dishonest; breaks margin model; incompatible with zk-object-fabric fair-use economics    |
| Enterprise DLP / legal hold      | Requires heavyweight compliance workstream; address in a future "Enterprise" SKU         |

---

## 4. KChat Integration Design

KChat is a separate B2B team chat product. ZK Drive is its storage
backbone. The integration is **one-directional**: KChat depends on
ZK Drive, but ZK Drive does not import any KChat code, does not run
any KChat-specific processes, and ships as a standalone product.

### 4.1 How KChat uses ZK Drive

- **Room attachments** — when a KChat user attaches a file, the KChat
  client requests a presigned PUT URL from ZK Drive, uploads directly
  to zk-object-fabric, then posts the file's ZK Drive metadata ID as
  the attachment reference in the chat message.
- **Room folders** — every KChat room is 1:1 with a ZK Drive folder.
  Room membership changes sync to ZK Drive folder permissions via the
  KChat integration API.
- **Client room dropbox** — KChat client rooms let external clients
  upload to a scoped dropbox folder without a full KChat account,
  using ZK Drive guest invites.
- **Voice notes** — KChat voice notes are short audio files stored
  in the room folder.
- **Call recordings** — KChat video / voice call recordings (Business+
  tier) are stored as files in the room folder and indexed for
  playback.
- **Cold message archive** — old KChat messages are compressed to
  JSONL or Parquet and stored as objects in ZK Drive for long-term
  retention and cost control.
- **Exports / eDiscovery** — admin exports land in a dedicated export
  bucket in zk-object-fabric, accessed through ZK Drive.
- **File previews** — KChat clients render previews via ZK Drive's
  preview service.
- **Virus scanning** — every KChat attachment is scanned via the ZK
  Drive scan pipeline before it is visible to other room members.

### 4.2 Integration API

There are **two** integration interfaces:

1. **zk-object-fabric S3 API** — KChat clients upload and download
   file bytes directly via presigned URLs. Neither KChat nor ZK
   Drive proxies the bytes.
2. **ZK Drive application API** (REST) — KChat server talks to ZK
   Drive for folder management, permission sync, attachment
   metadata, guest invite creation, and retention configuration.

This two-interface split is the same pattern as the ZK Drive web UI
itself, which also uses zk-object-fabric for bytes and the ZK Drive
REST API for everything else.

### 4.3 Storage usage pattern

| KChat data                    | Where it lives                         | Notes                                                     |
| ----------------------------- | -------------------------------------- | --------------------------------------------------------- |
| Chat attachments              | ZK Drive (room folder)                 | Presigned-URL direct upload                               |
| Attachment previews           | zk-object-fabric derived-object bucket | Generated by ZK Drive preview worker                      |
| Voice notes                   | ZK Drive (room folder)                 | Short audio, same path as attachments                     |
| Call recordings               | ZK Drive (room folder, Business+)      | Subject to retention                                      |
| Cold message archive          | zk-object-fabric archive bucket        | Compressed JSONL / Parquet; priced at archive rate        |
| Exports / eDiscovery          | zk-object-fabric export bucket         | Expiring presigned URLs to requester                      |
| **Hot chat messages**         | **KChat Postgres (NOT ZK Drive)**      | Messages are KChat-owned state, not files                 |

The last line is load-bearing: hot chat messages are **not** files.
They live in KChat's own Postgres. ZK Drive is only involved once a
message has an attachment or once messages age into cold archive.

---

## 5. Architecture Decisions

### 5.1 ZK Drive is an application layer, not a storage layer

ZK Drive does **not** reimplement:

- Encryption envelopes or per-object DEKs.
- Hot-object caching.
- Placement policy evaluation.
- Backend migration or dual-write.
- S3 compatibility.

All of the above are owned by zk-object-fabric. This decision is the
single biggest cost and risk reduction in the ZK Drive plan. If the
team tried to rebuild these primitives, we would double our
engineering scope and gain nothing — zk-object-fabric already has
them and already passes its S3 compliance test suite against every
adapter.

### 5.2 File content never lives in Postgres

All file content — bytes on disk, bytes in memory during a request,
bytes in a versioned copy — lives in zk-object-fabric via the S3 API.
Postgres holds only metadata: workspace ID, folder ID, file ID,
version number, object key (the S3 key pointing into
zk-object-fabric), size, mime type, checksum, and timestamps.

This decision keeps Postgres small, keeps backups cheap, and ensures
that encryption and placement are always delegated to the layer that
is built for them.

### 5.3 Direct-to-storage uploads

ZK Drive **never proxies file bytes**. The upload flow is:

1. Client calls `POST /api/files/upload-url` with the target folder
   and filename.
2. ZK Drive checks permissions and calls zk-object-fabric to generate
   a presigned PUT URL scoped to a single object key.
3. ZK Drive returns the presigned URL and a server-issued upload ID
   to the client.
4. Client uploads bytes **directly to zk-object-fabric** using the
   presigned URL. ZK Drive is not in this path.
5. Client calls `POST /api/files/confirm-upload` with the upload ID,
   final size, and checksum.
6. ZK Drive records the file version in Postgres, dispatches async
   jobs to NATS (preview, scan, index), and returns the file
   metadata.

The download flow is the mirror: ZK Drive returns a presigned GET
URL; the client downloads directly from zk-object-fabric.

This pattern keeps ZK Drive's API servers small and cheap. They never
move the file bytes, so a single server can broker thousands of
concurrent uploads.

### 5.4 Postgres FTS before dedicated search

Phase 1–2 search runs on Postgres full-text search (`tsvector` /
`tsquery`). This handles file names, folder names, tags, and text
extracted from managed-encrypted documents by the index worker.

Dedicated search (OpenSearch or Meilisearch) is introduced only when:

- Workspaces routinely exceed a few million files, or
- p95 search latency exceeds target (~500 ms), or
- Query language demands exceed what Postgres FTS offers (for
  example, relevance tuning or faceted search across tenants).

Postgres FTS is free, operationally cheap, and good enough for SME
workloads. Dedicated search is a real operational investment and
deferring it until we need it keeps Phase 1–2 small.

### 5.5 NATS JetStream for async jobs

Preview, virus scan, index, retention, and archive jobs run as NATS
JetStream consumers. Each job type is a separate subject with its own
worker pool. JetStream gives us:

- Durable delivery with ack semantics.
- Per-subject scaling (many preview workers, few archive workers).
- Retry and dead-letter semantics without a separate DB.
- A natural path to extend KChat signals into ZK Drive workers.

Redis streams, Kafka, and RabbitMQ were considered. Kafka is too
heavy for Phase 1 operational footprint; Redis streams lack the
durability guarantees; RabbitMQ adds an unrelated operational
runtime. NATS is already in the KChat / sn360 operational stack.

---

## 6. Build vs. Integrate

| Concern                        | Decision   | Source                                      |
| ------------------------------ | ---------- | ------------------------------------------- |
| Object storage                 | Integrate  | zk-object-fabric S3 API                     |
| Folder / file metadata         | Build      | `internal/folder/`, `internal/file/`        |
| Permissions / sharing          | Build      | `internal/permission/`, `internal/sharing/` |
| Preview generation             | Integrate  | LibreOffice + ImageMagick + FFmpeg          |
| Virus scanning                 | Integrate  | ClamAV                                      |
| Search                         | Build      | Postgres FTS (phase 1–2)                    |
| Authentication                 | Build      | `api/auth/` (+ OAuth2 for Google / MS SSO)  |
| Real-time collaboration        | Defer      | Integrate OnlyOffice / Collabora later      |
| Desktop sync                   | Defer      | —                                           |

"Build" means we own the code and the behavior. "Integrate" means we
wrap an existing tool or service and do not try to improve on it.
"Defer" means we explicitly don't build or integrate until a later
phase.

---

## 7. Cost Architecture

### 7.1 COGS formula

ZK Drive's **application-layer COGS per user per month** is
approximately:

```
COGS/user/mo = storage_cost/user/mo          # inherited from zk-object-fabric
             + preview_compute/user/mo        # NATS + preview workers
             + scan_compute/user/mo           # ClamAV scanner workers
             + search_index/user/mo           # Postgres FTS updates
             + application_compute/user/mo    # Go API + React static hosting
             + bandwidth/user/mo              # direct-to-storage keeps this low
```

Storage cost dominates and is inherited from zk-object-fabric.
Compute terms are small because (1) preview and scan are async and
amortized across many users, (2) direct-to-storage uploads keep the
API server idle on the byte path, and (3) React is served statically
from CDN.

### 7.2 Margin targets by tier

| Tier              | Target margin            | Notes                                                              |
| ----------------- | ------------------------ | ------------------------------------------------------------------ |
| Free              | Negative (acquisition)   | Capped at 5 users and 5 GB; converts to Starter via quota pressure |
| Starter           | > 50%                    | Managed encrypted only; most storage still on pooled Wasabi        |
| Business          | > 60%                    | Pooled storage, SSO, audit; preview / scan fully baked in          |
| Secure / BYOC     | > 65%                    | CMK and strict-ZK reduce compute cost (no previews); placement fee |

Free must convert. Free is not a sustainable SKU on its own; it is a
trial lane. The 5-user / 5 GB cap is chosen to be useful enough to
onboard a small team but not enough to replace a real plan.

---

## 8. Risks and Mitigations

| Risk                                                | Impact                            | Mitigation                                                                                              |
| --------------------------------------------------- | --------------------------------- | ------------------------------------------------------------------------------------------------------- |
| zk-object-fabric S3 API stability                   | Hard dependency on upstream       | Pin zk-object-fabric version; run ZK Drive integration tests against the same S3 compliance suite       |
| Preview compute cost                                | Margin erosion on Business tier   | Queue-based amortization; cap preview size per file; cache rendered previews in derived-object bucket   |
| Search quality                                      | Poor UX on large workspaces       | Keep Phase 1–2 on Postgres FTS; gate OpenSearch rollout on real metrics, not speculation                |
| Guest / external abuse                              | Spam uploads, malware vectors     | Rate limit guest uploads; virus scan before visibility; admin-approved guest invites above a threshold  |
| Competing with Google / Microsoft                   | Positioning confusion             | Explicit non-goal to ship an office suite; lean into privacy and client-collaboration use cases         |
| KChat coupling                                      | One product blocks the other      | One-directional dependency (KChat → ZK Drive); ZK Drive ships standalone without KChat installed       |
| Brand confusion with Infomaniak kDrive              | SEO / marketing drag              | Product name is **ZK Drive**, not kDrive; emphasize the "ZK" prefix consistently in all surfaces        |

---

## 9. What NOT to Build First

The following items are explicitly **out of scope for Phase 1 – 3**.
They are tempting, but any of them will slow the product down without
materially improving the value proposition.

| Item                                           | Why                                                                                       |
| ---------------------------------------------- | ----------------------------------------------------------------------------------------- |
| Full office suite                              | Google / Microsoft own this. Ship a preview viewer, not an editor.                        |
| Desktop sync client                            | Native sync is an ops tax and a security surface. PWA + browser first; native only later. |
| Real-time collaborative editing                | Enormous build cost. If demand appears, integrate OnlyOffice / Collabora in Phase 4+.     |
| Unlimited storage tiers                        | Dishonest; incompatible with zk-object-fabric fair-use economics; destroys margin.        |
| Video conferencing                             | Wrong product. KChat covers calls; ZK Drive stores recordings.                            |
| Enterprise DLP / legal hold / eDiscovery       | Belongs in a future Enterprise SKU with dedicated compliance engineering.                 |
| Native mobile apps                             | PWA + responsive web first. Reassess only when PWA metrics show adoption loss.            |
| AI-powered features                            | Available in Phase 4 only, and only in managed encrypted mode where extraction is legal.  |

The early product is: a clean drive UI, zk-object-fabric-backed
storage, sharing and guest access, previews and scanning, and a
small admin surface. That is already a viable SME product.
