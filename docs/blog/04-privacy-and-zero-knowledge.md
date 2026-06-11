# 4. Privacy you can actually explain

**Persona:** Everyone — but especially the person who has to justify the tool to
a client or a regulator
**Job to be done:** *"Pick the right level of protection for each folder, and be
able to explain — honestly — what the vendor can and cannot see."*

---

This is the post that matters most, because it is where ZK Drive refuses to do
the thing every competitor does: market everything as "zero-knowledge" when it
is not.

## Two modes, one honest choice

ZK Drive ships a built-in explainer page — **"How your data is protected"** —
that lays out both protection modes and their trade-offs in tables, in plain
language. The opening line sets the tone:

> *"We try to be honest about the trade-offs rather than market both modes as
> 'zero-knowledge' — most providers do that and it is not accurate for either
> one."*

![The in-product privacy explainer page](img/14-privacy-page.png)

### Confidential managed (the default)

Files are encrypted at rest, but the server **can** read plaintext in memory
while handling a request. That is not a weakness to hide — it is precisely what
enables previews, full-text search, virus scanning, and admin recovery. For most
SME content (project files, deliverables, anything you want to find by typing),
this is the right trade-off. ZK Drive says so plainly, and **never calls it
zero-knowledge**.

### Strict zero-knowledge (opt-in, per folder)

Files are encrypted on the client with keys the server never sees. The gateway
only ever stores opaque ciphertext. Because the server cannot decrypt the
contents, previews, full-text search, virus scanning, and admin password-reset
recovery are all **disabled** — "the honest cost of the guarantee."

## The choice is made where it matters — at folder creation

The same trade-off table appears *inline* when you create a folder, so nobody
chooses zero-knowledge by accident or misunderstands what they are giving up:

![Folder-creation trade-off table and irreversibility warning](img/13-create-folder-strict-zk.png)

Note the amber warning: **strict-ZK is irreversible once the folder has
content**, because the server never had the plaintext to migrate back. ZK Drive
tells you this *before* you commit, not in a support ticket afterward.

## You can mix both in one workspace

This is the practical superpower: the choice is **per folder, not per
workspace**. Northwind keeps `Engineering`, `Finance`, and `Marketing` as
confidential (so they're searchable and previewable) while `Legal Contracts`
and `Client Vault` are zero-knowledge. Here is a zero-knowledge folder — the
file is present with its metadata, but there is no thumbnail and no full-text
indexing of its contents, exactly as promised:

![A zero-knowledge folder: metadata yes, preview/search no](img/16-strict-zk-folder.png)

## Bring your own key

For regulated customers, the **Encryption (CMK)** screen exposes customer-managed
keys and a workspace-default mode for new folders. The accepted key URI schemes
(`arn:aws:kms:`, `kms://`, `vault://`, `transit://`) are real, and the page
again restates the managed-vs-zero-knowledge distinction so admins set defaults
with eyes open.

![Customer-managed key configuration and default mode](img/27-admin-encryption.png)

> **Honest caveat.** This screen shows *"Workspace storage is not yet
> configured"* because the zk-object-fabric storage control plane is not
> provisioned in this local demo. The CMK field, schemes, and default-mode
> selector are real product; binding a key requires the storage backend, which
> we did not stand up here.

---

### What this journey demonstrates

- **Honesty as a feature:** ZK Drive names the default *"confidential
  managed — not zero-knowledge"* instead of overclaiming, in the product itself.
- **Informed, per-folder choice:** the trade-offs are shown at the decision
  point, including irreversibility.
- **Mixed-mode workspaces:** searchable everyday folders and end-to-end
  encrypted vaults coexist — you are not forced into all-or-nothing.
- **BYOK path** for customers who need to hold their own keys.

Next: [Compliance & security evidence →](05-compliance-and-security.md)
