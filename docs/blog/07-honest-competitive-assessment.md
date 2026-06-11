# 7. Honest assessment vs the competition

**Persona:** Buyer / decision maker
**Job to be done:** *"Tell me where this genuinely beats what I already use —
and, more importantly, where it doesn't — so I can make a real decision."*

---

Most vendor comparison pages are a column of green checkmarks for "us" and red
crosses for "them." This is not that. ZK Drive occupies a deliberately narrow
position, and the honest answer to "should you buy it?" is "it depends on what
you need." Here is the real picture.

## Where ZK Drive sits

ZK Drive targets a specific gap: **governed file collaboration with privacy,
data residency, and predictable cost** for SMEs, agencies, and professional-
services firms — between consumer-grade bundled suites and heavyweight
enterprise file platforms.

| Segment | Examples | Their strength | The gap ZK Drive targets |
|---|---|---|---|
| Bundled productivity suites | Google Drive, OneDrive, iCloud | Ubiquitous, deep office-suite integration | Provider can read your files; minimal residency control |
| Enterprise file management | Box, SharePoint, Egnyte | Strong governance, DLP, compliance | Expensive (~$15–47/user/mo), complex, over-featured for SMEs |
| Encrypted storage | Tresorit, Proton Drive, SpiderOak | True client-side encryption | Weak collaboration; no client rooms; limited admin tooling |
| Low-cost storage | Dropbox, pCloud, MEGA, Nextcloud | Cheap, wide device support | Not privacy-first; self-host needs ops capacity SMEs lack |

## Where ZK Drive genuinely wins

These are backed by the evidence in this series:

1. **Honesty about encryption.** ZK Drive ships an in-product page that
   explicitly says its default is *"not zero-knowledge, and we never call it
   that"* ([post 4](04-privacy-and-zero-knowledge.md)). Tresorit and Proton
   market end-to-end encryption broadly; ZK Drive instead lets you choose
   per-folder and tells you the exact cost of each choice.
2. **Mixed-mode workspaces.** Searchable confidential folders and end-to-end
   encrypted vaults live side by side in one workspace
   ([post 4](04-privacy-and-zero-knowledge.md)). Pure zero-knowledge products
   make you trade away search/preview globally.
3. **Real external-collaboration primitives.** Password/expiry/download-capped
   links, scoped guest invites, and client rooms
   ([post 3](03-external-collaboration.md)) — exactly what Tresorit/Proton are
   weak at and what consumer drives lack.
4. **Verifiable governance for SMEs.** A cryptographically verifiable audit
   chain (`valid: true`, [post 5](05-compliance-and-security.md)), retention,
   and IP allowlisting — Box-style governance without Box-style price or
   complexity.
5. **Data residency + BYOK** ([post 4](04-privacy-and-zero-knowledge.md),
   [post 6](06-operations-noops.md)) at SME price points.
6. **Honest, pooled economics.** Published cost curve and pooled storage
   instead of "unlimited" plans that hide egress fees
   ([post 1](01-onboarding-and-admin.md)).

### Indicative pricing (from internal benchmarks)

| Product | Price (USD/user/mo) |
|---|---|
| Google Workspace | $7 – $22 |
| Dropbox Business | $15 – $24 |
| Box Business | $15 – $47 |
| Tresorit Business | €12 – €16 |
| Proton Drive | €4 – €10 |
| **ZK Drive (Business)** | **$4 – $6** |
| **ZK Drive + KChat bundle** | **$3 – $4** |

## Where ZK Drive does *not* win — the real gaps

This is the part most comparison pages omit. We will not.

1. **No office suite / real-time co-editing parity.** ZK Drive is explicitly
   *not* a Google Workspace competitor. There is a collaborative-editing
   integration and an OnlyOffice path, but if your team lives in Docs/Sheets
   with simultaneous editing, Google and Microsoft are far ahead. (In this very
   demo, OnlyOffice showed *"Not configured"* — [post 6](06-operations-noops.md).)
2. **Ecosystem & integrations breadth.** Google and Microsoft have thousands of
   third-party integrations and deep OS hooks. ZK Drive offers SSO (Google/
   Microsoft), webhooks, and a KChat integration — useful, but a fraction of the
   marketplace depth of the incumbents.
3. **Mobile & desktop maturity.** Native iOS and Android apps and a desktop sync
   client exist, but they are young compared to Dropbox's decade-plus of sync
   reliability and offline polish. Dropbox still sets the bar for sync.
4. **Compliance certifications & DLP depth.** Box and SharePoint carry broad
   certification portfolios (SOC 2, HIPAA, FedRAMP, etc.) and mature DLP/
   eDiscovery. ZK Drive gives you the *primitives* (audit chain, retention,
   residency) but not (yet) the certification checklist a large regulated
   enterprise procurement will demand.
5. **Brand & market mindshare.** "Nobody got fired for buying Microsoft." ZK
   Drive is a challenger; that is a real adoption cost for risk-averse buyers.
6. **Operational completeness depends on the storage control plane.** Several
   marquee capabilities — CMK binding, placement/residency enforcement, storage
   health — depend on the zk-object-fabric control plane being provisioned. Our
   own demo showed those screens as *"not configured / not supported"*
   ([posts 4](04-privacy-and-zero-knowledge.md) &
   [6](06-operations-noops.md)). The product surface is there; the backend must
   be stood up to realize it.

## Who should (and shouldn't) buy ZK Drive

**Strong fit:**
- Agencies, consultancies, and professional-services firms that need governed
  external collaboration (client rooms, expiring links, guest dropboxes).
- Privacy- or residency-sensitive SMEs (legal, healthcare-adjacent) that want
  per-folder zero-knowledge without losing search on everyday files.
- Teams that want Box-style governance at a Dropbox-style price, especially
  bundled with KChat.

**Poor fit (today):**
- Teams whose core workflow is real-time co-authored documents → stay on
  Google/Microsoft.
- Large enterprises with a hard certification checklist (FedRAMP, etc.) → Box/
  SharePoint are safer until ZK Drive's compliance portfolio matures.
- Users who want the most battle-tested mobile/desktop sync on the market →
  Dropbox still leads.

---

### The honest bottom line

ZK Drive is a focused, privacy-first **file collaboration** product that beats
the incumbents on honesty, per-folder zero-knowledge, external-collaboration
primitives, governance-for-the-price, and economics — and trails them on office
suites, ecosystem breadth, mobile/desktop maturity, and compliance
certifications. If your job-to-be-done is *"share sensitive files with clients
and partners under control, affordably,"* it is an excellent fit. If it is
*"replace Google Workspace,"* it is not trying to be — and says so.

← Back to the [series index](README.md)
