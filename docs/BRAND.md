# Brand alignment

ZK Drive is **not** zero-knowledge by default. The product offers
zero-knowledge as an **opt-in per-folder capability**. This document
codifies the customer-facing phrasing rules so every marketing page,
in-product label, docs page, support reply, and sales conversation
treats the brand the same way.

If you are editing UI copy, marketing copy, sales-enablement decks, or
any other customer-facing surface, this is the source of truth. If
something here conflicts with `docs/PRODUCT.md`, that is a bug — open a
PR and update both files in the same change.

## Tagline

> **ZK Drive — Zero-Knowledge When You Need It, Seamless When You Don't.**

Sub-tagline:

> Every workspace ships with confidential managed storage — encrypted
> at rest, with previews, full-text search, and virus scanning. Turn on
> strict zero-knowledge per folder when you need the server out of the
> loop: your keys, your control, no server access.

## What "ZK" means in "ZK Drive"

"ZK" in the product name refers to the platform's **capability** to
host strict zero-knowledge folders alongside server-readable ones. It
is *not* a claim that every byte in every folder is zero-knowledge.

This positioning is honest about what most customers actually need:
the default mode (confidential managed) enables previews, search,
virus scanning, and admin recovery, which is the right trade-off for
99% of business workflows. The strict zero-knowledge mode exists for
the 1% of content — legal hold, HR investigations, M&A diligence,
customer credentials, source code under NDA — where losing those
features is the right price for keeping the server out of the loop.

We are not a one-size-fits-all "everything is encrypted" product. We
are a platform that lets each folder pick the right trade-off.

## Mode names — canonical and forbidden

| Use this (customer-facing)                 | …for this (internal / fabric-level) | Never use                                         |
| ------------------------------------------ | ------------------------------------ | ------------------------------------------------- |
| **Confidential** (badge label, pricing)    | `ManagedEncrypted` mode in zk-object-fabric | "managed" alone, "encrypted-at-rest" as a claim about privacy, "private" |
| **Confidential managed** (full label)      | `ManagedEncrypted` mode             | "zero-knowledge", "ZK", "end-to-end"              |
| **Zero-knowledge** (badge label, pricing)  | `StrictZK` mode in zk-object-fabric  | "private mode", "secret mode"                     |
| **Strict zero-knowledge** (full label)     | `StrictZK` mode                      | "fully encrypted" (ambiguous — both modes are encrypted) |

The mode the customer chooses lives in `Folder.encryption_mode` in the
API; the badge component (`EncryptionBadge.tsx`) and the folder-creation
dialog (`FileBrowserPage.tsx`) are the single rendering points. Adding
a third customer-facing mode requires updating this file, the badge,
the dialog, the PrivacyPage explainer, and the `EncryptionMode` union
in `api/client.ts` in the same change.

## Phrasing rules

### MUST

- Always state which mode you are describing. "ZK Drive folders" is
  ambiguous; "confidential folders" or "zero-knowledge folders" are
  clear.
- Always pair the strict zero-knowledge claim with the disabled
  features (no previews, no full-text search, no virus scan, no admin
  recovery). The trade-off is part of the product, not a footnote.
- Always link customer-facing privacy claims to the in-product
  PrivacyPage (`/drive/privacy`) so the user can verify them at the
  point of decision.

### NEVER

- Never write "ZK Drive is zero-knowledge" unqualified. ZK Drive
  **supports** zero-knowledge as a per-folder mode.
- Never call `ManagedEncrypted` zero-knowledge. The gateway can read
  plaintext in memory during a request — that is the whole point of
  the mode.
- Never describe confidential managed as "you hold the keys". The
  workspace holds the keys (gateway-side). "You hold the keys" is
  reserved for strict zero-knowledge.
- Never use "ZK" as a synonym for "secure". Confidential is also
  secure (encrypted at rest, mTLS in transit, audit-logged, access-
  controlled). "ZK" specifically means *the server cannot decrypt*.

## Customer-facing FAQ

### "Is ZK Drive zero-knowledge by default?"

No. By default, folders are **confidential managed** — encrypted at
rest, but the server can read plaintext in memory during request
handling. This is what powers previews, full-text search, and virus
scanning, and it is the right default for most teams.

If you need true zero-knowledge for a specific folder, mark it as
**strict zero-knowledge** when you create it. The server will never be
able to decrypt that folder, and the features that depend on the
server reading content (previews, full-text search, virus scanning,
admin recovery) will be disabled for it.

### "Why isn't every folder zero-knowledge?"

Because most teams want previews and search to work. Strict zero-
knowledge means the server never sees plaintext, which means it can't
generate a PDF thumbnail, extract text for full-text search, scan an
attachment for malware, or help an admin recover access if a user
loses their credentials.

For the rare content where you do want all of that disabled (legal
hold, HR investigations, NDA-bound source code, customer credentials),
strict zero-knowledge is the right choice — and the folder-creation
dialog tells you exactly what you are trading off before you commit.

### "Can I switch a folder from confidential to zero-knowledge?"

Yes — an empty folder can be re-created in the other mode. Once a
folder has content, the choice is sticky: confidential → strict
zero-knowledge would re-encrypt every existing version, and strict
zero-knowledge → confidential is impossible because the server never
had the plaintext.

### "Is the platform name dishonest then?"

We do not think so, and we want to be transparent about why. "ZK
Drive" describes what the platform *can* do — host strict
zero-knowledge folders — alongside what it does by default. Calling it
"Drive" alone would understate the privacy story; calling it
"Zero-Knowledge Drive" without the per-folder opt-in framing would
overstate it. "ZK Drive" with this brand-alignment document is the
honest middle.

If the framing reads otherwise to you, that's a real bug — please open
an issue on the repo or tell your support contact and we will fix the
copy.

## Cross-references

- `docs/PRODUCT.md` §3.3 — Encryption modes (trade-off matrix)
- `frontend/src/pages/PrivacyPage.tsx` — customer-facing explainer
- `frontend/src/components/EncryptionBadge.tsx` — badge component
- `frontend/src/pages/FileBrowserPage.tsx` — folder-creation dialog
- `README.md` — top-level positioning
