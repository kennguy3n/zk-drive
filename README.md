# zk-drive

**Privacy-first, multi-tenant secure document management for small and
mid-sized teams — and the storage backbone for KChat.**

zk-drive is a secure place for an SME to keep, organize, edit, and share its
files: a familiar drive (folders, files, previews, search, sharing) that a
team can run with **no training and no dedicated ops**. It is an alternative
to Google Drive, Dropbox, and OneDrive for organizations that care about who
can read their data and where it lives.

The thing that sets zk-drive apart is a choice you make **per folder**:

| Privacy mode | Folder value | Can the server read the contents? | Preview · search · malware scan |
| --- | --- | --- | --- |
| **Managed encrypted** (default) | `managed_encrypted` | Yes — the gateway manages the keys | Enabled |
| **Strict zero-knowledge** | `strict_zk` | No — the server only ever stores ciphertext | Disabled |

`managed_encrypted` is the default because it powers previews, full-text
search, and malware scanning — the right trade-off for most everyday work.
`strict_zk` encrypts content on the client so the server holds nothing but
ciphertext; in exchange, **server-side preview, search, and malware scanning
are turned off** for that folder. zk-drive states this trade-off plainly
everywhere it appears — being honest about it is part of the product.
(`internal/folder/folder.go:14-17`.)

![The drive root in zk-drive, showing a team's top-level folders with their privacy-mode badges](docs/screenshots/03-drive-root-folders.png)

## What you can do with it

- **Keep and organize files.** Nested folders, file versions, previews,
  and full-text search across the files the server can read.
- **Choose privacy per folder.** Mark any folder `managed_encrypted` or
  `strict_zk` when you create it; the dialog shows exactly what each mode
  gives up before you commit. The workspace default mode is itself
  configurable (`PUT /api/admin/workspace/default-encryption-mode`).
- **Edit together, live.** Collaborative documents support live multi-user
  editing with presence (`rich_presence`), plus plain `markdown` and `rich`
  modes; OnlyOffice is a supported editor integration
  (`internal/document/document.go:25-28`, `internal/collab/onlyoffice.go`).
- **Share inside and outside the team.** Share links (with optional
  password, expiry, and download cap), folder-scoped guest invites for
  external collaborators, and client rooms provisioned from templates.
- **Administer and govern.** Invite members and assign roles, read a
  tamper-evident audit log, set retention policies, watch storage usage and
  a health dashboard, pin data placement, bring your own keys (CMK), and
  send outbound webhooks — all from the admin console.
- **Work from any device.** A web app (installable PWA), native iOS and
  Android apps, and a desktop sync client.

![Creating a strict zero-knowledge folder — the dialog spells out the trade-off](docs/screenshots/06-create-folder-strict-zk.png)

## How it fits together

- **One server binary** (`cmd/server`) serves the HTTP API, the WebSocket
  collaboration relay, and the static web app (from `STATIC_DIR`) in a single
  process; the router is [chi](https://github.com/go-chi/chi)
  (`cmd/server/main.go:1406`).
- **A worker binary** (`cmd/worker`) drains the background job queue —
  malware scanning, preview and thumbnail generation, and search indexing.
- **PostgreSQL** holds all metadata; an **S3-compatible object-storage
  gateway** holds the file bytes. Uploads use presigned URLs the server mints
  (`POST /api/files/upload-url`) and confirms afterward
  (`POST /api/files/confirm-upload`), so bytes flow straight from the client
  to object storage.
- **NATS JetStream** carries the post-upload jobs.
- **The audit log is HMAC hash-chained** — every entry carries a sequence
  number, the previous entry's hash, and an HMAC over the row, so the log is
  independently verifiable (`internal/audit/audit.go:98-107`).

For the full system design, data model, and API surface, see
[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

## What's in this repo

```
zk-drive/
  cmd/          Binaries: server, worker, migrate, plus maintenance tools
                (compact, healthcheck, reconciler, orphan-gc,
                audit-archiver, audit-restore)
  api/          HTTP handlers and middleware (auth, drive, admin, platform)
  internal/     Core logic: folders, files, documents, sharing, permissions,
                billing, audit, retention, search, previews, scanning, jobs,
                KChat integration, storage, configuration
  frontend/     React + TypeScript web app (installable PWA)
  mobile/       Native iOS (Swift), native Android (Kotlin), shared Rust bridge
  desktop/      Tauri desktop sync client
  sdk/          Rust client SDK crates (api-client, auth, crypto, sync-engine, cli)
  migrations/   PostgreSQL schema
  scripts/      Operational and demo tooling, including the demo seed
  deploy/       Docker Compose and Kubernetes/Helm deployment templates
  docs/         Product, brand, architecture, configuration, operations, blog
  tests/        Integration tests
```

## Quickstart

zk-drive needs PostgreSQL, an S3-compatible object-storage gateway, and (for
the background jobs) NATS. The reference setups, exact environment variables,
and the one-command demo seed live in:

- [`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md) — run the stack locally, the
  test suites, frontend build, and Playwright e2e.
- [`docs/OPERATIONS.md`](docs/OPERATIONS.md) — deployment, observability,
  audit-log archival, webhook delivery, and account-recovery flows.
- [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md) — every environment
  variable for every binary.
- [`deploy/README.md`](deploy/README.md) — Docker Compose and Kubernetes/Helm
  paths.

To populate a running stack with the canonical demo workspace (Northwind
Trading and the isolation tenant Lakeside Legal), see the seed walkthrough in
[`docs/FACTS_AND_VOICE.md`](docs/FACTS_AND_VOICE.md) §7.

## Documentation

- [`docs/PRODUCT.md`](docs/PRODUCT.md) — the definitive capability and
  positioning reference: privacy modes, collaboration, sharing, security,
  administration, and billing tiers.
- [`docs/BRAND.md`](docs/BRAND.md) — the visual design language, product
  naming, and the writing and terminology rules.
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) and
  [`docs/IAM_CORE.md`](docs/IAM_CORE.md) — system architecture and identity.
- [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md),
  [`docs/OPERATIONS.md`](docs/OPERATIONS.md),
  [`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md) — configure, operate, develop.
- [`docs/MOBILE_IOS.md`](docs/MOBILE_IOS.md),
  [`docs/ANDROID_APP.md`](docs/ANDROID_APP.md),
  [`docs/MOBILE_BRIDGE.md`](docs/MOBILE_BRIDGE.md),
  [`desktop/README.md`](desktop/README.md),
  [`sdk/README.md`](sdk/README.md) — the clients and SDK.
- [`docs/blog/README.md`](docs/blog/README.md) — walkthroughs and evidence,
  grounded in the demo workspace.
- [`docs/FACTS_AND_VOICE.md`](docs/FACTS_AND_VOICE.md) — the code-verified
  fact sheet every document above is built on.

## License

Proprietary — all rights reserved. See [`LICENSE`](LICENSE).
