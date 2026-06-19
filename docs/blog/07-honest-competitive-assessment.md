# 7. Honest assessment vs the competition

**Persona:** The buyer or decision-maker weighing ZK Drive against the drives
they already use
**Job to be done:** *"Tell me where this genuinely beats what I have — and,
more importantly, where it doesn't — so I can make a real decision."*

---

Most vendor comparison pages are a column of green checkmarks for "us" and red
crosses for "them." This is not that. ZK Drive occupies a deliberately narrow
position, and the honest answer to "should you buy it?" is "it depends on what
you need." Here is the real picture.

## Where ZK Drive sits

ZK Drive targets a specific gap: **governed file collaboration with privacy,
data residency, and predictable cost** for SMEs, agencies, and
professional-services firms — between consumer-grade bundled suites and
heavyweight enterprise file platforms (`docs/PRODUCT.md`).

| Segment | Examples | Their strength | The gap ZK Drive targets |
|---|---|---|---|
| Bundled productivity suites | Google Drive, OneDrive, iCloud | Ubiquitous, deep office-suite integration | The provider can read your files; minimal residency control |
| Enterprise file management | Box, SharePoint, Egnyte | Strong governance, DLP, compliance | Expensive (~$15–47/user/mo), complex, over-featured for SMEs |
| Encrypted storage | Tresorit, Proton Drive, SpiderOak | True client-side encryption | Weak collaboration; no client rooms; limited admin tooling |
| Low-cost storage | Dropbox, pCloud, MEGA, Nextcloud | Cheap, wide device support | Not privacy-first; self-host needs ops capacity SMEs lack |

## Where ZK Drive genuinely wins

Each of these is backed by evidence elsewhere in this series:

1. **Honesty about encryption.** ZK Drive ships an in-product page that says,
   in its own words, that the default is *not* zero-knowledge — and lets you
   choose **per folder**, stating the exact cost of each choice
   ([Privacy you can actually explain](04-privacy-and-zero-knowledge.md)).
   Pure end-to-end products market one global mode; ZK Drive tells you
   precisely what `managed_encrypted` and `strict_zk` each give up.
2. **Mixed-mode workspaces.** Searchable, scannable confidential folders and
   end-to-end-encrypted vaults live side by side in one workspace. A
   `strict_zk` folder disables server-side preview, search, and malware
   scanning — and an everyday `managed_encrypted` folder keeps them — so you
   don't trade search away across the board to protect the crown jewels.
3. **Verifiable governance, priced for an SME.** A cryptographically
   verifiable, HMAC hash-chained audit log (`valid: true` from a self-service
   endpoint), policy-driven retention, role-based access, and per-workspace IP
   allow rules ([Compliance & security evidence](05-compliance-and-security.md))
   — Box-style governance without Box-style price or complexity.
4. **Real external-collaboration primitives.** Password-protected, expiring,
   download-capped share links; folder-scoped guest invites; and client rooms
   from templates ([Working with clients & partners](03-external-collaboration.md))
   — exactly what the encrypted-storage players are weak at and what consumer
   drives lack.
5. **A fit for teams without ops.** A one-screen health board, self-draining
   job pipelines, per-workspace preview budgets, and honest "not configured"
   callouts ([Operations without an ops team](06-operations-noops.md)) mean an
   SME admin runs the platform without a dedicated ops team.
6. **KChat integration.** Every KChat room maps to a backing folder, so chat
   attachments live in the same governed, retained, audited store as everything
   else (`docs/PRODUCT.md`) — and the bundle is what makes the economics work.

### Pricing, benchmarked

From the published pricing benchmarks (`docs/PRODUCT.md`):

| Product | Price (USD / user / mo) |
|---|---|
| Google Workspace | $7 – $22 |
| Dropbox Business | $15 – $24 |
| Box Business | $15 – $47 |
| Tresorit Business | €12 – €16 |
| Proton Drive | €4 – €10 |
| **ZK Drive (Business)** | **$4 – $6** |
| **ZK Drive + KChat bundle** | **$3 – $4** |

Storage and bandwidth are always priced separately, and there is no
"unlimited" tier — the fair-use economics of the underlying object storage make
"unlimited" a dishonest promise. Quota is **pooled across the workspace** rather
than capped per seat (`docs/PRODUCT.md`).

## Where ZK Drive does *not* win — the real gaps

This is the part most comparison pages omit. We will not.

1. **No office-suite / co-authoring parity.** ZK Drive is explicitly not a
   Google Workspace replacement. It ships real-time collaborative editing (a
   Yjs CRDT engine) and an OnlyOffice integration path, but a team that lives
   in Docs/Sheets with simultaneous editing is better served by Google or
   Microsoft. In the demo deployment, OnlyOffice even shows as *"Not
   configured"* ([Operations without an ops team](06-operations-noops.md)).
2. **Ecosystem & integration breadth.** Google and Microsoft each carry
   thousands of third-party integrations and deep OS hooks. ZK Drive offers SSO
   (Google / Microsoft / OIDC), outbound webhooks, and the KChat integration —
   useful, but a fraction of the incumbents' marketplace depth.
3. **Desktop & mobile sync breadth.** ZK Drive ships a cross-platform desktop
   sync client built on the same change feed the server exposes
   ([Desktop sync](10-desktop-sync.md)). Dropbox's sync — deep offline
   handling, LAN sync, smart-sync placeholders — covers more ground. If
   best-in-class sync is your first requirement, Dropbox leads.
4. **Compliance certifications & DLP depth.** Box and SharePoint hold broad
   certification portfolios (SOC 2, HIPAA, FedRAMP, and more) and deep
   DLP/eDiscovery. ZK Drive gives you the *primitives* — a verifiable audit
   chain, retention, residency controls — not the full certification checklist
   a large regulated-enterprise procurement expects.
5. **Brand & market mindshare.** "Nobody got fired for buying Microsoft." ZK
   Drive is a challenger, and for risk-averse buyers that is a real adoption
   cost.
6. **Some marquee controls depend on the storage control plane.**
   Customer-managed-key binding and data-residency placement enforcement run
   through the storage control plane. In the minimal demo those screens read
   *"not yet configured"* and *"not supported"*
   ([Compliance & security evidence](05-compliance-and-security.md)). The
   product surface is there; realising it requires the storage backend to be
   provisioned.

## Who should — and shouldn't — choose ZK Drive

**Strong fit**

- Agencies, consultancies, and professional-services firms that need governed
  external collaboration (client rooms, expiring links, guest dropboxes).
- Privacy- or residency-sensitive SMEs (legal, healthcare-adjacent) that want
  per-folder zero-knowledge without losing search on everyday files.
- Teams that want Box-style governance at a Dropbox-style price, especially
  bundled with KChat.

**Poor fit**

- Teams whose core workflow is real-time co-authored documents → Google or
  Microsoft.
- Large enterprises with a hard certification checklist (FedRAMP and the like)
  → Box or SharePoint, whose compliance portfolios are broader.
- Buyers who want the single most battle-tested desktop/mobile sync on the
  market → Dropbox.

---

### The honest bottom line

ZK Drive is a focused, privacy-first **file-collaboration** product that beats
the incumbents on honesty, per-folder zero-knowledge, external-collaboration
primitives, governance-for-the-price, and economics — and trails them on office
suites, ecosystem breadth, sync depth, and compliance certifications. If your
job to be done is *"share sensitive files with clients and partners, under
control, affordably,"* it is an excellent fit. If it is *"replace Google
Workspace,"* it is not trying to be — and says so.

← Back to the [series index](README.md)
