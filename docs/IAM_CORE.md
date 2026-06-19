# ZK Drive — Identity & Access

**License**: Proprietary — All Rights Reserved.

This is the identity and access reference for ZK Drive: how users
authenticate, how two-factor and SSO work, how roles and sessions are
enforced, and how an external identity provider (IAM Core OIDC) can take
over authentication. It is written for engineers and security
evaluators, so claims are cited at `path:line`. For the surrounding
system design see [`ARCHITECTURE.md`](ARCHITECTURE.md); for the
exhaustive configuration-key table see
[`CONFIGURATION.md`](CONFIGURATION.md).

## 1. Two identity modes

ZK Drive runs in one of two authentication modes, selected entirely by
whether `IAM_CORE_ISSUER_URL` is set:

| Mode | Selected by | Who issues tokens | Where MFA lives |
| --- | --- | --- | --- |
| Built-in | `IAM_CORE_ISSUER_URL` empty (default) | ZK Drive — session JWT (HS256 or ES256) | ZK Drive TOTP pages |
| IAM Core OIDC | `IAM_CORE_ISSUER_URL` set | iam-core — verified against its JWKS | iam-core Universal Login |

Both modes converge on the **same request context**:
`AuthMiddleware` (built-in) and the iam-core middleware each bind the
identical `(workspaceID, userID, role)` into every request, so the tenant
guard, Postgres row-level security, and all downstream handlers behave
identically regardless of who authenticated the caller
(`cmd/server/main.go:1002`, `cmd/server/main.go:1036`).

The sign-in screen is the same product surface in either mode; in
built-in mode it renders the password form and any configured SSO
buttons.

![Sign-in page](screenshots/01-login-page.png)

## 2. Built-in authentication (session JWT)

In built-in mode ZK Drive issues its own session token. Two environment
variables govern signing:

- **`JWT_SECRET`** — required; the server refuses to boot without it
  (`internal/config/config.go:746-747`). It signs HS256 tokens and, when
  no dedicated `AUDIT_HMAC_KEY` is set, also seeds the audit-chain HMAC
  key (see [`ARCHITECTURE.md`](ARCHITECTURE.md) §7).
- **`JWT_ALGORITHM`** — selects the signing algorithm
  (`internal/config/config.go:61-73`):
  - `auto` — sign with ES256 when an active asymmetric signing key
    exists, otherwise fall back to HS256 using `JWT_SECRET`.
  - `ES256` — force asymmetric signing.
  - Verification **always accepts both**, so rotating from HS256 to ES256
    does not invalidate live sessions. The default is `ES256` under the
    production profile (asymmetric-only signing is mandatory there) and
    `auto` otherwise.

Asymmetric signing keys live in their own table
(`migrations/034_jwt_asymmetric_keys.up.sql`); each replica re-reads them
on an interval (`JWT_KEY_REFRESH_INTERVAL`) so a key rotation propagates
across the fleet without a restart.

The built-in auth routes are mounted under `/api/auth`
(`cmd/server/main.go:1600-1635`):

| Route | Purpose |
| --- | --- |
| `POST /api/auth/signup` | Create the first admin / accept an invite |
| `POST /api/auth/login` | Password sign-in (brute-force guarded, §8) |
| `POST /api/auth/refresh` | Exchange a refresh token for a new session JWT |
| `POST /api/auth/logout` | End the current session |
| `GET /api/auth/sessions` | List the caller's active sessions |
| `DELETE /api/auth/sessions/{id}` | Revoke one of the caller's sessions |

Invited members set their own password on first sign-in; admins never see
member passwords. In IAM Core OIDC mode the entire built-in `/api/auth/*`
surface is disabled and returns `409 Conflict` (§5).

## 3. TOTP two-factor

ZK Drive implements RFC 6238 time-based one-time passwords in
`internal/totp`. The parameters are the RFC default profile
(`internal/totp/service.go:25-44`):

- **SHA-1**, **6 digits**, **30-second** period.
- A validation skew of **±1 window** absorbs clock drift
  (`internal/totp/service.go:44`).
- **10 recovery codes** are issued at enrollment
  (`internal/totp/service.go:51`); each is single-use and stored
  bcrypt-hashed.

The shared secret is encrypted at rest with AES-256-GCM under
`CREDENTIAL_ENCRYPTION_KEY` (`internal/totp/totp.go:11`); it is never
stored in plaintext. Enrollment returns both a QR code (PNG) and the
base32 secret for manual entry, plus the standard otpauth URI
(`otpauth://totp/…?secret=…&algorithm=SHA1&digits=6&period=30`,
`internal/totp/service.go:376`). Reusing a code within its window is
rejected: a `last_used_at` stamp provides replay protection.

The TOTP routes sit under `/api/auth/totp` in **three groups**, one per
valid token purpose, so a token minted for one stage cannot be replayed
against another (`cmd/server/main.go:1638-1680`):

| Group (token purpose) | Routes | When |
| --- | --- | --- |
| Session JWT | `POST /enroll/begin`, `POST /enroll/finalize`, `POST /disable`, `GET /status` | Managing 2FA from account settings |
| `mfa_enroll` purpose | `POST /enroll/begin/required`, `POST /enroll/finalize/required` | Forced enrollment on a workspace that requires MFA |
| `mfa_challenge` purpose | `POST /verify` | Completing the second factor after a correct password |

The `mfa_enroll` and `mfa_challenge` tokens authorize only their own
endpoints — never the data plane — so capturing one cannot grant API
access (`cmd/server/main.go:1664-1678`).

![Two-factor enrollment](screenshots/21-two-factor-enroll.png)

## 4. OAuth / SSO (Google, Microsoft)

ZK Drive supports Google and Microsoft Entra sign-in through
`oauth2.Config` providers (`api/auth/oauth.go:85-103`). A provider is
active only when its client id is configured; an unconfigured provider
returns `501 Not Implemented` rather than a confusing error
(`api/auth/oauth.go:70-72`). The routes mount under `/api/auth/oauth`
(`api/auth/oauth.go:136-141`):

- `GET /api/auth/oauth/google` and `GET /api/auth/oauth/google/callback`
- `GET /api/auth/oauth/microsoft` and
  `GET /api/auth/oauth/microsoft/callback`

The flow is Authorization Code with **PKCE (S256)**: the start handler
generates a random state and PKCE verifier, stores them in a short-lived
signed `HttpOnly` cookie scoped to `/api/auth/oauth` (5-minute TTL), and
redirects with `code_challenge`/`code_challenge_method=S256`
(`api/auth/oauth.go:48-51`, `api/auth/oauth.go:148-172`). On callback the
server validates state, exchanges the code, fetches the provider's
userinfo, and links the provider subject to a ZK Drive user.

Two safety behaviors at callback:

- A **deactivated** account is refused with `403 Forbidden`
  (`api/auth/oauth.go:264-265`) — so a deactivated member such as Eve
  Thompson (`eve@northwind.example`) cannot sign in via SSO.
- If the resolved user has TOTP enabled, the callback issues an MFA
  challenge rather than a full session, so SSO does not bypass the second
  factor (`api/auth/oauth.go:275-281`).

SSO sign-ins are recorded in the audit log (`auth.sso_login`,
`auth.sso_link`; `internal/audit/audit.go:29-30`). The relevant config
keys are `GOOGLE_CLIENT_ID`/`SECRET`/`REDIRECT_URL` and the
`MICROSOFT_*` equivalents — see [`CONFIGURATION.md`](CONFIGURATION.md).

## 5. IAM Core OIDC provider

ZK Drive can delegate authentication entirely to
[iam-core](https://github.com/uneycom/iam-core), Uney's OAuth2/OIDC
provider. When enabled, ZK Drive becomes a standard OAuth2 **client**:
the browser runs Authorization Code + PKCE against iam-core's Universal
Login, and every `/api/*` request carries an iam-core access token that
the server verifies against iam-core's JWKS. The integration is optional
and the built-in stack is the default.

### Configuration

The provider is configured from environment variables
(`internal/config/config.go`); the exhaustive table is in
[`CONFIGURATION.md`](CONFIGURATION.md):

```bash
IAM_CORE_ISSUER_URL=https://id.example.com   # enables iam-core mode
IAM_CORE_CLIENT_ID=zk-drive-spa
IAM_CORE_CALLBACK_URL=https://drive.example.com/auth/callback
IAM_CORE_AUDIENCE=zk-drive                    # recommended (audience pinning)
IAM_CORE_SCOPES="openid email profile offline_access"
IAM_CORE_CLIENT_SECRET=                       # empty for a public SPA (PKCE only)
```

The server validates at startup and refuses to boot if the issuer is set
but the client id or callback URL is missing or malformed — a
misconfigured deployment fails fast rather than booting into a
half-configured auth state.

### Discovery

The SPA fetches `GET /api/config` (public, `Cache-Control: no-store`) to
learn which mode is active (`cmd/server/main.go:1552-1564`). In iam-core
mode it returns the issuer, authorize/token URLs, client id, redirect
URI, audience, and scopes — **never** the client secret. In built-in mode
it returns `{ "auth_mode": "builtin" }` and the SPA renders the password
form.

### Request verification

`internal/iamcore.Middleware` runs in front of the data plane and, for
every request, verifies the bearer token with `internal/iamcore.Verifier`
— signature against the JWKS key identified by the token's `kid`, plus
`iss`, `aud`, `exp`, and `nbf`. Only asymmetric algorithms (RSA/ECDSA)
are accepted, defending against `alg=none` / HMAC-confusion attacks. The
JWKS is cached per its `Cache-Control` with a single-flight guard;
resolved identities are cached briefly and never beyond the token's own
expiry.

### Tenant → workspace mapping

The `(iam_tenant_id, iam_org_id) → workspace_id` mapping is persisted in
`iam_core_tenant_workspaces`
(`migrations/039_iam_core_tenant_workspaces.up.sql`). First contact from a
tenant not seen before provisions a workspace atomically under a
transaction-scoped advisory lock, so two concurrent first-logins from the
same tenant converge on one workspace instead of racing to create two. A
token carrying neither `tenant_id` nor `org_id` is rejected with `401`
rather than landing in a default workspace (fail closed).

### Built-in endpoints in iam-core mode

When iam-core is active, a single wildcard disables the entire built-in
auth surface — `login`, `signup`, `oauth/*`, `totp/*`, `refresh`, and
`logout` — and responds `409 Conflict` with a hint pointing at
`GET /api/config`, so a stale client still calling those endpoints gets a
clear signal to switch to SSO rather than a bare `404`
(`cmd/server/main.go:1600-1612`). Federated users carry the
`FederatedPasswordSentinel` in place of a password hash, so the local
password path can never authenticate an IdP-provisioned user
(`internal/user/user.go:15-21`).

## 6. Roles & authorization

The role model at the data layer is **binary**
(`internal/user/user.go:11-12`):

- `admin` — full workspace administration (`/api/admin/*`).
- `member` — standard collaborator.

"Owner-admin" is the presentation label for a workspace's primary
administrator — the first admin created by the setup wizard, such as
Alice Chen (`alice@northwind.example`) for Northwind Trading or Morgan
Reyes (`morgan@lakeside.example`) for Lakeside Legal. At the code level an
owner-admin is an `admin`; there is no separate owner role column.

Admin-gated endpoints sit behind the `AdminOnly` middleware on top of the
data-plane spine (`cmd/server/main.go` admin group). Member management —
invite, role assignment, deactivation — is itself an admin action under
`/api/admin/users`, and each role change is audited
(`admin.user_role_change`, `internal/audit/audit.go:44`).

In iam-core mode, authorization is **authoritative upstream**:
`Identity.MappedRole()` collapses the token's roles claim to a ZK Drive
role, granting `admin` for any admin-equivalent role
(`admin`, `owner`, `administrator`, or a namespaced variant like
`zk-drive:admin`) and `member` otherwise; the match is case-insensitive
(`internal/iamcore/user_info.go:47-61`). When the token's role differs
from the stored row, the local row is synced so built-in authorization
checks and admin listings stay consistent
(`internal/iamcore/middleware.go:192-194`).

Resource-level access (sharing a folder or file with specific people) is
a separate axis from the workspace role and is managed through
`/api/permissions` and the sharing surface; see
[`ARCHITECTURE.md`](ARCHITECTURE.md) §8.

![Admin user management](screenshots/09-admin-users.png)

## 7. Sessions & device binding

Built-in sessions are tracked in Redis so they can be listed and revoked
across the whole fleet (`internal/session/redis.go`). Each session
carries a **device fingerprint** captured at sign-in and re-checked on
every request (`internal/session/redis.go:99-116`):

```
fingerprint = SHA-256( User-Agent || 0x00 || network_prefix(IP) )
```

The IP is reduced to its **network prefix** — a /16 for IPv4, /48 for
IPv6 (`internal/session/redis.go:124`) — so a legitimate client on a
dynamic address within the same ISP/region keeps a stable fingerprint,
while a different browser or a different network (the signature of a token
replayed elsewhere) yields a different fingerprint. A mismatch surfaces as
a session anomaly and forces re-authentication rather than silently
honoring the token.

Users can review and revoke their own sessions
(`GET /api/auth/sessions`, `DELETE /api/auth/sessions/{id}`); a revoke is
recorded in the audit log (`auth.session_revoke`,
`internal/audit/audit.go:22-27`). In iam-core mode, token revocation
remains iam-core's responsibility; access tokens are short-lived and the
principal cache never extends a session past the token's expiry.

## 8. Rate limiting & brute-force protection

Two independent controls protect the auth surface and the API.

**Per-user / per-workspace request rate limiting** wraps the data plane.
When Redis is configured it uses a Redis-backed limiter (shared across
replicas); otherwise it falls back to an in-memory limiter
(`cmd/server/main.go:686-695`). The limits come from `RATE_LIMIT_PER_USER`
and `RATE_LIMIT_PER_WORKSPACE`.

**Auth brute-force reputation** guards `POST /api/auth/login`
specifically (`cmd/server/main.go:712`, `cmd/server/main.go:1621`). It is
keyed per client IP and Redis-backed
(`api/middleware/ratelimit_reputation.go`):

- After **5** failed sign-ins (`DefaultAuthFailureThreshold`,
  `api/middleware/ratelimit_reputation.go:58`) the IP is hard-blocked for
  **15 minutes** (`DefaultAuthBlockDuration`,
  `api/middleware/ratelimit_reputation.go:63`); reputation is retained
  with a 24-hour TTL.
- The thresholds are tunable via `AUTH_FAILURE_THRESHOLD`,
  `AUTH_BLOCK_DURATION`, and `AUTH_REPUTATION_RETENTION`
  (`internal/config/config.go:822-826`).
- The guard **fails open** if the Redis check errors (a Redis hiccup must
  not lock everyone out), and a successful sign-in **resets** the IP's
  reputation.

The platform control plane (`/api/platform/*`) has no workspace JWT to
key the normal limiter on, so it runs its own per-client-IP limiter before
authentication as defense-in-depth (`cmd/server/main.go:1896`).

Workspaces can additionally enforce **conditional access** with an IP
allowlist; when enabled, requests outside the allowed CIDRs are refused
on both the REST data plane and the realtime upgrade. The allowlist is
deliberately not applied to `/api/admin/*` so an admin cannot lock
themselves out (`cmd/server/main.go:1742-1749`).

## 9. Tenant isolation

Identity feeds isolation. After authentication, `TenantGuard` binds the
workspace into the Postgres row-level-security session variable
(`cmd/server/main.go:1740`), so every query is physically constrained to
the caller's workspace
(`migrations/024_row_level_security.up.sql`). `SuspensionGuard` returns
`503` for a suspended workspace across both REST and realtime transports.
This is the same enforcement path for built-in and iam-core identities;
see [`ARCHITECTURE.md`](ARCHITECTURE.md) §10 for the full request
lifecycle.

## 10. Configuration

Every key referenced here — `JWT_SECRET`, `JWT_ALGORITHM`,
`JWT_KEY_REFRESH_INTERVAL`, `CREDENTIAL_ENCRYPTION_KEY`, the `GOOGLE_*` /
`MICROSOFT_*` SSO keys, the `IAM_CORE_*` keys, `RATE_LIMIT_PER_USER` /
`RATE_LIMIT_PER_WORKSPACE`, `AUTH_FAILURE_THRESHOLD` /
`AUTH_BLOCK_DURATION` / `AUTH_REPUTATION_RETENTION`, and `AUDIT_HMAC_KEY`
— is documented with its default and validation rules in
[`CONFIGURATION.md`](CONFIGURATION.md). This document describes their
behavior; that document is the canonical reference table.
