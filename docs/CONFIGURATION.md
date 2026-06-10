# Configuration Reference

ZK Drive is configured entirely via environment variables. Every
binary reads them at startup and logs the effective value (or
`<empty>` / `<unset>`) for every key, so operators can confirm the
runtime configuration without re-reading their deploy manifests.

This document is the canonical reference for every variable the
server, worker, and out-of-band binaries read.

## Required

These two are required by every binary that touches the database
(`server`, `worker`, `migrate`, `reconciler`, `orphan-gc`,
`audit-archiver`). The `audit-restore` CLI does not require them — it
is read-only against S3.

| Variable       | Purpose                                                |
| -------------- | ------------------------------------------------------ |
| `DATABASE_URL` | Postgres DSN (pgx-style).                              |
| `JWT_SECRET`   | HS256 signing secret for session tokens.               |

## Deployment profile

| Variable          | Default       | Purpose |
| ----------------- | ------------- | ------- |
| `ZKDRIVE_PROFILE` | `development` | Bundles security defaults so a non-technical SME operator gets a hardened configuration from one variable instead of tuning a dozen knobs. Parsed case-insensitively (`prod` is accepted as an alias of `production`); empty or unrecognised values fall back to `development` — the server never silently assumes production. |

Profiles:

- **`production`** — hardened. JWT signing is **asymmetric-only**: the
  default `JWT_ALGORITHM` becomes `ES256`, the server **auto-generates
  an ES256 signing key at startup** if none exists yet (so first login
  works with no manual rotation step), and HS256 tokens are **rejected
  on verification** — a leaked `JWT_SECRET` can neither sign nor forge
  a session the server will accept. `Expect-CT` is emitted for TLS
  deployments.
- **`compact`** / **`development`** (default) — relaxed. The HS256
  fallback is retained for single-binary and local-dev simplicity
  (`JWT_ALGORITHM` defaults to `auto`).

An explicit `JWT_ALGORITHM` always overrides the profile default.

## Server runtime

| Variable         | Default      | Purpose                                                                                       |
| ---------------- | ------------ | --------------------------------------------------------------------------------------------- |
| `LISTEN_ADDR`    | `:8080`      | HTTP listen address for the API server.                                                       |
| `MIGRATIONS_DIR` | `migrations` | Path to SQL migrations applied by `migrate` (read-only by `server` / `worker`).                |
| `STATIC_DIR`     | _empty_      | Optional path to the frontend Vite build. When set, the server serves the SPA from this dir.   |
| `TRUSTED_PROXY_DEPTH` | `1`     | Number of trusted reverse proxies in front of the server. Governs how the IP-allowlist middleware resolves the client IP from `X-Forwarded-For`. See [IP allowlisting](#ip-allowlisting-trusted_proxy_depth).  |

### IP allowlisting (`TRUSTED_PROXY_DEPTH`)

Workspaces may restrict access to a set of public CIDR ranges
(conditional access). When a workspace enables its allowlist, the
server enforces it on every authenticated data-plane request by
resolving the client IP and rejecting any address not covered by a
rule with `403 Forbidden` and an `X-ZkDrive-IP-Blocked: true` header.

`TRUSTED_PROXY_DEPTH` tells the server how many reverse proxies (load
balancers, CDNs) sit in front of it so it can trust the right entry in
the `X-Forwarded-For` header. The header is a left-to-right list of
addresses appended by each hop; only the right-most entries — those
added by infrastructure you control — are trustworthy, because a
client can forge any value to the left. The middleware takes the
address `TRUSTED_PROXY_DEPTH` entries from the **right**:

- `1` (default): a single trusted proxy (e.g. one ALB/nginx). The
  client IP is the last `X-Forwarded-For` entry.
- `2`: two trusted hops (e.g. CDN → load balancer). The client IP is
  the second-from-last entry.
- `0`: no trusted proxy; ignore `X-Forwarded-For` entirely and use the
  raw TCP peer address (`RemoteAddr`).

If the header is absent or has fewer entries than the configured
depth, the server falls back to `RemoteAddr`. **Set this to the actual
number of proxies in your deployment** — too low admits spoofed
addresses, too high trusts a client-supplied entry. Because the
allowlist only accepts public ranges, the resolved client IP must be a
routable public address, so it must reflect the real external client
rather than an internal proxy hop.

**Allowlistable ranges.** Only publicly routable CIDRs are accepted.
Private (RFC1918 / RFC4193), loopback, link-local, unspecified,
multicast, and RFC 6598 carrier-grade-NAT shared space
(`100.64.0.0/10`) are rejected, because an internet-facing gateway
never legitimately observes them as a client source address.

**Enabling requires at least one rule.** Because matching fails
closed, turning the allowlist on for a workspace with no rules would
block every data-plane request for that workspace. The policy endpoint
therefore rejects an enable with no rules (`409`,
`IP_ALLOWLIST_NO_RULES`); add a rule first. Disabling is always
allowed.

**Cannot remove the last rule while enabled.** For the same
fail-closed reason, deleting the final rule of an *enabled* allowlist
is rejected (`409`, `IP_ALLOWLIST_LAST_RULE`) — it would leave the
workspace enabled with zero rules and lock everyone out. Disable the
allowlist first, then remove the rule. Both this guard and the
enable-requires-a-rule guard are enforced atomically under a
per-workspace row lock, so concurrent "enable" and "remove last rule"
requests can never race the workspace into the locked-out state.

**Enforcement scope.** The allowlist is enforced on authenticated
data-plane HTTP requests — the main drive routes and the `/api/kchat`
routes. The `/api/admin` routes are intentionally exempt so an admin
who misconfigures the allowlist can still reach the management
endpoints to fix it.

The long-lived WebSocket endpoints (`/api/ws` and
`/api/documents/{id}/ws`) are **also** gated by the allowlist. They
still skip `TenantGuard` and the rate limiter (that stack assumes
ordinary request/response semantics and would otherwise charge per WS
frame), but the IP-allowlist check runs on the initial upgrade request,
before the handshake completes — it needs only the workspace id, which
the auth middleware binds from the JWT claims. A request from a blocked
network is therefore rejected with the same `403` + `X-ZkDrive-IP-Blocked`
response as the REST routes and never opens a socket.

## Database connection pool

These tune the pgxpool created at startup by `server` and `worker`.
Values are clamped at load time so a typo can neither starve the pool
nor exhaust Postgres' `max_connections`. Other binaries (`migrate`,
`reconciler`, `orphan-gc`, `audit-archiver`) use the default sizing.

| Variable                 | Default | Purpose                                                                                                  |
| ------------------------ | ------- | -------------------------------------------------------------------------------------------------------- |
| `DB_MAX_CONNS`           | `20`    | Maximum open connections in the pool. Clamped to `[2, 200]`.                                              |
| `DB_MIN_CONNS`           | `2`     | Minimum idle connections kept warm. Clamped to `[0, DB_MAX_CONNS]`.                                       |
| `DB_MAX_CONN_IDLE_TIME`  | `30m`   | How long an idle connection is kept before being closed. Go duration string (e.g. `45s`, `5m`, `1h`).     |

### Sizing the pool across replicas (read this before scaling out)

`DB_MAX_CONNS` is **per process**, not per cluster. Every `server`
(and `worker`) replica opens its own pool, so the load Postgres
actually sees is:

```
peak backends ≈ server_replicas × server.DB_MAX_CONNS
              + worker_replicas × worker.DB_MAX_CONNS
              + (migrate / reconciler / orphan-gc / audit-archiver, default sizing)
```

The default `DB_MAX_CONNS` was raised from `10` to `20`. With the
server HPA scaling up to **20 pods**, that is `20 × 20 = 400`
connections from the server tier alone — before the worker tier and
the one-shot jobs. A stock Postgres ships with `max_connections = 100`,
so a fully scaled-out fleet will exhaust it and new connections will be
refused (`FATAL: sorry, too many clients already`).

**Plan for one of these before scaling past a handful of replicas:**

- **Put PgBouncer (transaction pooling) in front of Postgres** and
  point `DATABASE_URL` at it. This is the intended production topology
  and is what the Terraform / Helm chart wires up. PgBouncer multiplexes
  the fleet's many client connections onto a small set of real Postgres
  backends, so `replicas × DB_MAX_CONNS` becomes the *client* count
  (cheap) rather than the *backend* count (scarce).
- **Or** raise Postgres `max_connections` to comfortably exceed the
  worst-case sum above (and size RAM accordingly — each backend costs
  several MB).
- **Or** lower `DB_MAX_CONNS` so `replicas × DB_MAX_CONNS` stays under
  Postgres `max_connections` with headroom.

> ⚠️ **In-place upgrades:** if you are upgrading an existing deployment
> that scaled replicas under the old `DB_MAX_CONNS=10` default and you
> do **not** run PgBouncer, the bump to `20` doubles the backend count
> and can push you past `max_connections`. Either keep `DB_MAX_CONNS=10`
> explicitly, raise Postgres `max_connections`, or adopt PgBouncer as
> part of the upgrade — make it a conscious choice rather than a
> surprise.

## JWT signing

| Variable                   | Default | Purpose                                                                                                                                            |
| -------------------------- | ------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| `JWT_ALGORITHM`            | `auto` (`ES256` under the `production` profile)  | Session-token signing algorithm. `auto` signs with ES256 when an active asymmetric key exists in `jwt_signing_keys`, else HS256 (`JWT_SECRET`). `ES256` forces asymmetric signing — if no active key has been rotated in yet, token signing **fails** rather than silently downgrading to HS256 (run `POST /api/platform/jwt/rotate` first, or use the `production` profile which auto-generates one at startup). `HS256` forces legacy symmetric signing. Under non-production profiles verification accepts both, so rotating to ES256 never invalidates existing HS256 sessions; under the `production` profile HS256 tokens are **rejected** on verification (asymmetric-only). The default is profile-dependent — see [Deployment profile](#deployment-profile). |
| `JWT_KEY_REFRESH_INTERVAL` | `60s`   | How often each replica re-reads `jwt_signing_keys` so a key rotation performed on one replica propagates to all others without a restart. Go duration string, clamped to `[10s, 1h]`. A non-positive value (e.g. `0`) disables the background refresh — appropriate for single-replica deployments. |
| `PLATFORM_ADMIN_USER_IDS`  | _(empty)_ | **Legacy / no longer used.** Earlier releases gated a per-workspace admin JWT-rotation endpoint behind this allowlist. Rotation has since moved to the platform control plane (`POST /api/platform/jwt/rotate`, gated by the `keys:manage` platform-API-key capability) and the admin endpoint was removed, so this var no longer gates anything. The server logs a startup warning if it is still set; drop it from your config. |

## Storage (zk-object-fabric S3 gateway)

These four are required together. If `S3_ENDPOINT` is unset, the
server still boots and serves metadata-only endpoints, but
`/api/files/upload-url`, `/api/files/confirm-upload`, and
`/api/files/{id}/download-url` respond `501 Not Implemented`. If
`S3_ENDPOINT` is set, the bucket, access key, and secret key must
also be set; otherwise startup fails.

| Variable        | Purpose                                                                                |
| --------------- | -------------------------------------------------------------------------------------- |
| `S3_ENDPOINT`   | zk-object-fabric gateway base URL (e.g. `http://localhost:8080`).                       |
| `S3_BUCKET`     | Bucket to store all file versions under.                                                |
| `S3_ACCESS_KEY` | Gateway access key.                                                                     |
| `S3_SECRET_KEY` | Gateway secret key.                                                                     |

## Cache, queue, and scan dependencies

| Variable         | Default | Purpose                                                                                                     |
| ---------------- | ------- | ----------------------------------------------------------------------------------------------------------- |
| `REDIS_URL`      | _empty_ | Redis / Valkey DSN. When set, sessions, rate-limit counters, and the WebSocket fan-out hub use Redis.        |
| `NATS_URL`       | _empty_ | NATS JetStream URL. When set, the server publishes async-job and webhook events and the worker consumes.     |
| `CLAMAV_ADDRESS` | _empty_ | `host:port` of a ClamAV INSTREAM daemon. When set, uploads are virus-scanned before becoming visible.        |

## Credential encryption

Workspace-scoped credentials (per-tenant S3 keys, TOTP secrets) are
stored encrypted at rest with AES-256-GCM. The same key protects
both surfaces; rotating the key requires the standard credential
re-encryption runbook.

| Variable                     | Default   | Purpose                                                                                                  |
| ---------------------------- | --------- | -------------------------------------------------------------------------------------------------------- |
| `CREDENTIAL_ENCRYPTION`      | `aesgcm`  | Mode. `aesgcm` (production) or `passthrough` (development only — credentials are stored in cleartext).    |
| `CREDENTIAL_ENCRYPTION_KEY`  | _empty_   | 32-byte base64 key. Required when `CREDENTIAL_ENCRYPTION=aesgcm`.                                         |

## Rate limiting

| Variable                  | Default | Purpose                                                                                                                |
| ------------------------- | ------- | ---------------------------------------------------------------------------------------------------------------------- |
| `RATE_LIMIT_PER_USER`     | `0`     | Requests per user per minute. `0` disables. When `REDIS_URL` is set the limiter is Redis-backed and survives restarts.  |
| `RATE_LIMIT_PER_WORKSPACE`| `0`     | Requests per workspace per minute. Same semantics as the per-user limiter, just a different scope.                      |

Every rate-limited response (allowed **and** throttled) carries the
standard telemetry headers so clients can self-pace:

- `X-RateLimit-Limit` — the per-window request budget.
- `X-RateLimit-Remaining` — requests left in the current window (`0` on a 429).
- `X-RateLimit-Reset` — unix second at which the window resets.

A `429` additionally carries `Retry-After` (seconds).

### Auth brute-force reputation

Independent of the per-user/-workspace limiter, `POST /api/auth/login`
is protected by a **per-client-IP** brute-force guard. It tracks failed
sign-ins per IP and, once the failure threshold is crossed, escalates a
cooldown the IP must wait out before its next attempt is accepted: `1s`,
then `5s`, then `30s`, then a hard block. Attempts made inside a cooldown
are rejected with `429 AUTH_TOO_MANY_ATTEMPTS` + `Retry-After` **before**
the password is checked, and a *successful* sign-in clears the IP's
reputation so a legitimate user is never punished for earlier typos.

The escalation is a cooldown, **not** a connection tarpit, so it adds no
held goroutines/sockets (which an attacker could weaponise). The client
IP is resolved with `TRUSTED_PROXY_DEPTH`, so a spoofed `X-Forwarded-For`
cannot dodge the guard. With `REDIS_URL` set the reputation is shared
across replicas (and retained for the window below); otherwise a
per-replica in-memory fallback still provides best-effort protection.

Because the key is the client IP, a large NAT (a whole office behind one
egress IP) shares one reputation — which is why the hard block defaults
to a short 15 minutes rather than the full retention window.

| Variable                   | Default | Purpose                                                                                                                                            |
| -------------------------- | ------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| `AUTH_FAILURE_THRESHOLD`   | `5`     | Failed sign-ins tolerated per client IP before cooldowns begin. The first `threshold-1` failures are free (human typos); the threshold-th arms the first cooldown. `<= 0` falls back to the default. |
| `AUTH_BLOCK_DURATION`      | `15m`   | Hard-block cooldown applied once the progressive `1s/5s/30s` delays are exhausted. Kept short so a shared-NAT office is not locked out for hours. Accepts a Go duration (e.g. `30m`). `<= 0` falls back to the default. |
| `AUTH_REPUTATION_RETENTION`| `24h`   | How long an IP's failure counter survives with no further failures (the Redis TTL). Accepts a Go duration. `<= 0` falls back to the default.        |

## Preview pipeline

Read by the `worker` binary. These tune the per-tenant preview
(thumbnail) budget and the tier-based priority worker pools. The
budget is **fail-open**: with no `REDIS_URL` the enforcer is nil and
every preview is admitted, so Redis is a fairness guard, never a
correctness dependency. See `docs/OPERATIONS.md` ("Preview pipeline
scaling") for the runbook.

| Variable                            | Default | Purpose                                                                                                                                                              |
| ----------------------------------- | ------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `PREVIEW_BUDGET_PER_WORKSPACE_HOUR` | `100`   | Max previews rendered per workspace per rolling hour. Over budget, the job is `NakWithDelay`-deferred (exponential backoff up to 5m) rather than dropped. Needs `REDIS_URL`; with no Redis the budget is unenforced (all admitted). A value `<= 0` falls back to the default (`100`), so to keep Redis but effectively disable the budget, set this very high (e.g. `1000000`). |
| `PREVIEW_PRIORITY_WORKERS`          | `6`     | Goroutine pool size for the priority preview subject (`drive.preview.generate.priority`, Business/Secure-Business tiers). Values `<= 0` fall back to the default; the pool is clamped to a minimum of 1. |
| `PREVIEW_STANDARD_WORKERS`          | `2`     | Goroutine pool size for the standard preview subject (`drive.preview.generate.standard`, Free/Starter tiers). Same `<= 0` / minimum-1 semantics as the priority pool. |

## OAuth2 single sign-on (Google / Microsoft)

All six are optional. SSO is enabled per provider when the three
fields for that provider are all set; otherwise that provider is
hidden from the login screen.

| Variable                    | Purpose                                                                              |
| --------------------------- | ------------------------------------------------------------------------------------ |
| `GOOGLE_CLIENT_ID`          | OAuth2 client ID issued by Google Cloud Console.                                      |
| `GOOGLE_CLIENT_SECRET`      | OAuth2 client secret.                                                                 |
| `GOOGLE_REDIRECT_URL`       | Callback URL registered with Google (e.g. `https://drive.example.com/auth/google/callback`). |
| `MICROSOFT_CLIENT_ID`       | Application (client) ID from the Azure AD app registration.                           |
| `MICROSOFT_CLIENT_SECRET`   | OAuth2 client secret.                                                                 |
| `MICROSOFT_REDIRECT_URL`    | Callback URL registered with Azure AD.                                                |

## Storage gateway integration (zk-object-fabric console)

The admin surface (workspace placement, CMK validation, tenant
provisioning) talks to the zk-object-fabric console API. Leave the
console URL empty to disable the advanced storage admin surface.

| Variable                       | Default                  | Purpose                                                                |
| ------------------------------ | ------------------------ | ---------------------------------------------------------------------- |
| `FABRIC_CONSOLE_URL`           | _empty_                  | zk-object-fabric console base URL.                                      |
| `FABRIC_CONSOLE_ADMIN_TOKEN`   | _empty_                  | Admin bearer token for the console API.                                 |
| `FABRIC_BUCKET_TEMPLATE`       | `zk-drive-{tenant}`      | Template for per-tenant bucket names. `{tenant}` is the workspace slug. |
| `FABRIC_DEFAULT_PLACEMENT_REF` | `b2c_pooled_default`     | Placement reference applied to new workspaces.                          |

## Stripe billing

| Variable                  | Purpose                                                                                                                  |
| ------------------------- | ------------------------------------------------------------------------------------------------------------------------ |
| `STRIPE_SECRET_KEY`       | Stripe API secret. When set, the billing admin endpoints become live.                                                     |
| `STRIPE_WEBHOOK_SECRET`   | `whsec_…` signing secret for the Stripe webhook endpoint. Required for `checkout.session.completed` / `customer.subscription.*` / `invoice.*` ingestion. |
| `STRIPE_PRICE_TIER_MAP`   | Comma-separated `price_id=tier` pairs (e.g. `price_abc=starter,price_xyz=business`). Maps Stripe prices to ZK Drive tiers. |

## AI thread summary (optional)

| Variable       | Default | Purpose                                                                                                   |
| -------------- | ------- | --------------------------------------------------------------------------------------------------------- |
| `OLLAMA_URL`   | _empty_ | Base URL of a local Ollama server (e.g. `http://ollama:11434`). When unset the summariser falls back to a deterministic rule-based mode. |
| `OLLAMA_MODEL` | _empty_ | Model name to request (e.g. `llama3:8b`). Ignored when `OLLAMA_URL` is unset.                              |

## Web Push notifications (optional)

PWA users receive notifications (share-link created, etc.) even when no
browser tab is open via Web Push (RFC 8030 + VAPID). The server delivers
a push only to users who have registered a subscription **and** have no
live WebSocket connection, so an open tab still uses the realtime path.

| Variable             | Default | Purpose                                                                                                       |
| -------------------- | ------- | ------------------------------------------------------------------------------------------------------------- |
| `VAPID_PUBLIC_KEY`   | _empty_ | VAPID application-server **public** key. Sent to the browser as `applicationServerKey` and in the VAPID header. |
| `VAPID_PRIVATE_KEY`  | _empty_ | VAPID application-server **private** key used to sign the VAPID JWT. Keep secret.                              |
| `VAPID_SUBSCRIBER`   | _built-in placeholder_ | `sub` claim in the VAPID JWT — a `mailto:` or `https:` URI push services use to contact you about a misbehaving sender. Set to a monitored mailbox (e.g. `mailto:ops@yourdomain.com`) for production push traffic. |

When **either** key is empty, Web Push is disabled (graceful
degradation): the `/api/push/*` endpoints respond `501 Not Implemented`,
the frontend skips the subscription flow, and the notification publisher
skips the push fan-out — the in-app + WebSocket notification path is
unaffected.

Generate a key pair once and inject the same pair into every replica:

```sh
npx web-push generate-vapid-keys
# =>
# Public Key:  BMod_...           # set as VAPID_PUBLIC_KEY
# Private Key: 3K...              # set as VAPID_PRIVATE_KEY
```

The two keys are a matched pair: rotating them invalidates every
existing browser subscription (clients re-subscribe automatically on
next login). The browser fetches the public key from
`GET /api/push/vapid-public-key`; subscriptions are registered via
`POST /api/push/subscribe` and removed via `DELETE /api/push/subscribe`.

## Collaborative office editing (ONLYOFFICE)

Lets users open office documents (`.docx`, `.xlsx`, `.pptx`, `.odt`,
`.csv`, …) in an embedded [ONLYOFFICE Document
Server](https://www.onlyoffice.com/) editor. The server hands the
browser a presigned GET URL for the current version; when the user
finishes editing, the Document Server POSTs the edited bytes back to a
ZK Drive callback, which stores them as a **new file version**.

Office editing requires the server to read and write the document, so
it is available **only for `managed_encrypted` folders**. Files in
`strict_zk` (zero-knowledge) folders return `403` — the server holds no
key and must not see plaintext.

| Variable            | Default | Purpose                                                                                                                                                             |
| ------------------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ONLYOFFICE_URL`    | _empty_ | Base URL of the ONLYOFFICE Document Server (e.g. `https://onlyoffice.example.com`). When empty, office editing is disabled: `/api/onlyoffice/status` reports `enabled:false`, the editor-config endpoint returns `503`, and the frontend hides the "Edit" / "Open in editor" affordances. |
| `ONLYOFFICE_SECRET` | _empty_ | Shared JWT secret matching the Document Server's `JWT_SECRET` (`JWT_ENABLED=true`). ZK Drive signs the editor config with it (HS256) and verifies the inbound save callback against it. When empty, the config is emitted unsigned and the callback skips verification — acceptable only for trusted local development. |
| `ONLYOFFICE_MAX_DOCUMENT_MB` | `100` | Maximum size (MiB) of a single edited document the save callback will buffer in memory before rejecting it. Generous — real office documents are 1–50 MiB. |
| `ONLYOFFICE_SAVE_MEMORY_BUDGET_MB` | `256` | Total memory (MiB) the save path may buffer **concurrently**. The save-concurrency cap is **derived** as `budget ÷ per-document`, so the worst case (`concurrency × per-document`) never exceeds the budget. Must be `>=` `ONLYOFFICE_MAX_DOCUMENT_MB` or the server refuses to start. |

**Save concurrency & memory.** The editor-callback route runs outside
the per-user / per-workspace rate limiter (the Document Server holds no
ZK Drive JWT), so a burst of save callbacks could each buffer a whole
document and OOM the API container. ZK Drive bounds this with a counting
semaphore whose size is **derived** from the two knobs above:
`max_concurrent_saves = ONLYOFFICE_SAVE_MEMORY_BUDGET_MB ÷ ONLYOFFICE_MAX_DOCUMENT_MB`
(floored at 1). The defaults (`256 ÷ 100 = 2`) are sized for the 512 MiB
production API container in
[`deploy/docker-compose.prod.yml`](../deploy/docker-compose.prod.yml)
(half the container, leaving headroom for the runtime and other
requests). Excess callbacks are shed with a retryable `503` — the
Document Server keeps the edited bytes in its cache and retries, so a
storm degrades gracefully instead of OOMing.

- **Raising throughput** on a larger container: raise
  `ONLYOFFICE_SAVE_MEMORY_BUDGET_MB` (e.g. to half a 1 GiB container →
  `512`, giving concurrency `5` at the default per-document cap). Do
  **not** simply raise it past what the container can hold — the budget
  exists to keep `concurrency × per-document` inside the container.
- **Smaller documents** (lowering `ONLYOFFICE_MAX_DOCUMENT_MB`) raises
  concurrency at the same budget, but rejects larger legitimate
  documents — keep it above your largest expected office file.

The callback URL the Document Server posts to is composed from
[`PUBLIC_URL`](#transactional-email-guest-invite-delivery): `${PUBLIC_URL}/api/files/{id}/editor-callback?workspace_id={ws}`.
Ensure `PUBLIC_URL` is reachable from the Document Server's network and
that the Document Server's cache URL is reachable from ZK Drive (the
callback fetches the edited bytes from it).

Setup:

1. Deploy a Document Server (see the optional `onlyoffice` service in
   [`deploy/docker-compose.prod.yml`](../deploy/docker-compose.prod.yml)).
2. Set `JWT_ENABLED=true` and a strong `JWT_SECRET` on the Document
   Server.
3. Set `ONLYOFFICE_URL` and a matching `ONLYOFFICE_SECRET` on the ZK
   Drive server.
4. Confirm `/api/onlyoffice/status` returns `{"enabled":true}` once
   authenticated.

> **CSP note:** the embedded editor loads `api.js` from the Document
> Server and renders inside an iframe. Add the Document Server origin to
> the relevant `SECURITY_HEADERS_CSP_*` allowances (script / frame /
> connect sources) so the browser can load it.

## Workspace suspension enforcement

When the platform control plane suspends a workspace, `SuspensionGuard`
(REST + WebSocket) and the ONLYOFFICE save-callback write boundary
return `503` for that workspace. These knobs control what happens when
the suspension **lookup itself** errors (e.g. a transient database
blip).

| Variable                 | Default | Purpose                                                                                                                          |
| ------------------------ | ------- | -------------------------------------------------------------------------------------------------------------------------------- |
| `SUSPENSION_FAIL_CLOSED`  | `false` | Posture on a suspension-lookup error. `false` (default) **fails open**; `true` **fails closed** (returns `503`). See below.      |

- **`false` — fail open (default).** A lookup error lets the request
  proceed. Suspension is an **availability control** (e.g. billing /
  non-payment), not a security boundary, so a transient database
  outage must not lock the entire fleet out of their own data. This is
  the right default for the overwhelming majority of deployments.
- **`true` — fail closed.** A lookup error rejects the request with
  `503` and a distinct `{"error":"suspension_check_unavailable"}` body
  (separate from a confirmed `{"error":"workspace_suspended"}`, so
  tooling can tell "can't confirm" from "confirmed suspended"). Enable
  this **only** if you use suspension for **compliance / legal holds**,
  where a held workspace must never transact even during a database
  outage — accepting that a suspension-lookup outage will block *all*
  workspaces (including healthy ones) until the lookup recovers. The
  ONLYOFFICE save callback honours the same posture, returning a
  retryable error so the Document Server keeps the edited bytes and
  retries once the lookup recovers (no data loss).

## Browser security headers

Emitted on every response by `api/middleware.SecurityHeaders`. The
defaults are safe for a same-origin SPA; these knobs are how
operators allow-list the storage gateway origin and stage CSP
rollouts.

| Variable                              | Default | Purpose                                                                                                                                                                  |
| ------------------------------------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `SECURITY_HEADERS_CSP_CONNECT_EXTRA`  | _empty_ | Comma-separated origins added to CSP `connect-src` on top of `'self'`. Put the **public** URL the browser sees for the fabric storage gateway here so direct-to-storage uploads / downloads land. The default omits bare `wss:` / `ws:` scheme sources (an XSS exfil vector). |
| `SECURITY_HEADERS_CSP_IMG_EXTRA`      | _empty_ | Comma-separated origins added to CSP `img-src` on top of `'self' data: blob:`. Same gateway origin if thumbnails are served from it.                                       |
| `SECURITY_HEADERS_CSP_REPORT_ONLY`    | `false` | When `true`, the policy emits under `Content-Security-Policy-Report-Only` instead of enforcing — browsers report violations but do not block. Use during the first rollout, then flip to `false`. |
| `SECURITY_HEADERS_CSP_REPORT_URI`     | _empty_ | When set, appended as `report-uri <value>` to the CSP value. Browsers POST violation reports there.                                                                      |
| `SECURITY_HEADERS_DISABLE_HSTS`       | `false` | When `true`, skips `Strict-Transport-Security`. Use for local HTTP development only; keep `false` in production.                                                          |
| `SECURITY_HEADERS_CSP_NONCE`          | `true`  | When `true`, a fresh per-request `'nonce-<base64>'` source is added to CSP `script-src` and the same nonce is injected into the `<meta name="csp-nonce">` tag of the served `index.html`. The policy already ships nonce-clean (no inline scripts), so this is purely additive: a future inline script can be allow-listed by nonce without reopening `'unsafe-inline'`. Read it in the SPA via `document.querySelector('meta[name=csp-nonce]').content`. |
| `SECURITY_HEADERS_EXPECT_CT`          | _profile_ | Emits `Expect-CT: max-age=86400, enforce`. Defaults to **on** under the `production` profile and **off** otherwise; an explicit value wins. Suppressed automatically when HSTS is disabled (Expect-CT is HTTPS-only). Superseded by browsers enforcing Certificate Transparency by default, but still honoured by deployed clients. |

### CSP nonce & inline scripts (6.5)

`script-src` is nonce-clean: it carries **no** `'unsafe-inline'` and
**no** `'unsafe-eval'`. Vite emits external, content-hashed bundles
that load under `'self'`, so the SPA needs neither. When
`SECURITY_HEADERS_CSP_NONCE` is on, the server stamps a cryptographically
random 128-bit nonce into both the `script-src` directive and the
`index.html` meta tag on **every navigation request** (the document is
served `no-store` precisely so the nonce is never cached / reused). A
component or dependency that must inject an inline `<script>`/`<style>`
should copy that nonce onto the element so it satisfies the policy —
this keeps the door to `'unsafe-inline'` permanently shut.

`style-src` retains `'unsafe-inline'` because React's `style={}` prop
compiles to inline `style=` attributes (governed by `style-src-attr`,
which inherits `style-src`); removing it is a separate frontend
workstream (refactor every inline style to a class). Camera, microphone
and geolocation are denied outright via `Permissions-Policy`
(`camera=()`, `microphone=()`, `geolocation=()`) — stricter than the
"restrict to self" baseline, since the drive app uses none of them.

## Transactional email (guest-invite delivery)

When all three required fields are set, ZK Drive sends a templated
email to every guest invitee. With any one missing the server boots
cleanly in disabled mode and logs a single `transactional email
DISABLED` warning at startup so operators see the gap at deploy
time, not when the first invitee fails to arrive.

Required when email is enabled:

| Variable             | Purpose                                                                                                                                                  |
| -------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `PUBLIC_URL`         | Canonical externally-reachable base URL of the frontend (e.g. `https://drive.example.com`). Used to compose `/invites/{id}` links inside the email.       |
| `SMTP_HOST`          | Hostname of the SMTP relay. Anything that speaks SMTP-AUTH works (Postmark, Mailgun, AWS SES, Gmail App Passwords, corporate Exchange).                   |
| `SMTP_FROM_ADDRESS`  | Envelope sender (`MAIL FROM`) and From-header address.                                                                                                    |

Optional:

| Variable                            | Default     | Purpose                                                                                                       |
| ----------------------------------- | ----------- | ------------------------------------------------------------------------------------------------------------- |
| `SMTP_PORT`                         | `587`       | TCP port. `465` = implicit TLS, `587` = STARTTLS, `25`/`2525` = plain (dev only).                              |
| `SMTP_USERNAME`                     | _empty_     | SMTP-AUTH username (PLAIN). Skipped when both username + password are empty (anonymous relay).                 |
| `SMTP_PASSWORD`                     | _empty_     | SMTP-AUTH password.                                                                                            |
| `SMTP_FROM_NAME`                    | _empty_     | Display name in From header (`"ZK Drive" <noreply@…>`).                                                        |
| `SMTP_TLS_MODE`                     | `starttls`  | One of `implicit`, `starttls`, `none`. `none` is plain text — local dev only.                                  |
| `SMTP_TLS_SERVER_NAME`              | `SMTP_HOST` | SNI / cert-verify hostname override. Set this when the relay is reachable by IP but presents a hostname cert.   |
| `SMTP_TLS_INSECURE_SKIP_VERIFY`     | `false`     | Disable certificate verification. Operators with self-signed dev relays only — keep `false` in production.      |

Provider-specific guidance is in [`OPERATIONS.md`](OPERATIONS.md#smtp-provider-notes).

## OpenTelemetry tracing

Tracing is disabled by default. Setting `OTEL_EXPORTER_OTLP_ENDPOINT`
to any collector URL turns it on; the propagator is installed either
way so distributed correlation IDs continue to flow through your
deployment even with the local exporter disabled.

| Variable                        | Default    | Purpose                                                                                                                                                          |
| ------------------------------- | ---------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `OTEL_EXPORTER_OTLP_ENDPOINT`   | _empty_    | Collector URL (e.g. `https://otlp.honeycomb.io:443`, `http://otel-collector.observability.svc:4318`). **Empty = tracing disabled (no-op provider).**              |
| `OTEL_EXPORTER_OTLP_HEADERS`    | _empty_    | Comma-separated `key=value` pairs added to every export request (e.g. `x-honeycomb-team=<key>`, `dd-api-key=<key>`).                                              |
| `OTEL_EXPORTER_OTLP_INSECURE`   | `false`    | Set to `true` to skip TLS verification — local collectors only.                                                                                                   |
| `OTEL_EXPORTER_OTLP_COMPRESSION`| `gzip`     | `gzip` or `none`.                                                                                                                                                 |
| `OTEL_SERVICE_NAME`             | `zk-drive` | `service.name` resource attribute. Server and worker share this so trace backends present them as one logical service; `service.instance.id` distinguishes them. |
| `OTEL_DEPLOYMENT_ENVIRONMENT`   | _empty_    | `deployment.environment` resource attribute, e.g. `production` / `staging`. Omitted entirely when unset.                                                          |
| `OTEL_TRACES_SAMPLER_ARG`       | `0.1`      | Root-span sample ratio. Set `1.0` in dev / staging for full visibility; `0.0` keeps propagation working but stops root sampling.                                  |

The sampler is **parent-based**: if the request arrives carrying a
sampled W3C trace-context, ZK Drive honours it. Root spans use
`OTEL_TRACES_SAMPLER_ARG`.

## Audit-log cold archival

Opt-in via `AUDIT_LOG_ARCHIVE_ENABLED=true`. With the flag off the
archiver binary logs `audit-archiver disabled by
AUDIT_LOG_ARCHIVE_ENABLED; exiting zero` and exits cleanly — safe
to schedule before the bucket is configured.

| Variable                              | Default            | Purpose                                                                                                                                                          |
| ------------------------------------- | ------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `AUDIT_LOG_ARCHIVE_ENABLED`           | `false`            | Opt-in safety floor. The archiver refuses to delete rows when unset.                                                                                              |
| `AUDIT_LOG_RETENTION_DAYS`            | `90`               | Rows older than `now() - retention_days` are eligible for archival. Clamped to `[7, 3650]`; values below 7 are raised to 7 with a startup log notice.              |
| `AUDIT_LOG_ARCHIVE_PREFIX`            | `audit-archive/`   | S3 key prefix. Normalised to a single trailing slash.                                                                                                             |
| `AUDIT_LOG_ARCHIVE_BUCKET`            | _(use `S3_BUCKET`)_| Optional dedicated bucket — typically one with Glacier transition or object-lock retention rules.                                                                  |
| `AUDIT_LOG_ARCHIVE_MAX_ROWS_PER_BATCH`| `50000`            | Batch size for the FetchBatch → encode → PUT loop. ~25 MB uncompressed, ~5 MB compressed per batch.                                                               |

Typical compliance windows:

- **SOC2 Type II** — 1 year minimum (`AUDIT_LOG_RETENTION_DAYS=365`).
- **HIPAA** — 6 years (`AUDIT_LOG_RETENTION_DAYS=2190`).
- **GDPR** — retain as long as the lawful basis applies; revisit on
  each policy change.

The full archive design and restore workflow are in
[`OPERATIONS.md`](OPERATIONS.md#audit-log-cold-archival).

## Worker runtime

The worker binary runs the JetStream consumer plus in-process
reconciliation loops. Two of those loops can also be run as
dedicated CronJobs (`reconciler`, `orphan-gc`); set the in-process
cadence to `0` to disable that loop and schedule the binary
externally.

| Variable                       | Default | Purpose                                                                                                                                                            |
| ------------------------------ | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `WORKER_METRICS_ADDR`          | `:9091` | Listen address for the worker's dedicated `/metrics` + `/healthz` HTTP server. Set to `off` (or explicit empty) to disable. The server binary serves `/metrics` on the main `LISTEN_ADDR`. |
| `RECONCILE_INTERVAL_MINUTES`   | `60`    | Cadence of the in-process storage-counter reconciler. `0` disables it — deploys that run `/app/reconciler` as a dedicated CronJob set this to `0`.                  |
| `GC_INTERVAL_MINUTES`          | `360`   | Cadence of the in-process orphan-object GC. Reclaims S3 objects whose presigned PUT completed but whose ConfirmUpload never landed. `0` disables it.                |
| `GC_PENDING_UPLOAD_TTL_HOURS`  | `168`   | Cooldown applied before a pending-upload row is considered an orphan. Default 7 days matches the trash retention window. Below the presigned-URL expiry (15 minutes) risks racing a still-uploading client. |
