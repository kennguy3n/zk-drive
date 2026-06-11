# ZK Drive in Practice — A Business-Facing Blog Series

This series shows how ZK Drive actually works for a real small-to-medium
enterprise, told through the jobs each persona is trying to get done. Every
screenshot and number in these posts was captured from a **live ZK Drive
deployment running real data** — not mockups, not a marketing render. We
uploaded real files, invited real members, granted real permissions, created
real share links, and read back a real, cryptographically verifiable audit log.

We also include an **honest competitive assessment** (post 7): where ZK Drive
genuinely wins against Google Drive, Dropbox, Box, Tresorit, and Proton Drive —
and where it does not. We would rather you trust the gaps than oversell the
wins.

## The demo workspace

Everything below is anchored to two seeded SME tenants:

- **Northwind Trading** — a 5-person company (1 owner/admin, 1 second admin,
  3 members) with 8 folders mixing everyday *confidential* storage and opt-in
  *zero-knowledge* vaults, 14 real uploaded documents (PDF, PNG, YAML, CSV,
  Markdown, text), internal permission grants, a password-protected share link,
  a guest invitation, a client room, a retention policy, and an IP allowlist
  rule.
- **Lakeside Legal** — a second isolated tenant, demonstrating tenant
  separation.

The stack under the screenshots: a Go API and a React single-page app served
same-origin, Postgres 16 for metadata, NATS JetStream for the async pipelines
(preview, full-text index, virus scan, classify, archive), and an
S3-compatible object store with **per-tenant buckets**.

## The posts

| # | Post | Persona | Job to be done |
|---|------|---------|----------------|
| 1 | [Standing up a workspace](01-onboarding-and-admin.md) | Admin / IT | Get a team onto secure storage in minutes |
| 2 | [A day in the files](02-knowledge-worker.md) | Knowledge worker | Find, open, version, and organize work |
| 3 | [Working with clients & partners](03-external-collaboration.md) | Account / project lead | Share outside the company without losing control |
| 4 | [Privacy you can actually explain](04-privacy-and-zero-knowledge.md) | Everyone | Choose the right protection per folder |
| 5 | [Compliance & security evidence](05-compliance-and-security.md) | Security / compliance officer | Prove who did what, and contain blast radius |
| 6 | [Operations without an ops team](06-operations-noops.md) | Admin / IT | Know the system is healthy at a glance |
| 7 | [Honest assessment vs the competition](07-honest-competitive-assessment.md) | Buyer / decision maker | Decide if ZK Drive fits |

## How to read the evidence

Screenshots live in [`img/`](img/). Where a claim depends on data the UI does
not surface (the audit hash-chain verification), we include the **real API
response** inline so you can see it was not hand-waved.

A note on honesty: this demo runs a deliberately minimal stack. Optional
subsystems (ClamAV, Redis, the OnlyOffice editor, and the zk-object-fabric
storage *control plane*) are not provisioned here, and the posts call out
exactly where that shows up in the UI rather than hiding it.
