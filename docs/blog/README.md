# ZK Drive in Practice — A Business-Facing Blog Series

This series shows how ZK Drive actually works for a real small-to-medium
enterprise, told through the jobs each persona is trying to get done. Every
screenshot and number in these posts comes from a **live ZK Drive deployment
running real data** — not mockups, not a marketing render. We uploaded real
files, invited real members, granted real permissions, minted real share links,
edited a real collaborative document, and read back a real, cryptographically
verifiable audit log.

The series also includes an **honest competitive assessment** (post 7): where
ZK Drive genuinely wins against Google Drive, Dropbox, Box, Tresorit, and Proton
Drive — and where it does not. We would rather you trust the gaps than oversell
the wins.

## The demo workspace

Everything below is anchored to two seeded SME tenants:

- **Northwind Trading** — a 5-person company (1 owner-admin, 1 admin, 2 active
  members, and 1 deactivated account) with folders mixing everyday
  `managed_encrypted` storage (`Engineering`, with `Architecture` and
  `Releases` nested inside it; `Finance`; `Marketing Assets`) and opt-in
  `strict_zk` vaults (`Legal Contracts`, `Client Vault`). It holds 13 real
  uploaded files (PDF, DOCX, XLSX, CSV, PNG, Markdown, YAML), internal
  permission grants, a password- and expiry-protected share link with a download
  cap, a folder share link, a guest invitation into the vault, a live
  collaborative document, a **Brightwave Partners** client room, a
  `northwind-engineering` KChat room, and retention policies — on the business
  plan.
- **Lakeside Legal** — a second, isolated tenant (its own owner and folders,
  including a `strict_zk` matters vault) that demonstrates tenant separation.

The stack under the screenshots: a Go API and a React single-page app served
same-origin, PostgreSQL for metadata, NATS JetStream for the async pipelines
(preview, full-text index, virus scan, classify, archive), and S3-compatible
object storage for the file bytes.

## The posts

| # | Post | Persona | Job to be done |
|---|------|---------|----------------|
| 1 | [Standing up a workspace](01-onboarding-and-admin.md) | Owner-admin / IT | Get a team onto secure, governed storage in minutes |
| 2 | [A day in the files](02-knowledge-worker.md) | Knowledge worker | Find, open, organize, and keep history of work |
| 3 | [Working with clients & partners](03-external-collaboration.md) | Account / project lead | Share outside the company without losing control |
| 4 | [Privacy you can actually explain](04-privacy-and-zero-knowledge.md) | Everyone | Choose the right protection per folder, honestly |
| 5 | [Compliance & security evidence](05-compliance-and-security.md) | Security / compliance officer | Prove who did what, and contain blast radius |
| 6 | [Operations without an ops team](06-operations-noops.md) | Admin / IT | Know the system is healthy at a glance |
| 7 | [Honest assessment vs the competition](07-honest-competitive-assessment.md) | Buyer / decision maker | Decide if ZK Drive fits |
| 8 | [Editing together, live](08-collaborative-editing.md) | Any team that drafts together | Co-author documents in real time, in the drive |
| 9 | [ZK Drive as KChat's storage backbone](09-kchat-integration.md) | Admin / IT | Connect team chat to governed storage |
| 10 | [Files on the desktop](10-desktop-sync.md) | Anyone working from a laptop | Keep a local folder in sync with the workspace |

## How to read the evidence

Screenshots live in [`img/`](img/), each with a light variant and a `-dark`
companion. Where a claim depends on data the UI does not surface — the audit
hash-chain verification — the posts include the **real API response** inline (in
[Compliance & security evidence](05-compliance-and-security.md)) so you can see
it was not hand-waved.

A note on honesty: ZK Drive draws a clear line between what each privacy mode can
and cannot do, and the posts keep that line in view rather than hiding it.
Optional integrations — the OnlyOffice Document Server for office files,
ClamAV-backed malware scanning — are connected per deployment, and the posts say
plainly where that shows up in the UI instead of pretending otherwise.
