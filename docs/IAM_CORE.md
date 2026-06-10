# IAM-Core OIDC Integration

ZK Drive can delegate authentication to
[iam-core](https://github.com/uneycom/iam-core), Uney's OAuth2/OIDC
identity provider, instead of using its built-in password + SSO stack.
When enabled, ZK Drive becomes a standard OAuth2 **client** of iam-core:

- the browser runs an Authorization Code + PKCE flow against iam-core's
  Universal Login, and
- every `/api/*` request carries an iam-core-issued access token that
  the server verifies against iam-core's JWKS.

The integration is **optional and backward compatible**. When
`IAM_CORE_ISSUER_URL` is empty (the default), ZK Drive falls back to its
built-in auth stack (`api/auth`, `internal/session`, `internal/totp`) so
dev/demo deployments keep working with no external identity provider.

## When to use it

| Mode      | Enable with                     | Who issues tokens | MFA            |
| --------- | ------------------------------- | ----------------- | -------------- |
| built-in  | `IAM_CORE_ISSUER_URL` empty     | ZK Drive (HS256/ES256 session JWT) | ZK Drive TOTP pages |
| iam-core  | `IAM_CORE_ISSUER_URL` set       | iam-core (verified via JWKS)       | iam-core Universal Login (TOTP, passkeys) |

iam-core mode is the recommended posture for production multi-tenant
deployments (5000+ SME tenants): authentication, MFA, and session
lifecycle are centralized in iam-core, and ZK Drive never stores a
password for federated users.

## Configuration

All settings come from environment variables (see
[`CONFIGURATION.md`](CONFIGURATION.md#iam-core-oidc-identity-provider-optional)
for the canonical table):

```bash
# Enable iam-core auth.
IAM_CORE_ISSUER_URL=https://id.example.com
IAM_CORE_CLIENT_ID=zk-drive-spa
IAM_CORE_CALLBACK_URL=https://drive.example.com/auth/callback

# Recommended.
IAM_CORE_AUDIENCE=zk-drive

# Optional.
IAM_CORE_SCOPES="openid email profile offline_access"   # default
IAM_CORE_CLIENT_SECRET=                                 # leave empty for a public SPA (PKCE only)
```

The server **validates at startup** and refuses to boot if the issuer
is set but `IAM_CORE_CLIENT_ID` or `IAM_CORE_CALLBACK_URL` is missing,
or if any URL is malformed — a misconfigured deployment fails fast and
closed rather than booting into a half-configured auth state.

### Registering the client in iam-core

Register a client with:

- **redirect URI** = `IAM_CORE_CALLBACK_URL` (e.g.
  `https://drive.example.com/auth/callback`),
- **grant types** = `authorization_code`, `refresh_token`,
- **token endpoint auth** = `none` (public SPA, PKCE) — or
  `client_secret_basic` if you set `IAM_CORE_CLIENT_SECRET` for a
  confidential deployment,
- **audience** = the value you set for `IAM_CORE_AUDIENCE`.

ZK Drive uses a **public client by default**: no secret lives in the
browser, and the Authorization Code flow is secured entirely by PKCE
(RFC 7636, S256).

## How it works

### Discovery (`GET /api/config`)

The SPA fetches `GET /api/config` (public, `Cache-Control: no-store`) at
startup to learn which mode is active. In iam-core mode it returns:

```json
{
  "auth_mode": "iam-core",
  "issuer": "https://id.example.com",
  "authorize_url": "https://id.example.com/oauth2/authorize",
  "token_url": "https://id.example.com/oauth2/token",
  "client_id": "zk-drive-spa",
  "redirect_uri": "https://drive.example.com/auth/callback",
  "audience": "zk-drive",
  "scopes": ["openid", "email", "profile", "offline_access"]
}
```

The client secret is **never** included. In built-in mode the response
is simply `{ "auth_mode": "builtin" }` and the SPA renders the password
form.

### Login flow (browser)

1. The SPA generates a PKCE `verifier`/`challenge` and a random `state`,
   stores them in `sessionStorage`, and redirects to `authorize_url`.
2. iam-core authenticates the user (including MFA) and redirects back to
   `/auth/callback?code=…&state=…`.
3. The SPA validates `state`, exchanges `code` + `verifier` at
   `token_url` for an `access_token` (+ `refresh_token`), and stores the
   access token under the same key the built-in session JWT uses, so the
   API client attaches it as `Authorization: Bearer …` unchanged.
4. The SPA calls `GET /api/me` once to resolve the ZK Drive identity
   (`user_id`, `workspace_id`, `role`) that the UI needs for admin
   gating and collaboration presence.
5. A silent refresh is scheduled ~60s before access-token expiry using
   the refresh token, so the user is never bounced through Universal
   Login while their session is alive.

### Request verification (server)

`internal/iamcore.Middleware` runs in front of the data plane and, for
every request:

1. extracts the bearer token and verifies it with
   `internal/iamcore.Verifier` — signature against the JWKS key
   identified by the token's `kid`, plus `iss`, `aud`, `exp`, and `nbf`.
   Only asymmetric algorithms (RSA/ECDSA) are accepted, defending
   against `alg=none` / HMAC-confusion attacks.
2. maps the token's `tenant_id` + `org_id` claims to a ZK Drive
   workspace (`TenantMapper`), auto-provisioning a workspace on first
   contact from a new tenant,
3. resolves (or provisions) a passwordless **federated** user row for
   the token's `sub`,
4. binds the identical `(workspaceID, userID, role)` request context the
   built-in `AuthMiddleware` produces — so every downstream handler,
   the tenant guard, and the Postgres row-level-security GUC behave
   identically regardless of which provider authenticated the caller.

The JWKS is cached and refreshed honoring the JWKS response's
`Cache-Control: max-age` (clamped to a sane range), with a single-flight
guard so a burst of requests on a cold/stale cache triggers at most one
upstream fetch. Resolved identities are cached for 60s to keep the
steady-state request path off the database, never beyond the token's own
expiry.

### Tenant → workspace mapping

The `(iam_tenant_id, iam_org_id) → workspace_id` mapping is persisted in
the `iam_core_tenant_workspaces` table (migration `039`). First login
from a new tenant provisions a workspace atomically under a
transaction-scoped advisory lock, so two concurrent first-logins from
the same tenant converge on a single workspace rather than racing to
create two.

### Roles

iam-core is authoritative for authorization. The token's `roles` claim
is mapped to a ZK Drive role (`admin`/`member`); an admin-equivalent
role (`admin`, `owner`, `administrator`, optionally namespaced like
`zk-drive:admin`) grants admin. When the token's role differs from the
stored row, the local row is synced so built-in authorization checks and
admin listings stay consistent.

## Behavior of the built-in auth endpoints in iam-core mode

When iam-core is active, the built-in password endpoints are disabled
and respond `409 Conflict` with a machine-readable hint pointing at
`GET /api/config`, so a client that still POSTs credentials gets a clear
signal to switch to SSO rather than a confusing 404:

- `POST /api/auth/login`
- `POST /api/auth/signup`

The frontend likewise skips the `/signup`, `/mfa-challenge`, and
`/mfa-enroll` routes in iam-core mode (MFA is handled upstream by
iam-core).

## Security notes

- **Public client + PKCE:** no secret in the browser; the code exchange
  is bound to the PKCE verifier (S256).
- **Audience pinning:** set `IAM_CORE_AUDIENCE` so a token minted for a
  different relying party cannot be replayed against ZK Drive.
- **Fail closed:** a token with neither `tenant_id` nor `org_id` is
  rejected (`401`) rather than landing in a default workspace.
- **Federated users carry no password:** provisioned rows store a
  sentinel in place of a password hash so the built-in password path can
  never authenticate an IdP-provisioned user.
- **Token revocation** remains iam-core's responsibility; access tokens
  are short-lived and the 60s principal cache never extends a session
  past the token's expiry.

## Tests

End-to-end coverage lives in
`tests/integration/iamcore_test.go`, which stands up a mock iam-core
OIDC server (discovery + JWKS + token endpoints) and asserts:

- a valid token resolves a workspace and the drive API works,
- a returning user / second user in the same tenant reuses the same
  workspace (no duplicate provisioning),
- expired, wrong-issuer, wrong-audience, missing-tenant, and
  missing-bearer tokens are all rejected with `401`,
- the Authorization Code + PKCE exchange round-trips (and a wrong PKCE
  verifier is refused),
- with `IAM_CORE_ISSUER_URL` empty, iam-core is disabled and the
  built-in auth path continues to work.
