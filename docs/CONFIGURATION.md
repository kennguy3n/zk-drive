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
| `DATABASE_URL` | Postgres DSN (pgx-style). Primary (writes + read-your-write). |
| `JWT_SECRET`   | HS256 signing secret for session tokens.               |

> `DATABASE_READ_URL` (optional) points the read path at a Postgres
> read replica or a PgBouncer read pool. See
> [Read replicas](#read-replicas-database_read_url) below.

## Deployment profiles (`ZKDRIVE_PROFILE`)

A profile is a named bundle of defaults that collapses the 50+ knobs
below into a single deployment-shape selector. Set `ZKDRIVE_PROFILE`
and the profile fills in sensible defaults for everything else; you
only supply the handful of genuinely site-specific variables. An
explicitly-set env var always wins over a profile default — the profile
only fills blanks, it never overrides a deliberate choice. An
unrecognised value fails closed at startup (no silent zero-preset boot).

| Variable          | Default   | Purpose                                            |
| ----------------- | --------- | -------------------------------------------------- |
| `ZKDRIVE_PROFILE` | _empty_   | One of `compact`, `production`, `development`. Empty = no profile (read every var directly; pre-profile behaviour). |

### `compact` — single-node SME (NoOps)

The shape behind [`deploy/docker-compose.compact.yml`](../deploy/docker-compose.compact.yml):
one container runs the API server + async worker (as supervised child
processes) plus an **embedded NATS JetStream** broker, targeting
<512MB RAM. Reduces configuration to ~5 site-specific variables:

| Variable        | Required | Notes                                  |
| --------------- | -------- | -------------------------------------- |
| `JWT_SECRET`    | yes      | 32+ byte signing secret.               |
| `DATABASE_URL`  | yes\*    | \*Defaults to the bundled Postgres in the compact compose file; required if you point at an external database. |
| `S3_ENDPOINT`   | yes      | zk-object-fabric / S3 endpoint.        |
| `S3_BUCKET`     | yes      | Bucket name.                           |
| `S3_ACCESS_KEY` | yes      | Access key.                            |
| `S3_SECRET_KEY` | yes      | Secret key.                            |

Everything else is defaulted by the profile:

- `ZKDRIVE_AUTO_MIGRATE=true` — schema is applied in-process on startup
  under the advisory lock (no separate `migrate` Job/binary).
- `NATS_URL=nats://127.0.0.1:4222` — the in-process embedded broker.
- No Redis — the rate limiter and session store run in-memory (single
  node, so cross-replica coordination is moot).
- ClamAV optional — virus scanning is skipped when `CLAMAV_ADDRESS` is
  unset; point it at a daemon to enable.
- Trimmed resource footprint — `DB_MAX_CONNS=10`, `DB_MIN_CONNS=1`,
  `PREVIEW_PRIORITY_WORKERS=2`, `PREVIEW_STANDARD_WORKERS=1`.

The embedded broker persists JetStream state under
`ZKDRIVE_NATS_STORE_DIR` (see [Cache, queue, and scan
dependencies](#cache-queue-and-scan-dependencies)) so durable consumers
survive a restart.

### `production` — horizontally-scaled, multi-replica

Adds **fail-closed requirements** rather than defaults: `REDIS_URL` and
`NATS_URL` are both **required** (startup aborts if either is missing),
because in-memory rate limiting / session revocation and an embedded
broker are not safe across replicas. Migrations run out-of-band via the
`migrate` Job, so `ZKDRIVE_AUTO_MIGRATE` stays off. All workers enabled.

### `development` — local laptop

The same dependency-free in-memory behaviour as `compact` but without
compact's resource clamps, so a developer's box uses the full
connection pool. Imposes no required-var validation.

## Auto-migration (`ZKDRIVE_AUTO_MIGRATE`)

| Variable               | Default | Purpose                                                       |
| ---------------------- | ------- | ------------------------------------------------------------- |
| `ZKDRIVE_AUTO_MIGRATE` | `false` | When `true`, `server` applies pending migrations before it begins listening, guarded by the same Postgres advisory lock `migrate` uses (so multiple replicas racing to boot apply migrations safely — only one wins the lock, the rest wait then no-op). Equivalent to the `--auto-migrate` flag. Always on under the `compact` profile. For production K8s the separate `migrate` Job remains recommended; leave this off there. |

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

### Read replicas (`DATABASE_READ_URL`)

For read-heavy workloads (5000 SME tenants + B2C consumers browsing
folders and searching), offload `SELECT` traffic to one or more Postgres
read replicas. Set `DATABASE_READ_URL` to the replica DSN (or a PgBouncer
pool that fronts the replicas):

```
DATABASE_URL=postgres://app@primary:5432/zkdrive?sslmode=require
DATABASE_READ_URL=postgres://app@replica:5432/zkdrive?sslmode=require
```

| Variable            | Default        | Purpose                                                                 |
| ------------------- | -------------- | ----------------------------------------------------------------------- |
| `DATABASE_READ_URL` | _(unset)_      | Read-replica DSN. Unset (or equal to `DATABASE_URL`) → reads use the primary. |

**Routing.** When set and distinct from `DATABASE_URL`, the server opens
a second `pgxpool` (sized by the same `DB_MAX_CONNS` / `DB_MIN_CONNS` /
`DB_MAX_CONN_IDLE_TIME`) and wires the read-heavy repositories (folder
tree walks, file listings, version/tag lookups) through a
`ReadWriteSplitter` (`internal/database/splitter.go`). The splitter
classifies each statement:

- **Replica:** plain `SELECT` / read-only CTE (`WITH … SELECT`) /
  `VALUES` / `TABLE` / `SHOW` / `EXPLAIN` (without `ANALYZE`).
- **Primary (always):** `INSERT` / `UPDATE` / `DELETE` / `MERGE`,
  data-modifying CTEs, `SELECT … FOR UPDATE/SHARE`, sequence functions
  (`nextval`), `EXPLAIN ANALYZE`, every `Exec`, `CopyFrom`, `SendBatch`,
  and **all transactions** (`Begin` / `BeginTx`, including read-only
  transactions — a unit of work is never split across hosts).

The classifier is conservative: anything it cannot positively prove is
read-only is routed to the primary, so a misclassification only costs a
missed offload, never a write sent to a read-only host.

**Replica lag & read-your-write.** Physical replicas are asynchronous,
so a `SELECT` issued immediately after a write may observe a slightly
stale replica. Code paths that require read-your-write consistency
(e.g. re-reading a row inside the same logical operation) run inside a
transaction, which the splitter pins to the primary. If you front the
replica with PgBouncer, use **session or transaction pooling** — not
statement pooling — so the GUC-based tenant isolation
(`SELECT set_config('app.workspace_id', …)` on connection acquire) and
RLS policies behave identically to the primary.

**Health.** Both pools are pinged at startup; a configured-but-
unreachable replica fails the boot fast rather than silently serving all
reads off the primary.

See `deploy/POSTGRES_SCALING.md` for the full PgBouncer + replica
topology, example configs, and HPA sizing math.

## JWT signing

| Variable                   | Default | Purpose                                                                                                                                            |
| -------------------------- | ------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| `JWT_ALGORITHM`            | `auto`  | Session-token signing algorithm. `auto` signs with ES256 when an active asymmetric key exists in `jwt_signing_keys`, else HS256 (`JWT_SECRET`). `ES256` forces asymmetric signing — if no active key has been rotated in yet, token signing **fails** rather than silently downgrading to HS256 (run `POST /api/platform/jwt/rotate` first). `HS256` forces legacy symmetric signing. Verification always accepts both, so rotating to ES256 never invalidates existing HS256 sessions. |
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
| `REDIS_URL`      | _empty_ | Redis / Valkey DSN. When set, sessions, rate-limit counters, the WebSocket fan-out hub, the permission cache, and the response cache (folder listings, search, storage usage) use Redis. |
| `NATS_URL`       | _empty_ | NATS JetStream URL. When set, the server publishes async-job and webhook events and the worker consumes.     |
| `CLAMAV_ADDRESS` | _empty_ | `host:port` of a ClamAV INSTREAM daemon. When set, uploads are virus-scanned before becoming visible.        |
| `ZKDRIVE_NATS_STORE_DIR` | _temp dir_ | **Compact mode only.** Filesystem directory the in-process embedded NATS JetStream broker (`cmd/compact`) persists stream/consumer state to. Mount a volume here so durable consumers survive a restart; defaults to a temporary directory (state lost on restart) when unset. Ignored when NATS runs as an external service. |

## WebSocket proxy tier (`WS_PROXY_MODE`)

| Variable        | Default | Purpose                                                                                          |
| --------------- | ------- | ------------------------------------------------------------------------------------------------ |
| `WS_PROXY_MODE` | `false` | Delegate WebSocket fan-out to an external connection proxy (Centrifugo/Pusher). Requires `REDIS_URL`. |

By default each API pod terminates its own WebSocket connections in the
in-process hub and fans events out across replicas via Redis pub/sub
(`ws:*`). For deployments past ~10k concurrent connections per pod, set
`WS_PROXY_MODE=true` to move connection holding to a dedicated proxy
tier:

- The API still **publishes** every event to Redis (`ws:{workspaceID}:{userID}`,
  unchanged envelope), but does **not** run the `ws:*`→hub subscribe
  loop — the external proxy is the subscriber and fans out to clients.
- `GET /api/ws` responds **501**; clients connect to the proxy instead.
- **Requires `REDIS_URL`.** If `WS_PROXY_MODE` is set without
  `REDIS_URL`, the server logs a warning and falls back to the
  in-process hub rather than silently dropping notifications.

See [`deploy/WEBSOCKET_PROXY.md`](../deploy/WEBSOCKET_PROXY.md) for the
Centrifugo / Pusher wire contract and example configs.

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
| `PREVIEW_LIGHTWEIGHT_WORKERS`       | `8`     | Goroutine pool size for the **lightweight** preview tier (`drive.preview.generate.lightweight`) — pure-Go renderers (images, text, archives, email) that carry no subprocess. Run these on the slim server image. Set to `0` on heavy-only pods to skip the subscription entirely. |
| `PREVIEW_HEAVY_WORKERS`             | `4`     | Goroutine pool size for the **heavy** preview tier (`drive.preview.generate.heavy`) — subprocess renderers (LibreOffice, FFmpeg, ImageMagick, PDF, SVG). Run these only on the heavy worker image that ships those binaries. Set to `0` on slim pods to skip the subscription. |
| `PREVIEW_WORKER_CONCURRENCY`        | `0`     | Per-pod cap on **concurrent subprocess renders** (the slot is taken once per heavy job, not per nested subprocess, so a Office→PDF render never deadlocks against itself). Bounds peak RAM/CPU from LibreOffice/FFmpeg, which are single-threaded and memory-hungry. `0` = unlimited (gate disabled). Size it to roughly `pod_memory ÷ per-render-RSS`. |
| `PREVIEW_HEAVY_QUEUE_BACKPRESSURE_THRESHOLD` | `0` | When `> 0`, before publishing a **heavy** preview job the publisher probes the heavy consumer's queue depth (pending + unacked) and, when it is `>=` this threshold, defers the job (`ErrPreviewDeferred`) instead of enqueueing — clients get a "preview generating…" placeholder and the file is previewed later. Lightweight jobs are never deferred. Probe errors fail **open** (the job is published): backpressure is an optimization, not a correctness gate. `0` disables the probe. |

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

## IAM-Core OIDC identity provider (optional)

When `IAM_CORE_ISSUER_URL` is set, zk-drive stops issuing its own
session JWTs and becomes a standard OAuth2 client of
[iam-core](https://github.com/uneycom/iam-core): the browser runs an
Authorization Code + PKCE flow against iam-core's Universal Login, and
every `/api/*` request carries an iam-core-issued access token that the
server verifies against iam-core's JWKS. MFA (TOTP, passkeys) is handled
by iam-core during the authorize flow, so zk-drive's built-in MFA pages
are skipped in this mode.

When `IAM_CORE_ISSUER_URL` is **empty** the server falls back to the
built-in auth stack (password + optional Google/Microsoft SSO), so
dev/demo deployments keep working with no external identity provider.
This is the default. See [`docs/IAM_CORE.md`](IAM_CORE.md) for the full
integration guide.

| Variable                | Default                    | Purpose                                                                                                   |
| ----------------------- | -------------------------- | --------------------------------------------------------------------------------------------------------- |
| `IAM_CORE_ISSUER_URL`   | _empty_                    | iam-core OIDC issuer (e.g. `https://id.example.com`). Setting it enables iam-core auth; empty = built-in. |
| `IAM_CORE_CLIENT_ID`    | _empty_                    | OAuth2 client ID registered with iam-core for this zk-drive deployment. Required when the issuer is set.   |
| `IAM_CORE_CLIENT_SECRET`| _empty_                    | Optional confidential-client secret. Leave empty for a public SPA client (PKCE only); never exposed to the browser. |
| `IAM_CORE_AUDIENCE`     | _empty_                    | Expected `aud` claim on access tokens. Verified on every request so a token minted for another relying party cannot be replayed. |
| `IAM_CORE_SCOPES`       | `openid email profile offline_access` | Space- or comma-separated scopes requested on the authorize redirect. `openid` is always included. |
| `IAM_CORE_CALLBACK_URL` | _empty_                    | OAuth2 `redirect_uri` — the SPA route that receives the authorization code (e.g. `https://drive.example.com/auth/callback`). Required when the issuer is set. |

The server validates these at startup and **fails fast** if the issuer
is set but `IAM_CORE_CLIENT_ID` or `IAM_CORE_CALLBACK_URL` is missing,
so a half-configured auth state never boots.

### Auth-mode discovery endpoints

Two endpoints let the SPA and integrators work in either mode through a
single code path:

| Method & path  | Auth       | Purpose                                                                                                              |
| -------------- | ---------- | -------------------------------------------------------------------------------------------------------------------- |
| `GET /api/config` | public  | Returns `{ auth_mode: "iam-core" \| "builtin", ... }`. In iam-core mode it also returns `issuer`, `client_id`, `authorize_url`, `token_url`, `redirect_uri`, `audience`, and `scopes` so the SPA can build the PKCE authorize redirect. Never exposes the client secret; sent `Cache-Control: no-store`. |
| `GET /api/me`     | bearer  | Returns the caller's resolved zk-drive identity (`user_id`, `workspace_id`, `role`, and profile fields). Auth-mode agnostic: it echoes the session claims in built-in mode and the tenant-mapped identity in iam-core mode. |

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
| `ONLYOFFICE_MAX_DOCUMENT_MB` | `100` | Maximum size (MiB) of a single edited document the save callback will accept before rejecting it. Generous — real office documents are 1–50 MiB. Enforced on both the streaming path (against the advertised `Content-Length`) and the buffered fallback. |
| `ONLYOFFICE_SAVE_MEMORY_BUDGET_MB` | `256` | Memory (MiB) budget for the **buffered fallback** save path. The fallback-concurrency cap is **derived** as `budget ÷ per-document`, so its worst case (`concurrency × per-document`) never exceeds the budget. Must be `>=` `ONLYOFFICE_MAX_DOCUMENT_MB` or the server refuses to start. With streaming saves (the normal path) this budget is rarely exercised. |

**Save path: streaming, with a bounded buffered fallback.** The save
callback relays the edited document from the Document Server straight to
object storage. The **primary path streams** the fetched body directly
into a presigned PUT (`storage.PutObjectStream`) — only a small fixed
copy buffer is ever in memory, regardless of document size — so
concurrent saves are **not** memory-bounded and need no up-front gate.
This is taken whenever the Document Server's cache download advertises a
`Content-Length` (its normal behaviour).

The editor-callback route runs outside the per-user / per-workspace rate
limiter (the Document Server holds no ZK Drive JWT). Only the **buffered
fallback** — used in the rare case the cache download omits
`Content-Length` (a chunked response) and the whole document must be
read into memory to learn its size — could OOM the API container under a
burst. That fallback alone is bounded by a counting semaphore whose size
is **derived** from the two knobs above:
`max_concurrent_buffered_saves = ONLYOFFICE_SAVE_MEMORY_BUDGET_MB ÷ ONLYOFFICE_MAX_DOCUMENT_MB`
(floored at 1). The defaults (`256 ÷ 100 = 2`) are sized for the 512 MiB
production API container in
[`deploy/docker-compose.prod.yml`](../deploy/docker-compose.prod.yml).
Excess **buffered-fallback** callbacks are shed with a retryable `503` —
the Document Server keeps the edited bytes in its cache and retries, so a
storm degrades gracefully instead of OOMing. Streaming saves are never
shed.

- **Throughput** is no longer gated for the streaming path; raising
  `ONLYOFFICE_SAVE_MEMORY_BUDGET_MB` only widens the rarely-used buffered
  fallback. Do **not** raise it past what the container can hold — the
  budget exists to keep `concurrency × per-document` inside the container.
- **Smaller documents** (lowering `ONLYOFFICE_MAX_DOCUMENT_MB`) raises
  fallback concurrency at the same budget, but rejects larger legitimate
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
