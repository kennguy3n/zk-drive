# 1. Standing up a workspace

**Persona:** Owner-admin at an SME (Alice Chen, Northwind Trading)
**Job to be done:** *"Get my team onto secure, governed file storage in minutes
— without standing up infrastructure or reading a manual."*

---

Alice Chen runs operations at Northwind Trading, a small import and distribution
business. There is no IT department to file a ticket with, and no appetite for a
week-long rollout. Alice needs to create a workspace, invite four colleagues,
lay out a sensible folder structure, and be confident the sensitive folders are
genuinely protected — all before lunch. ZK Drive is built for exactly this: an
SME admin with no training and no dedicated ops team.

## Creating the workspace

Signup is a single self-serve screen. Alice names the workspace, enters her
name, email, and a password, and presses **Create workspace** — no tenant
picker, no "contact sales", no enterprise SSO wizard standing between a small
team and its first folder.

![Self-serve workspace creation](img/01-signup.png)

That makes Alice the workspace's **owner-admin**. From here on, the sign-in
screen is just as plain: an email, a password, and a link to create a workspace
for anyone who lands there without one.

## Inviting the team

From **Admin → Users**, Alice invites each colleague and assigns a role on the
invite itself. The roster below is the real seeded Northwind team:

| Name | Email | Role | Status |
| --- | --- | --- | --- |
| Alice Chen | `alice@northwind.example` | Owner-admin | Active |
| Dave Patel | `dave@northwind.example` | Admin | Active |
| Bob Martinez | `bob@northwind.example` | Member | Active |
| Carol Nguyen | `carol@northwind.example` | Member | Active |
| Eve Thompson | `eve@northwind.example` | Member | Deactivated |

![Admin user management with the real roster, roles, and statuses](img/20-admin-users.png)

Each invite creates an account with a temporary password; the member sets their
own on first sign-in. Roles are editable inline, and any account can be
deactivated in one click — Eve Thompson, who left, shows as **Deactivated**
rather than deleted, so her history stays intact. These are the controls an SME
admin actually reaches for, without the sprawl of an enterprise IAM console.

Every one of these actions is written to the audit log — invites land as
`admin.user_invite` and deactivations as `admin.user_deactivate`
(`internal/audit/audit.go:42-43`) — in a tamper-evident, hash-chained record we
walk through in [Compliance & security evidence](05-compliance-and-security.md).

## Laying out folders — with protection as a first-class choice

When Alice creates a folder, ZK Drive asks the one question most file products
bury in settings: **how should this folder be protected?** The choice is made
up front, in plain language, with the trade-offs shown side by side — the
default, `managed_encrypted` ("Confidential managed"), is right for most
folders, and the strong option, `strict_zk` ("Strict zero-knowledge"), is one
radio button away with its costs spelled out honestly. Alice does not need to
understand key management to make a safe choice. That decision gets its own
[dedicated post](04-privacy-and-zero-knowledge.md).

A few minutes later, Northwind's workspace looks like this — an ordinary-feeling
drive where every folder wears its protection level as a coloured badge:

![Drive root with per-folder privacy badges](img/10-drive-root.png)

`Engineering` (with `Architecture` and `Releases` nested inside it), `Finance`,
and `Marketing Assets` are everyday `managed_encrypted` storage and carry the
green **confidential** badge. `Legal Contracts` and `Client Vault` carry the
**zero-knowledge** badge — they are `strict_zk`, so the server only ever holds
ciphertext for them. Two more folders appear here that Alice did not create by
hand: a **Brightwave Partners** client room (see
[Working with clients & partners](03-external-collaboration.md)) and a
**KChat: northwind-engineering** folder that backs a chat room (see
[ZK Drive as KChat's storage backbone](09-kchat-integration.md)). The badge is
not decoration — it is the same label the product uses everywhere a folder is
shown, so nobody is ever guessing what the server can read.

## One console for everything else

The admin surface keeps the rest of governance one click away. Alongside
**Users**, the console carries **Audit**, **Retention**, **Storage**, and
**Health** tabs, plus **Placement**, **Encryption**, **KChat rooms**, and
**Billing**. Northwind runs on the **business** plan with explicit limits — 1 TB
of storage, 50 users, and 5 TB of bandwidth a month — and quotas are enforced,
not cosmetic: exceeding one returns a `402 Payment Required`
(`internal/billing/billing.go:44-48`). Retention is set the same way, with a
workspace default of 10 kept versions and a stricter 25 on `Legal Contracts`.

---

### What this journey demonstrates

- **Time-to-value in minutes:** self-serve signup → invite → folders → done,
  with no infrastructure to provision.
- **Governance from the first folder:** protection level is a deliberate,
  explained choice at creation time, not an afterthought.
- **Role-appropriate from day one:** owner-admin, admin, and member are real
  distinctions, and departed accounts are deactivated, not erased.
- **Everything is recorded:** invites, role changes, and deactivations all land
  in the tamper-evident audit log.

Next: [A day in the files →](02-knowledge-worker.md)
