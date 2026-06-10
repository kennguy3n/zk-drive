# Operations Guide

This document covers everything an operator needs to deploy, monitor,
and maintain a ZK Drive installation: which binaries ship, how to
order rollouts, what `/metrics` exposes, how distributed tracing is
wired, how audit-log cold archival works, and the operator surface
for two-factor authentication, transactional email, and outbound
webhooks.

For the underlying configuration values, see
[`CONFIGURATION.md`](CONFIGURATION.md). For deployment topology and
component diagrams, see [`ARCHITECTURE.md`](ARCHITECTURE.md).

## Binaries

ZK Drive ships seven binaries from a single container image:

- **`/app/migrate`** — applies pending SQL migrations to the database
  and exits. Acquires a Postgres advisory lock keyed on a fixed
  64-bit constant so concurrent invocations (e.g. two Job pods
  during a blue/green deploy) serialise safely.
- **`/app/server`** — the HTTP API server. Refuses to start if
  migrations are out of date (the minimum required version is
  pinned by `internal/database.MinRequiredMigrationVersion`).
- **`/app/worker`** — the JetStream consumer / job runner. Same
  migration precondition as the server. Also drives the in-process
  storage-counter reconciler on `RECONCILE_INTERVAL_MINUTES`
  (default 60) and the in-process orphan-object GC on
  `GC_INTERVAL_MINUTES` (default 360).
- **`/app/reconciler`** — one-shot CronJob for storage-counter
  reconciliation. Deploys that prefer dedicated CronJob scheduling
  set the worker's `RECONCILE_INTERVAL_MINUTES=0` and run
  `/app/reconciler` externally.
- **`/app/orphan-gc`** — one-shot CronJob for orphan-object reclaim.
  Reclaims S3 objects from presigned PUTs that completed but were
  never confirmed. Deploys that prefer dedicated CronJob scheduling
  set the worker's `GC_INTERVAL_MINUTES=0` and run `/app/orphan-gc`
  externally.
- **`/app/audit-archiver`** — one-shot CronJob for audit-log cold
  archival. Exports `audit_log` rows older than
  `AUDIT_LOG_RETENTION_DAYS` to S3 as compressed JSONL (one object
  per workspace per calendar month) and deletes them from the hot
  table. Opt-in via `AUDIT_LOG_ARCHIVE_ENABLED`. Has its own
  migration precondition distinct from the server/worker baseline.
- **`/app/audit-restore`** — read-only CLI for the cold tier. Reads
  archived audit rows back for incident investigation and compliance
  requests. Loads configuration via `config.LoadStorageOnly` so it
  does not require `DATABASE_URL` or `JWT_SECRET` — only the S3
  group.

The migrate binary must run **before** the server and worker pods
are rolled out. On Kubernetes this is wired as a Job (see
`deploy/k8s/migrate-job.yaml`); on Compose as a
`service_completed_successfully` dependency
(`deploy/docker-compose.prod.yml`).

A manual one-off:

```
docker run --rm \
  -e DATABASE_URL=postgres://zkdrive:...@host:5432/zkdrive \
  -e JWT_SECRET=unused-but-required \
  ghcr.io/kennguy3n/zk-drive:<version> /app/migrate
```

## Deployment ordering

The webhook subject (`webhook.events`) and every other JetStream
subject ZK Drive uses is added to the shared `DRIVE_JOBS` stream by
the **worker** binary at startup. If a release that introduces a
new subject reaches the **server** first while the worker is still
on the previous version, the server publishes events to a subject
that is not yet in the stream's subject list. NATS rejects those
publishes with `no responders` and the events are lost (the
publisher logs the error and returns to the request path; the
underlying file / permission mutation has already committed).

Two correct deployment shapes:

1. **Worker-first (recommended for rolling deploys).** Roll the
   worker before the server. The worker updates `DRIVE_JOBS` to
   include the new subject at startup, and subsequent server
   publishes are accepted. This is the standard "infrastructure
   before producer" pattern.
2. **Single-shot.** Stop the server, deploy both binaries, start the
   worker, then the server. Same end state with no in-flight
   publishes during the window.

If you control deploys via Argo CD or Flux, configure sync waves so
the worker's `Deployment` sorts ahead of the server's. Helm users
can use `helm.sh/hook-weight` on the worker chart.

After a deploy where the ordering may have been violated, verify
the stream subject list against the cluster:

```
nats stream info DRIVE_JOBS
```

The subject list should include `webhook.events`. If it doesn't,
the worker hasn't rolled yet — restart it and the `ensureStream`
call adds the subject without losing the in-stream messages on
existing subjects.

## Preview pipeline scaling & per-tenant budgets

The preview pipeline (document thumbnail generation) is designed to
scale horizontally and to stay fair across tenants under load.

### Horizontal scaling

`DRIVE_JOBS` is a JetStream **work-queue** stream and every preview
consumer is **durable** (`drive-preview`, `drive-preview-priority`,
`drive-preview-standard`). Running N worker replicas therefore needs
no special configuration: each replica binds the same durable
consumers and JetStream load-balances delivery across all of them
(at-most-one delivery per message while it is in flight). To add
capacity, raise the worker `Deployment` replica count — there is no
leader election or sharding to configure. Within a single replica,
each preview subject is additionally fanned across a fixed goroutine
pool (see below), so one replica already renders multiple previews
concurrently.

### Priority queue (paid tiers first)

Preview jobs are routed onto two subjects by the originating
workspace's billing tier:

| Subject                            | Tiers                         | Worker pool (per replica)            |
| ---------------------------------- | ----------------------------- | ------------------------------------ |
| `drive.preview.generate.priority`  | `business`, `secure_business` | `PREVIEW_PRIORITY_WORKERS` (default 6) |
| `drive.preview.generate.standard`  | `free`, `starter`             | `PREVIEW_STANDARD_WORKERS` (default 2) |
| `drive.preview.generate` (legacy)  | un-routed / pre-upgrade jobs  | single inline goroutine (compat)     |

The priority pool is sized ~3× the standard pool so paid tiers get
proportionally more render concurrency when the queue is backed up.
Tier is resolved from `workspace_plans.tier`, cached in Redis under
`preview_tier:{workspace_id}` with a 5-minute TTL so a plan change
(e.g. a Stripe upgrade) takes effect within a few minutes without a
DB hit on every job. The legacy `drive.preview.generate` subject is
retained so previews enqueued by a not-yet-upgraded API server during
a rolling deploy are still processed.

### Per-tenant budget

To stop a single tenant (e.g. one bulk-importing thousands of files)
from monopolising the shared worker fleet, each workspace has a
**sliding-window** preview budget enforced via Redis:

- Key `preview_budget:{workspace_id}` (a sorted set of admission
  timestamps); admission is atomic via a Lua script.
- Default **100 previews / hour / workspace**, configurable via
  `PREVIEW_BUDGET_PER_WORKSPACE_HOUR`.
- When a workspace is over budget the job is **NAK'd with an
  exponential backoff** (15s, doubling, capped at 5 minutes) so it
  re-enters the queue and is rendered once the trailing-hour window
  drains — it is **not** dropped. The priority/standard consumers use
  a higher `MaxDeliver` (50) than the legacy consumer precisely so
  these legitimate budget deferrals are not terminated as if they were
  poison payloads.
- Each deferral increments `zkdrive_preview_budget_exceeded_total{tier}`.

The budget and tier cache are **only active when `REDIS_URL` is
set**. Without Redis (single-replica / local dev) the worker admits
every preview and routing falls back to the standard behaviour — the
budget is a fairness guard, never a correctness dependency, so a Redis
outage fails **open** rather than blocking previews.

**Suggested alert:** a sustained non-zero rate on
`zkdrive_preview_budget_exceeded_total` for a single `tier` indicates
either a tenant hammering uploads or an under-provisioned budget; if
it is a legitimate workload, raise `PREVIEW_BUDGET_PER_WORKSPACE_HOUR`
and/or add worker replicas.

### JWT key rotation: platform control plane (`keys:manage`)

`POST /api/platform/jwt/rotate` rotates the **fleet-wide** ES256 signing
key shared by every tenant: it mints a new active key, retires the
previous one, and reloads the in-memory key set (tokens signed by the
retired key keep verifying until they expire). The endpoint lives on the
**platform control plane** and is gated by the `keys:manage` capability
on a **platform API key** — it is *not* part of the per-workspace admin
API, so no individual workspace admin can rotate the fleet key. Mint a
platform API key with `keys:manage` (see the platform API-key section)
and call:

```
curl -X POST https://<host>/api/platform/jwt/rotate \
  -H "Authorization: Bearer <platform-api-key>"
```

The response carries only public key metadata (`key_id`, `algorithm`,
`signing_algorithm`, `created_at`) — never private key material.
`algorithm` is the rotated key's own algorithm (always `ES256`);
`signing_algorithm` is what the manager will actually sign with now, so
when `JWT_ALGORITHM=HS256` pins signing the response shows `algorithm:
ES256` (key stored + verifying) but `signing_algorithm: HS256` (not yet
used to sign), letting an operator confirm whether the rotation is live.
The rotation is recorded via
structured logs (`slog`: `"platform jwt signing key rotated"` with
`key_id` / `algorithm`); forward these to your log aggregator for an
audit trail, since the platform plane has no workspace scope and so does
not write to the per-workspace `audit_log` table.

> **Upgrade note (legacy `PLATFORM_ADMIN_USER_IDS`):** earlier releases
> gated rotation on the per-workspace admin endpoint
> `POST /api/admin/jwt/rotate` behind a `PLATFORM_ADMIN_USER_IDS`
> allowlist. That admin-API endpoint has been **removed** — rotation now
> only happens on the platform control plane above. `PLATFORM_ADMIN_USER_IDS`
> is no longer consulted; the server logs a startup warning if it is
> still set so you can drop it from your config.

## Observability

Every long-running binary exposes a Prometheus scrape surface:

| Binary             | Endpoint                                       | Default address                | Toggle                                                                                                                       |
| ------------------ | ---------------------------------------------- | ------------------------------ | ---------------------------------------------------------------------------------------------------------------------------- |
| `/app/server`      | `/metrics` on the main HTTP port               | `:8080` (via `LISTEN_ADDR`)    | always on                                                                                                                    |
| `/app/worker`      | `/metrics` on a dedicated port                 | `:9091` (via `WORKER_METRICS_ADDR`) | set `WORKER_METRICS_ADDR=off` or empty to disable                                                                            |
| `/app/reconciler`  | _none — one-shot_                              | n/a                            | K8s Job status is the alerting signal; the worker's in-process reconciler exports the same metric family                       |
| `/app/orphan-gc`   | _none — one-shot_                              | n/a                            | K8s Job status is the alerting signal; the worker's in-process GC loop exports the same metric family                          |
| `/app/audit-archiver` | _none — one-shot_                           | n/a                            | K8s Job status is the alerting signal                                                                                         |

Exported series (under the `zkdrive_` prefix):

- `zkdrive_http_requests_total{method, route, status}` — counter
- `zkdrive_http_request_duration_seconds{method, route}` — histogram
- `zkdrive_http_in_flight_requests` — gauge
- `zkdrive_worker_jobs_total{subject, result}` — counter (`result` ∈ `ok|skip|error|dropped`)
- `zkdrive_worker_job_duration_seconds{subject}` — histogram
- `zkdrive_preview_budget_exceeded_total{tier}` — counter; incremented each time a preview job is deferred because its workspace is over the per-tenant hourly budget (see [Preview pipeline scaling & per-tenant budgets](#preview-pipeline-scaling--per-tenant-budgets)). Labelled by billing `tier` (`free|starter|business|secure_business`) rather than `workspace_id` to keep cardinality bounded
- `zkdrive_reconciler_runs_total{result}` — counter
- `zkdrive_reconciler_workspaces_scanned_total` — counter
- `zkdrive_reconciler_workspaces_updated_total` — counter
- `zkdrive_reconciler_drift_bytes_total` — counter
- `zkdrive_reconciler_workspace_errors_total` — counter
- `zkdrive_reconciler_run_duration_seconds` — histogram
- `zkdrive_gc_runs_total{result}` — counter (`result` ∈ `ok|error|cancelled`)
- `zkdrive_gc_workspaces_scanned_total` — counter
- `zkdrive_gc_orphans_found_total` — counter
- `zkdrive_gc_orphans_deleted_total` — counter
- `zkdrive_gc_objects_deleted_total` — counter
- `zkdrive_gc_workspace_errors_total` — counter
- `zkdrive_gc_run_duration_seconds` — histogram
- `zkdrive_audit_archive_runs_total{result}` — counter (`result` ∈ `ok|partial|error|cancelled`)
- `zkdrive_audit_archive_buckets_total{result}` — counter per `(workspace, year-month)`
- `zkdrive_audit_archive_rows_total` — counter
- `zkdrive_audit_archive_bytes_total` — counter
- `zkdrive_audit_archive_run_duration_seconds` — histogram
- `zkdrive_email_sent_total{template, outcome}` — counter
- `zkdrive_webhook_deliveries_total{outcome}` — counter
- `zkdrive_db_pool_*` — pgxpool live stats (total / acquired / idle / max / acquire count / acquire duration)
- `zkdrive_redis_pool_*` — go-redis client pool stats (server only)
- `go_*` / `process_*` — runtime + process collectors from `prometheus/client_golang`

Example Prometheus scrape config:

```yaml
scrape_configs:
  - job_name: zk-drive-server
    metrics_path: /metrics
    static_configs:
      - targets: ['zk-drive-server:8080']
  - job_name: zk-drive-worker
    metrics_path: /metrics
    static_configs:
      - targets: ['zk-drive-worker:9091']
```

The `/metrics` endpoint is intentionally **unauthenticated**: the
Go runtime and pool collectors expose modest internal state
appropriate for an operator metrics network but not for the public
internet. Production deployments MUST firewall `/metrics` off via a
Network Policy or ingress allow-list. Splitting metrics onto a
separate port (the worker's default at `:9091` is the model) is the
simplest posture.

## NoOps: health dashboard, setup wizard & auto-healing

Prometheus and OpenTelemetry are the right tools for a team with an
observability stack. The NoOps surface below is for the SME operator
who has neither: it answers "is my deployment healthy?" and "how do I
configure it?" from the product itself, with no external tooling.

### Health dashboard (`GET /api/admin/health-dashboard`)

Requires the `admin` role. Returns a single JSON document with an
overall traffic-light `status` (`green` / `yellow` / `red` / `unknown`)
plus one entry per subsystem, each with its own colour and a short
human-readable detail. Subsystems probed:

| Subsystem    | Green                                              | Yellow                                                       | Red                                  |
| ------------ | -------------------------------------------------- | ------------------------------------------------------------ | ------------------------------------ |
| `postgres`   | pool reachable; live pool stats reported           | pool near exhaustion                                         | unreachable                          |
| `redis`      | connected; memory usage reported                   | not configured (in-memory fallback active)                  | configured but unreachable           |
| `nats`       | connected; per-subject stream depth reported       | reconnecting (exponential backoff in progress)              | disconnected                         |
| `clamav`     | connected; definition date reported                | not configured / auto-disabled (scanning skipped)          | configured but unreachable           |
| `onlyoffice` | connected                                          | not configured (collaborative editing off)                  | configured but unreachable           |
| `fabric` / S3| connected; recent error rate low                   | elevated recent error rate                                  | unreachable                          |
| `workers`    | every worker type beat within `StaleAfter` (45s)   | a worker type is stale or reports `degraded`                | a worker type silent past `DeadAfter` (120s) |

Each probe runs under a bounded timeout and recovers from panics, so a
single wedged dependency degrades only its own row — the endpoint
always returns. The frontend renders this at **Admin → Health** as a
traffic-light grid that auto-refreshes every 15s.

Worker liveness is **pull-based**: the worker fleet upserts a row per
logical worker type into `worker_heartbeats` (migration 039) every 15s,
and the dashboard reads the freshest row per type. This deliberately
does not use a NATS request/reply so the dashboard can still report
"no workers have checked in" when the message bus itself is the outage.

### Guided setup wizard (`/api/setup/*`)

A fresh box (no workspaces) routes anonymous visitors to a five-step
wizard: admin account → storage (with a **Test Connection** button) →
optional services → first workspace → first invite.

| Method | Path                      | Auth          | Purpose                                                                                 |
| ------ | ------------------------- | ------------- | --------------------------------------------------------------------------------------- |
| `GET`  | `/api/setup/status`       | none          | What is configured and what is missing. Returns full step detail only while incomplete; once complete it returns just `setup_completed` + `completed_at` so a provisioned (possibly internet-exposed) box does not leak its deployment shape to anonymous callers. |
| `POST` | `/api/setup/test-storage` | none, gated   | Validates an S3 endpoint/bucket/key set with a bounded (8s) probe. **Refuses with `403` once setup is complete** so it cannot be abused as an SSRF primitive on a live box. |
| `POST` | `/api/setup/complete`     | `admin`       | Flips the `setup_state` singleton (migration 041) to completed; idempotent and preserves the original `completed_at`. |

`setup_state` is a single-row table (`id BOOLEAN PRIMARY KEY` pinned
`TRUE` with a `CHECK` constraint) so completion is atomic and there can
never be two conflicting rows.

### Auto-healing

The system self-recovers from dependency outages without operator
intervention; each transition is logged once at `warn` (degrade) and
`info` (recover) so the event is visible without flooding the log:

- **ClamAV down (worker):** virus scanning auto-disables, the scan
  worker flips its heartbeat to `degraded`, in-flight scans are skipped
  and ACKed (no Nak-loop), and a 60s health loop re-enables scanning the
  moment clamd answers again. The availability flag has a **single
  writer** — the dedicated health-loop ping. The scan handler does *not*
  flip it on a failed dial: a single failed dial is not proof clamd is
  down (it can be a transient blip), and disabling scanning on one
  failure would mark every upload in the window `clean` *without
  scanning it* — a worse failure mode for a security product than a few
  bounded Nak-redeliveries. So when clamd drops between probes, scan
  jobs Nak and redeliver (real scanning is still attempted) for up to
  the 60s probe cadence until the next ping confirms the outage and
  switches to skip-and-pass; recovery is likewise gated on a confirming
  ping. The degraded state is visible on the health dashboard throughout.
- **NATS down (worker):** reconnect uses exponential backoff with jitter
  (1s → 2s → … → 30s cap) so a fleet does not reconnect in lockstep.
- **Poison preview job:** after `PreviewMaxAttempts` (3) consecutive
  failures the version is marked `preview_failed` (migration 040) and the
  message is ACKed, instead of redelivering until the stream's MaxAge.
- **Redis down (server):** the session store and rate limiter fall back
  seamlessly to a per-replica in-memory store and recover automatically
  when Redis returns. While degraded, revocation is per-replica — the
  correct availability-over-consistency trade for a transient outage,
  and identical to healthy behaviour on the single-node SME profile.
  Both auth gates degrade **open** and consistently: the per-user
  revocation cutoff (`IsRevoked`) and the device-aware session check
  (`ValidateSession`) admit a request whose session predates the outage
  (it lives only in the unreachable Redis) rather than 401-ing it, so a
  Redis blip does not force the whole fleet to re-login. The request is
  still gated by the JWT signature/expiry; a force-sign-out issued
  during the outage is replayed into Redis on recovery; and a pre-outage
  per-session revocation is re-enforced automatically the instant Redis
  returns (its hash is still absent there). Sessions created mid-outage
  remain fully device-bound.

## Distributed tracing

Both `/app/server` and `/app/worker` emit OpenTelemetry spans over
**OTLP/HTTP** to whatever collector / backend you point them at —
Jaeger, Grafana Tempo, Honeycomb, Datadog (via the OTLP gateway),
New Relic, SigNoz, or an in-cluster OTel Collector that fans out to
multiple backends.

The instrumentation covers chi HTTP handlers (named by route
pattern, not raw path, so cardinality stays bounded), pgxpool
queries, redis commands, NATS JetStream publish + consume (parent /
child linked across the message boundary), and SMTP sends — every
blocking dependency a request can hit.

**Trace ↔ log correlation.** When tracing is enabled, every
access-log line and every handler-emitted slog record carries
`trace_id` and `span_id` fields alongside the existing
`request_id`. In any backend that supports the link (Honeycomb,
Datadog, Grafana Loki + Tempo, Splunk Observability) you can click
a slog record and jump straight to the trace, or click a span and
jump to the related log lines.

**Disabled-but-instrumented.** Leaving
`OTEL_EXPORTER_OTLP_ENDPOINT` unset installs a no-op tracer
provider. All instrumented code paths still compile and execute, so
flipping the env var on a running pod is sufficient to start
emitting spans on the next request without any code changes.

**NATS server version requirement.** When tracing is enabled, the
publisher (server) injects the W3C `traceparent` (and optional
`tracestate` / `baggage`) header onto every JetStream message it
publishes so the consumer (worker) can recreate the parent-child
span link. NATS server **2.2 or newer** is required to accept
messages with headers. The JetStream features ZK Drive already uses
(subjects, durable consumers, ack policies) require 2.3+ in any
case, so this is consistent with the existing minimum version.

Quick example — point a local server at the all-in-one Grafana
Tempo container for end-to-end testing:

```
docker run -d --name=tempo \
  -p 3200:3200 -p 4318:4318 \
  grafana/tempo:latest \
  -config.file=/etc/tempo.yaml -target=all

OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 \
OTEL_TRACES_SAMPLER_ARG=1.0 \
OTEL_DEPLOYMENT_ENVIRONMENT=dev \
./server
```

## Two-factor authentication (TOTP)

ZK Drive ships a built-in RFC 6238 TOTP second factor. It is opt-in
per user from the account-settings page, and opt-in per workspace
via an admin toggle that forces every member to enrol before
completing login.

**Encryption at rest.** TOTP secrets are stored encrypted with
AES-256-GCM keyed via `CREDENTIAL_ENCRYPTION_KEY` — the same codec
that protects per-tenant storage credentials. Operators rotating
the key already have the runbook from the credential-encryption
rotation procedure; TOTP rides on the same key.

**Recovery codes.** Ten codes are generated at enrolment finalise,
shown to the user exactly once, then bcrypt-hashed (cost 12) before
commit. Codes are normalised to lowercase and dash-separated on
input, so the user can type `XB-4Q-9Z-PM-TK`, `xb4q9zpmtk`, or
`xb 4q 9z pm tk` interchangeably. Burning a code marks `used_at`
(we never delete the row) so audit queries can prove a recovery
code was the second factor on a given session. If the user runs low
(≤ 2 remaining) the account-settings page warns them; the only way
to mint a fresh set is to Disable and re-enrol.

**Replay protection.** Each successful Verify stamps
`user_totp_credentials.last_used_at` with the accepted code's 30 s
period boundary. The verifier rejects any subsequent code whose
period start is `<=` `last_used_at`, so a code observed by a MITM
within its 30 s window cannot be replayed.

**Workspace policy.** Admins flip `workspaces.mfa_required` via
`PATCH /api/admin/workspace/mfa-policy`. The transition is audited
(`auth.mfa_policy_change`). When the policy is on, a user without
an enrolled credential receives a `purpose=mfa_enroll` token at
login that authorises only the enrolment endpoints — the user
cannot reach any data-plane handler until enrolment is finalised.
Disabling the policy does not delete any user's enrolled
credential; that user remains protected by their second factor, but
new users are no longer forced to enrol.

**Audit trail.** Five events are logged with the standard audit
shape (workspace, actor, request IP / UA): `auth.mfa_enroll`,
`auth.mfa_verify`, `auth.mfa_recovery_use`, `auth.mfa_disable`,
`auth.mfa_policy_change`. The `auth.login` event also records
`factor=totp` or `factor=recovery_code` so an investigator can
distinguish the two paths.

**Lost authenticator.** The user should burn a recovery code on
their next login (it goes through the same Verify path as a TOTP
code). After signing in they can Disable 2FA from settings (the
disable endpoint re-verifies the password to prevent a stolen
session token from quietly downgrading their auth posture), then
re-enrol a new authenticator and receive a fresh recovery set. If
the user has lost both the authenticator and all recovery codes, a
workspace admin can use `PATCH /api/admin/users/{id}` to deactivate
and re-activate the account (manual identity-proofing process —
deliberately not a one-click button on the admin page).

## SMTP provider notes

When email is enabled (all three of `PUBLIC_URL`, `SMTP_HOST`,
`SMTP_FROM_ADDRESS` are set), guest invites are delivered as
templated HTML / text messages. Delivery is best-effort: a relay
outage does **not** roll back the invite row. The HTTP response is
the same either way (`201 Created`).

Outcomes per attempt are recorded under the
`sharing.guest_invite_emailed` audit action with one of `ok`,
`smtp_error`, `template_error`, `address_invalid`, `disabled`. Use
the audit log to surface undelivered invites in compliance reports.
Metrics (`zkdrive_email_sent_total{template, outcome}`) carry the
same shape with bounded cardinality.

Tested provider settings:

- **Postmark**: `SMTP_HOST=smtp.postmarkapp.com`, `SMTP_PORT=587`,
  username and password are both the server token. Standard
  STARTTLS.
- **AWS SES**: `SMTP_HOST=email-smtp.<region>.amazonaws.com`,
  `SMTP_PORT=587`. Credentials are SES-specific SMTP credentials
  (generated via the SES console), **not** IAM access keys.
- **Mailgun**: `SMTP_HOST=smtp.mailgun.org`, `SMTP_PORT=587`.
  Username is `postmaster@<domain>`, password is the SMTP password
  from the domain settings page.
- **Gmail App Passwords**: `SMTP_HOST=smtp.gmail.com`,
  `SMTP_PORT=587`. Username is the full Gmail address, password is
  the 16-char app password. Low-volume internal tooling only — not
  customer-facing transactional mail.

The email links recipients to `{PUBLIC_URL}/invites/{invite_id}`.
The frontend then hits `GET /api/guest-invites/{id}/preview` (public,
no auth required — same RLS-bypass posture as
`/api/share-links/{token}`) to fetch the display-safe projection
(workspace name, folder name, recipient email, role, expiry).
Secrets such as the permission row ID and the inviter user ID are
not exposed; invite IDs are UUIDv4 so guessing is infeasible.

## Audit-log cold archival

ZK Drive's `audit_log` table records security-sensitive events
(login, SSO link, MFA lifecycle, permission grant/revoke, admin
user management, workspace settings changes, retention policy
changes, billing changes, guest-invite delivery outcomes). For
compliance reporting (SOC2 Type II, HIPAA, GDPR) operators
typically need a retention window of 1 – 6 years — well beyond what
the hot Postgres table should carry.

The `audit-archiver` binary is a one-shot CronJob that:

1. Selects audit rows older than `AUDIT_LOG_RETENTION_DAYS`.
2. Groups them by `(workspace_id, year-month)`.
3. For each bucket, uploads the rows to S3 as compressed JSONL at
   `{prefix}{workspace_id}/{YYYY-MM}/{batch_id}.jsonl.gz`. A single
   `(workspace, month)` bucket that exceeds
   `AUDIT_LOG_ARCHIVE_MAX_ROWS_PER_BATCH` is split into multiple
   pages, each with its own `batch_id` UUID so pages within the
   same bucket never overwrite each other in S3.
4. Records each batch in `audit_log_archive_runs` (`run_id`,
   `workspace_id`, `cutoff_time`, `year_month`,
   `archive_object_key`, `rows_archived`, `bytes_uploaded`,
   `started_at`, `completed_at`, `error_message`).
5. Deletes the archived rows from the hot table.

The archiver is **idempotent**: a crash between steps 3 – 5 leaves
the S3 object plus the `audit_log_archive_runs` row committed but
the rows still in the hot tier. The next run re-uploads the same
rows under a fresh `batch_id`-suffixed key; the cold tier may carry
duplicate objects which the restore CLI dedupes by row id.

The K8s CronJob ships in `deploy/k8s/audit-archiver-cronjob.yaml`
with a nightly schedule (`47 3 * * *`) staggered against the
reconciler (`17 */1 * * *`) and orphan-gc (`37 */6 * * *`) so
Postgres and S3 are not hit by all three background jobs at the
same minute. Skip a missed tick rather than overlap
(`concurrencyPolicy: Forbid`); `activeDeadlineSeconds: 14400` caps
a single run at four hours to match the histogram's top bucket.

### Restore workflow

The `audit-restore` binary reads archived rows back for incident
investigation. It is read-only — no S3 PUT, no Postgres DELETE — so
operators can run it freely against any historical period.

```
docker run --rm \
  -e S3_ENDPOINT=https://... \
  -e S3_ACCESS_KEY=... \
  -e S3_SECRET_KEY=... \
  -e S3_BUCKET=zk-drive-prod \
  -e AUDIT_LOG_ARCHIVE_BUCKET=zk-drive-audit-archive \
  -e AUDIT_LOG_ARCHIVE_PREFIX=audit-archive/ \
  ghcr.io/kennguy3n/zk-drive:<version> /app/audit-restore \
    --workspace 00000000-1111-2222-3333-444444444444 \
    --from 2024-01-01T00:00:00Z \
    --to   2024-03-31T23:59:59Z \
    --output /tmp/workspace-audit-q1-2024.jsonl
```

`audit-restore` does **not** require `DATABASE_URL` or `JWT_SECRET`
— on-call engineers responding to an incident or compliance request
can run it with only S3 credentials in hand.

The output is newline-delimited JSON, one row per audit event,
sorted chronologically (`created_at` ASC), deduplicated by row id.
Pipe into `jq` for ad-hoc analysis:

```
jq -r 'select(.action | startswith("admin.")) | [.created_at, .actor_id, .action] | @tsv' \
  workspace-audit-q1-2024.jsonl
```

### S3 object layout

```
{AUDIT_LOG_ARCHIVE_PREFIX}{workspace_id}/{YYYY-MM}/{batch_id}.jsonl.gz

audit-archive/00000000-1111-2222-3333-444444444444/2024-01/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.jsonl.gz
audit-archive/00000000-1111-2222-3333-444444444444/2024-01/ffffffff-1111-2222-3333-444444444444.jsonl.gz   # duplicate from a crashed earlier run
audit-archive/00000000-1111-2222-3333-444444444444/2024-02/cccccccc-dddd-eeee-ffff-000000000000.jsonl.gz
```

Each object is GZIP-compressed JSONL. The `batch_id` UUID suffix
makes every PUT idempotent (the same row content uploaded twice
produces two distinct objects; the restore tool dedupes by row id).

## Outbound webhooks

Workspace admins can register HTTPS endpoints that receive
notifications when state changes inside the workspace. The
subscriber's URL is hit with a JSON `POST`; payloads are signed
with HMAC-SHA256 so the subscriber can confirm the request came
from ZK Drive. Subscriptions live in `webhook_subscriptions` (one
per `(workspace, URL, event_type)` triple); every attempt is
recorded in `webhook_deliveries` for replay and debugging.

### Event catalogue

Each event ships as a versioned envelope. The `data` payload
differs by event type, but the envelope is stable:

```
{
  "id": "<event uuid>",          // stable across retries; subscribers de-dupe on this
  "type": "file.upload.confirmed",
  "workspace_id": "<workspace uuid>",
  "actor_id": "<user uuid|null>",
  "created_at": "2026-05-21T23:24:11.123456Z",
  "data": { /* event-type-specific payload */ }
}
```

| Event type              | When it fires                                                  | `data` fields                                                  |
| ----------------------- | -------------------------------------------------------------- | -------------------------------------------------------------- |
| `file.upload.confirmed` | A presigned upload completes and the file row is committed     | `file_id`, `version_id`, `folder_id`, `name`, `mime_type`, `size_bytes` |
| `file.deleted`          | A file is soft-deleted via API or bulk-delete                  | `file_id`, `folder_id`, `name`                                 |
| `permission.granted`    | A file or folder permission is added                           | `resource_type`, `resource_id`, `grantee_id`, `role`           |
| `permission.revoked`    | A file or folder permission is removed                         | `resource_type`, `resource_id`, `grantee_id`, `role`           |
| `member.joined`         | An admin invites a new user to the workspace                   | `user_id`, `email`, `role`                                     |
| `member.removed`        | A user is deactivated from the workspace                       | `user_id`, `email`, `role`                                     |

### Request headers

| Header                  | Purpose                                                                                              |
| ----------------------- | ---------------------------------------------------------------------------------------------------- |
| `Content-Type`          | Always `application/json; charset=utf-8`.                                                            |
| `User-Agent`            | `zk-drive-webhooks/<version>` (the server build version; `zk-drive-webhooks/dev` for unsemvered builds). Subscribers should match on the prefix only in reverse-proxy / WAF rules. |
| `X-ZkDrive-Signature`   | `t=<unix>,v1=<hex>` HMAC-SHA256 over `<unix>.<body>` keyed on the subscription secret.                |
| `X-ZkDrive-Event-Id`    | Stable across retries. Subscribers de-dupe on this.                                                  |
| `X-ZkDrive-Event-Type`  | Same as `type` in the body — lets a subscriber route to a per-type handler without parsing the body. |
| `X-ZkDrive-Delivery-Id` | Unique per attempt. Useful for cross-referencing the per-attempt row in `/api/admin/webhooks/{id}/deliveries`. |

### Verifying the signature

The signature is the canonical Stripe-style scheme. Pseudocode for
a subscriber:

```python
import hmac, hashlib, time

def verify(secret: str, header: str, body: bytes) -> bool:
    parts = dict(p.split("=", 1) for p in header.split(",") if "=" in p)
    ts = int(parts["t"])
    if abs(time.time() - ts) > 300:        # 5-minute window
        return False
    expected = hmac.new(secret.encode(), f"{ts}.".encode() + body,
                        hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, parts["v1"])
```

The 5-minute window matches Stripe's default and is enforced by
the reference `Verify` implementation in
`internal/webhooks/signer.go`.

### Reliability semantics

- **At-least-once delivery.** Subscribers MUST be idempotent on
  `X-ZkDrive-Event-Id` — a network blip can produce a duplicate
  delivery. The signature does not prove uniqueness; the event id
  does.
- **Retries: 5 attempts total** (initial + 4) on the schedule
  `0s, 1s, 2s, 4s, 8s`. Non-2xx responses, network errors, and
  `outcome=blocked` all retry; only 2xx is treated as success.
- **Auto-pause after 50 consecutive failures.** A persistently
  broken endpoint is paused (admin sees `auto_paused_at` on the
  subscription). Re-enable via
  `POST /api/admin/webhooks/{id}/resume`; the consecutive-failure
  counter resets to zero on resume.
- **Per-workspace cap: 20 active subscriptions.** A request that
  would push the workspace over the cap returns `409 Conflict`.
- **SSRF defence.** URLs are syntactically validated (HTTPS only in
  production, no userinfo) and the host is resolved at every
  delivery attempt to defend against DNS rebinding. Any IP in
  loopback, RFC1918, link-local, RFC6598, IPv6 ULA, multicast, or
  the documentation ranges is rejected with `outcome=blocked` and
  no request is sent.

### Admin REST surface

All endpoints sit under `/api/admin/webhooks` and require the
`admin` role on the workspace.

| Method   | Path                                  | Purpose                                                        |
| -------- | ------------------------------------- | -------------------------------------------------------------- |
| `POST`   | `/api/admin/webhooks`                 | Create a subscription. Response includes the secret **once**.  |
| `GET`    | `/api/admin/webhooks`                 | List subscriptions (secrets redacted).                         |
| `GET`    | `/api/admin/webhooks/{id}`            | Get a single subscription (secret redacted).                   |
| `DELETE` | `/api/admin/webhooks/{id}`            | Hard-delete a subscription.                                    |
| `GET`    | `/api/admin/webhooks/{id}/deliveries` | List recent attempt rows for the subscription.                 |
| `POST`   | `/api/admin/webhooks/{id}/test`       | Enqueue a synthetic event to verify connectivity end-to-end.   |
| `POST`   | `/api/admin/webhooks/{id}/resume`     | Re-activate an auto-paused subscription.                       |

## Searchable file types

The search endpoint (`GET /api/search?q=...`) queries Postgres
full-text search over file names, tag lists, and file body content
(`files.content_text`). The index worker (`drive.search.index`
subject) populates `content_text` after each successful upload. The
extractor in `internal/index` supports:

| Mime type                                                                          | Extractor    | Notes                                                                                                                                                                                                                              |
| ---------------------------------------------------------------------------------- | ------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `text/*`                                                                           | pass-through | Body bytes are written verbatim (UTF-8 truncated to 4 MiB on a rune boundary).                                                                                                                                                     |
| `application/json`, `application/xml`                                              | pass-through | Same as `text/*`.                                                                                                                                                                                                                  |
| `application/pdf`                                                                  | `pdftotext`  | Shells out to `pdftotext` (part of `poppler-utils`, GPL — used as a subprocess, not linked). Same package the preview pipeline requires for `pdftoppm`, so a host with PDF previews enabled also has PDF text extraction.           |
| `application/vnd.openxmlformats-officedocument.wordprocessingml.document` (.docx)  | pure Go      | `archive/zip` opens the .docx, `encoding/xml` streams `word/document.xml`, and visible `<w:t>` text runs are concatenated with `<w:tab>` / `<w:br>` rendered as `\t` / `\n` and `<w:p>` paragraphs separated by newlines.            |

Files of any other mime type (images, video, archives, binaries)
are still searchable by **name** and **tag** — the index worker
acks the message with no body content recorded. The same holds for
strict zero-knowledge folders: their bytes never reach the server,
so `content_text` stays empty regardless of mime type.

**Graceful skip.** If `pdftotext` is not installed on the worker
host (e.g. operators who strip `poppler-utils` from a custom
image), PDF extraction returns the same "unsupported mime" signal
as an image upload — the message is acked and the file remains
searchable by name. The official ZK Drive Dockerfile installs
`poppler-utils` so both extractors and previews work out of the
box.

**Limits.** The extractor reads at most 4 MiB of text per file
(`MaxIndexBytes`). DOCX archives are also bounded at 64 MiB
uncompressed XML (`docxMaxUncompressedBytes`) to defend against
zip-bomb inputs.
