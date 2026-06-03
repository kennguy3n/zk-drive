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
`/api/documents/{id}/ws`) are a known limitation: they authenticate the
upgrade request but deliberately sit outside the per-request middleware
stack (`TenantGuard` + the IP-allowlist middleware), because that stack
assumes ordinary request/response semantics and would otherwise charge
per WS frame. As a result the IP allowlist is **not** applied to
WebSocket traffic today. The practical exposure is bounded: a client
must already hold a valid session (JWT) to open a socket, and all
allowlist-gated REST endpoints — including the upload-URL / confirm
handshakes used to move file bytes — remain enforced. If you require
strict connection-time IP enforcement for real-time editing, terminate
WebSocket traffic at a proxy that applies the same CIDR allowlist.

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
