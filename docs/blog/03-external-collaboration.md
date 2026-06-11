# 3. Working with clients & partners

**Persona:** Account / project lead (agency, consultancy, or professional-
services firm)
**Job to be done:** *"Get files to someone outside my company — and receive
files from them — without emailing attachments, and without losing control of
who can see what, for how long."*

---

External collaboration is where consumer drives get scary and enterprise tools
get heavy. ZK Drive's whole reason to exist for agencies and consultancies is to
make *governed* outside sharing feel as light as a Dropbox link — but with the
controls a professional-services firm actually needs.

## A share link with guardrails

From any folder or file, **Share → Share link** mints a link with optional
guardrails built in: a viewer/editor role, an optional **password**, an optional
**expiry**, and an optional **maximum download count**.

![Share-link dialog with role, password, expiry, max downloads](img/30-share-dialog.png)

This is the difference between "anyone with the link, forever" and a link that
expires next Friday, needs a password, and stops working after 25 downloads. In
the seeded workspace we created exactly this kind of link on the
`Marketing Assets` folder (password-protected) and a separate file link with an
expiry and a download cap.

## Inviting a named guest

Sometimes a link is too blunt and you want a *named* external collaborator.
The **Invite by email** tab creates a guest invitation scoped to a specific
resource, with a role and an optional expiry. Below, Northwind invites
`client@acme.example` to a file in the **Client Vault** — and note the badge:
Client Vault is a **zero-knowledge** folder, so even an externally-shared file
here is end-to-end encrypted.

![Guest invite by email on a zero-knowledge folder](img/31-guest-invite.png)

The guest invitation is recorded in the audit log like everything else:

```
sharing.guest_invite_emailed | outcome=disabled
```

> **Honest caveat — email delivery.** The audit entry says
> `outcome=disabled` because this demo has no SMTP/email provider wired up, so
> the invitation record was created but no email was actually sent. The invite
> object, its scope, role, and expiry are all real; the delivery channel simply
> is not configured in a local demo.

## Client rooms

For recurring engagements, ZK Drive offers **client rooms** — a named, shared
space (optionally with a guest "dropbox" so the client can upload to you). The
seed created an *"Acme Corp — Q3 Deliverables"* room backed by a real folder and
share link. This is the primitive that consumer drives lack entirely and that
makes ZK Drive a fit for agencies running many parallel client relationships.

---

### What this journey demonstrates

- **Links with real guardrails:** password, expiry, and download caps are
  first-class, not enterprise add-ons.
- **Named guests, scoped tightly:** invite a specific person to a specific
  resource, with an expiry — and it works even on zero-knowledge folders.
- **Client rooms & dropboxes:** purpose-built for outside collaboration, which
  is exactly where bundled consumer suites fall short.
- **Everything is audited:** every external grant lands in the tamper-evident
  log (see [post 5](05-compliance-and-security.md)).

Next: [Privacy you can actually explain →](04-privacy-and-zero-knowledge.md)
