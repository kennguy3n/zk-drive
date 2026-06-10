package config

import (
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Config holds runtime configuration for the zk-drive server and worker
// binaries. All values are sourced from environment variables so deployments
// can inject them uniformly.
type Config struct {
	DatabaseURL string
	JWTSecret   string

	// DatabaseReadURL is an optional DSN for a Postgres read replica
	// (or a PgBouncer read pool). When set and distinct from
	// DatabaseURL, internal/database.ConnectReadWrite opens a second
	// pgxpool against it and the ReadWriteSplitter routes SELECT-family
	// statements there while every mutation and transaction stays on the
	// primary (DatabaseURL). Sourced from DATABASE_READ_URL; empty means
	// "no replica" (reads use the primary). See docs/CONFIGURATION.md and
	// deploy/POSTGRES_SCALING.md.
	DatabaseReadURL string

	// DB connection-pool sizing. These tune the pgxpool created by
	// internal/database.ConnectWithPool. Sourced from DB_MAX_CONNS,
	// DB_MIN_CONNS, and DB_MAX_CONN_IDLE_TIME. DBMaxConns is clamped
	// to [2, 200] and DBMinConns to [0, DBMaxConns] at load time so a
	// fat-fingered env var cannot starve the pool (max < min) or
	// exhaust Postgres' max_connections.
	DBMaxConns        int32
	DBMinConns        int32
	DBMaxConnIdleTime time.Duration

	// JWTAlgorithm selects the session-token signing algorithm:
	//   - "auto": sign with ES256 when an active asymmetric
	//     signing key exists in jwt_signing_keys, otherwise fall back
	//     to HS256 using JWTSecret. Verification always accepts both.
	//   - "ES256": force ES256 signing (still verifies HS256 tokens
	//     issued before the cutover so existing sessions survive).
	//   - "HS256": force HS256 signing (legacy behaviour).
	// Parsed case-insensitively. The default depends on Profile:
	// "ES256" under the production profile (asymmetric-only signing
	// is mandatory there), "auto" otherwise. An explicit JWT_ALGORITHM
	// always wins over the profile default. Unrecognised values fall
	// back to the profile default.
	JWTAlgorithm string

	// JWTKeyRefreshInterval is how often each replica re-reads the
	// jwt_signing_keys table so that a key rotation performed on one
	// replica (POST /api/platform/jwt/rotate) propagates to all others
	// without a restart. Sourced from JWT_KEY_REFRESH_INTERVAL and
	// clamped to [10s, 1h]; a non-positive value disables the
	// background refresh (single-replica deployments). Default 60s.
	JWTKeyRefreshInterval time.Duration

	// PlatformAdminUserIDs is LEGACY and no longer gates anything. It
	// once narrowed the per-workspace admin JWT-rotation endpoint to
	// designated platform operators, but rotation has since moved to the
	// platform control plane (POST /api/platform/jwt/rotate, gated by the
	// keys:manage platform-API-key capability) and the admin endpoint was
	// removed. The value is still parsed from PLATFORM_ADMIN_USER_IDS
	// solely so cmd/server can emit a startup warning when it is set,
	// nudging operators to drop it from their config.
	PlatformAdminUserIDs []uuid.UUID

	// PlatformAdminUserIDsInvalid holds the raw PLATFORM_ADMIN_USER_IDS
	// entries that failed to parse as UUIDs. They are dropped from the
	// allowlist (a malformed entry can only narrow access, never widen
	// it) but retained here so cmd/server can warn the operator at
	// startup — otherwise a typo'd UUID would silently exclude an
	// intended platform admin with no signal.
	PlatformAdminUserIDsInvalid []string

	ListenAddr    string
	S3Endpoint    string
	S3Bucket      string
	S3AccessKey   string
	S3SecretKey   string
	MigrationsDir string
	NATSURL       string
	ClamAVAddress string

	// SSO — optional, Business-tier feature. When the client id is
	// empty for a provider, the corresponding /api/auth/oauth/{provider}
	// routes return 501 Not Implemented so the rest of the server still
	// boots without credentials.
	GoogleClientID        string
	GoogleClientSecret    string
	GoogleRedirectURL     string
	MicrosoftClientID     string
	MicrosoftClientSecret string
	MicrosoftRedirectURL  string

	// iam-core (uneycom/iam-core) OAuth2/OIDC identity provider.
	// OPTIONAL: when IAMCoreIssuerURL is set the server delegates
	// authentication to iam-core (the built-in auth stack — password
	// login, internal sessions, internal TOTP — is bypassed); when it
	// is empty the server falls back to the built-in auth stack so
	// dev/demo deployments work without an external IdP. Parsed from
	// the IAM_CORE_* env vars and surfaced to the frontend via
	// GET /api/config. See internal/iamcore and docs/IAM_CORE.md.
	IAMCoreIssuerURL    string
	IAMCoreClientID     string
	IAMCoreClientSecret string
	IAMCoreAudience     string
	IAMCoreScopes       []string
	IAMCoreCallbackURL  string

	// Rate limiting — applied per (workspace_id, user_id) via an
	// in-memory token bucket. Values <= 0 fall back to the defaults
	// declared alongside the middleware so misconfigured env vars do
	// not accidentally disable rate limiting entirely.
	RateLimitPerUser      int
	RateLimitPerWorkspace int

	// Auth brute-force reputation — per-IP failed-sign-in tracking
	// that escalates a cooldown after repeated failures (6.3). After
	// AuthFailureThreshold failed attempts from one client IP the
	// guard applies progressively longer cooldowns (1s, 5s, 30s,
	// then a hard block of AuthBlockDuration) before the next attempt
	// is accepted, and the reputation counter is retained for
	// AuthReputationRetention. All three fall back to their defaults
	// (declared alongside the middleware) when unset or <= 0. Sourced
	// from AUTH_FAILURE_THRESHOLD, AUTH_BLOCK_DURATION,
	// AUTH_REPUTATION_RETENTION.
	AuthFailureThreshold    int
	AuthBlockDuration       time.Duration
	AuthReputationRetention time.Duration

	// TrustedProxyDepth is the number of trusted reverse proxies in
	// front of the server. It governs how the IP-allowlist
	// middleware resolves the client IP from X-Forwarded-For: the
	// real client address is taken TrustedProxyDepth entries from
	// the right of the header (entries further left are
	// client-supplied and spoofable). Sourced from
	// TRUSTED_PROXY_DEPTH; defaults to defaultTrustedProxyDepth
	// (single load balancer).
	TrustedProxyDepth int

	// Preview pipeline scaling + per-tenant fairness. The budget is a
	// Redis-backed sliding-window limit on previews generated per
	// workspace per hour so one tenant bulk-uploading cannot starve
	// the shared worker fleet; the priority / standard worker counts
	// size the two goroutine pools the worker fans preview jobs
	// across (paid tiers get the larger priority pool). All three
	// fall back to their defaults when unset or <= 0.
	PreviewBudgetPerWorkspaceHour int
	PreviewPriorityWorkers        int
	PreviewStandardWorkers        int

	// Preview worker fleet tiering (workstream 5.4). Preview jobs are
	// routed by renderer weight to two pod tiers: the slim "lightweight"
	// pods run pure-Go renderers, the "heavy" pods run subprocess
	// renderers (LibreOffice / FFmpeg / ImageMagick / poppler / librsvg).
	//
	//   PreviewLightweightWorkers / PreviewHeavyWorkers size the
	//   goroutine pool each tier's consumer fans deliveries across. Set
	//   the tier a pod should NOT serve to 0: a slim pod runs with
	//   PREVIEW_HEAVY_WORKERS=0 (subscribes only lightweight) and a heavy
	//   pod runs with PREVIEW_LIGHTWEIGHT_WORKERS=0. Both default > 0 so
	//   a single all-in-one deployment renders everything out of the box.
	//
	//   PreviewWorkerConcurrency caps concurrent subprocess renders per
	//   pod (LibreOffice is single-threaded and memory-hungry, so an
	//   unbounded fan-out OOM-kills the pod). 0 means unlimited.
	//
	//   PreviewHeavyQueueBackpressureThreshold is the heavy-queue depth
	//   (pending + unacked) at which the API defers new heavy preview
	//   jobs instead of enqueuing them, returning a "generating…"
	//   placeholder. 0 disables backpressure.
	PreviewLightweightWorkers              int
	PreviewHeavyWorkers                    int
	PreviewWorkerConcurrency               int
	PreviewHeavyQueueBackpressureThreshold int

	// RedisURL switches the rate limiter and session store from
	// in-memory state to a Redis-backed implementation so limits and
	// session revocation work across replicas. When empty, the
	// in-memory implementations are used (single-replica behaviour).
	RedisURL string

	// WSProxyMode delegates WebSocket fan-out to an external connection
	// proxy tier (Centrifugo / Pusher) instead of terminating WS
	// connections on the API pods. Sourced from WS_PROXY_MODE (bool).
	//
	// When true, the server:
	//   - publishes every real-time event to Redis pub/sub (ws:* channels)
	//     as the egress the external proxy consumes, and does NOT run the
	//     in-process Redis→hub subscribe loop (the proxy, not this
	//     process, holds the client connections);
	//   - serves /api/ws with 501 Not Implemented so a misconfigured
	//     client that still dials the API directly fails loudly rather
	//     than opening a connection the proxy will never feed.
	//
	// Requires REDIS_URL (the proxy and the API communicate through
	// Redis). When WS_PROXY_MODE is set but REDIS_URL is empty the server
	// logs a warning and falls back to in-process WS so a fat-fingered
	// rollout degrades to the single-process path rather than dropping
	// notifications silently. See docs/CONFIGURATION.md → WebSocket proxy
	// tier and deploy/WEBSOCKET_PROXY.md.
	WSProxyMode bool

	// FabricConsoleURL is the base URL of the zk-object-fabric console
	// API (e.g. "https://console.fabric.example.com"). When empty,
	// signup falls back to the static S3_* env vars and per-workspace
	// tenant provisioning is disabled.
	FabricConsoleURL string
	// FabricConsoleAdminToken is sent as a bearer token on console
	// admin endpoints (placement read / write). The signup endpoint is
	// public and does not require it.
	FabricConsoleAdminToken string
	// FabricBucketTemplate names the bucket created per tenant. The
	// literal string "{tenant}" is replaced with the new tenant ID.
	// When empty, defaults to "zk-drive-{tenant}".
	FabricBucketTemplate string
	// FabricDefaultPlacementRef is the placement_policy_ref recorded
	// on freshly provisioned workspaces. Defaults to
	// "b2c_pooled_default" to mirror the migration default.
	FabricDefaultPlacementRef string

	// StaticDir, when non-empty, makes the server serve a single-page
	// app (typically the Vite-built `frontend/dist`) on every request
	// that doesn't match an `/api` route or `/healthz`. Missing files
	// fall back to `index.html` so client-side routes (`/drive`,
	// `/login`, ...) work on a hard refresh. Leaving it empty keeps
	// the server API-only, which is the production deployment shape.
	StaticDir string

	// Stripe — billing webhook integration. When
	// StripeWebhookSecret is empty the /api/webhooks/stripe route is
	// still mounted but every request is rejected with 400 because
	// signatures cannot be verified. StripeSecretKey is currently
	// only used for outbound API calls (e.g. retrieving expanded
	// objects) and is reserved for future server-initiated flows.
	StripeWebhookSecret string
	StripeSecretKey     string
	// StripePriceTierMap maps Stripe price IDs to billing tier names
	// (`starter`, `business`, `secure_business`). Parsed from the
	// STRIPE_PRICE_TIER_MAP env var as comma-separated
	// `price_id:tier` pairs, e.g.
	// `price_123:starter,price_456:business`. Empty values fall
	// through to the price object's metadata.tier field.
	StripePriceTierMap map[string]string

	// Local on-device LLM. When OLLAMA_URL is set the AI summary
	// service routes through the daemon at that address (default
	// 127.0.0.1:11434, the standard Ollama port) using the model
	// named in OLLAMA_MODEL (default qwen2.5:1.5b). When unset, the
	// summary service stays on the deterministic rule-based
	// scaffold — there is no external-API fallback by design, the
	// product never sends file content to a third-party LLM.
	OllamaURL   string
	OllamaModel string

	// Browser security headers (CSP / HSTS / etc.) emitted on
	// every response by api/middleware.SecurityHeaders. The
	// middleware ships a safe default policy; these env vars
	// are knobs operators reach for during rollout or when
	// integrating the storage gateway origin.
	//
	// SecurityHeadersDisableHSTS skips Strict-Transport-Security.
	// Useful for local HTTP development; should remain false in
	// production where TLS terminates at the ingress.
	SecurityHeadersDisableHSTS bool
	// SecurityHeadersCSPReportOnly emits the policy under
	// Content-Security-Policy-Report-Only instead of enforcing.
	// Use during initial rollout to confirm the report stream
	// is clean before flipping the switch.
	SecurityHeadersCSPReportOnly bool
	// SecurityHeadersCSPReportURI is appended as `report-uri`
	// to the CSP value. Browsers POST violation reports there.
	SecurityHeadersCSPReportURI string
	// SecurityHeadersCSPConnectExtra is a comma-separated list
	// of additional origins to allow in `connect-src` (on top of
	// the default `'self'`). The default deliberately does NOT
	// include bare `wss:` / `ws:` scheme sources — `'self'`
	// already covers same-origin WebSocket upgrades, and bare
	// `wss:` would allow XSS exfiltration to any host. The
	// fabric storage gateway URL goes here (required so
	// presigned-URL uploads / downloads land); a cross-origin
	// WebSocket gateway URL (if any) goes here too.
	SecurityHeadersCSPConnectExtra []string
	// SecurityHeadersCSPImgExtra is a comma-separated list of
	// additional origins to allow in `img-src` (on top of
	// `'self' data: blob:`). Thumbnails / previews served from
	// the storage gateway origin go here.
	SecurityHeadersCSPImgExtra []string
	// SecurityHeadersCSPNonce enables a per-request CSP nonce
	// (6.5): a fresh `'nonce-<base64>'` source is added to
	// `script-src` on every response and surfaced to the SPA via
	// the `<meta name="csp-nonce">` tag in index.html, so an
	// inline script the app legitimately needs can be allow-listed
	// by nonce WITHOUT reopening `'unsafe-inline'`. Defaults to
	// true (the policy already ships nonce-clean — there are no
	// inline scripts — so enabling it is purely additive and
	// future-proofs against a dependency that injects one). Set
	// SECURITY_HEADERS_CSP_NONCE=false to suppress the per-request
	// nonce (e.g. when a downstream CDN strips the meta tag).
	SecurityHeadersCSPNonce bool
	// SecurityHeadersExpectCT emits the Expect-CT header (6.5).
	// Defaults to true under the production profile and false
	// elsewhere; also suppressed when HSTS is disabled (Expect-CT
	// is only meaningful over HTTPS). Sourced from
	// SECURITY_HEADERS_EXPECT_CT when set (overrides the profile
	// default). Expect-CT is superseded by browsers enforcing
	// Certificate Transparency by default, but it is still honoured
	// by deployed clients and is an explicit hardening requirement.
	SecurityHeadersExpectCT bool

	// WorkerMetricsAddr is the listen address for the worker
	// binary's dedicated /metrics HTTP server. Default ":9091"
	// (one port above the server's default :8080 so a single
	// host can run both without conflict). Set to "off" or the
	// empty string to disable the metrics server entirely —
	// useful for sidecar-only deployments where the operator
	// uses a different collection path. "Unset" and "explicitly
	// empty" are distinct: an unset env var falls back to the
	// default :9091, while WORKER_METRICS_ADDR= (explicit empty)
	// is the documented disable. The server binary exposes
	// /metrics on its main port (ListenAddr) and does NOT
	// consult this field. The reconciler binary is short-
	// lived and does not export metrics; see README.
	WorkerMetricsAddr string

	// PublicURL is the canonical externally-reachable base URL of
	// the frontend (e.g. "https://drive.example.com"). The email
	// service uses it to compose invite-accept links; when empty,
	// transactional email is forcibly disabled (links would point
	// at an unrouted host). Trailing slashes are stripped at
	// consumption time so operators can paste either form.
	PublicURL string

	// SMTP — transactional email transport. The email service
	// boots into a NoopClient when SMTPHost is empty (logged at
	// startup), so dev environments don't fail to come up. In
	// production, leaving SMTPHost unset means guest-invite
	// emails are silently no-ops; the operator README documents
	// the trade-off and the in-app notification path that still
	// fires for known users.
	SMTPHost                  string
	SMTPPort                  int
	SMTPUsername              string
	SMTPPassword              string
	SMTPFromAddress           string
	SMTPFromName              string
	SMTPTLSMode               string // "starttls" (default), "implicit", "none"
	SMTPTLSServerName         string
	SMTPTLSInsecureSkipVerify bool

	// OpenTelemetry tracing. When OTELExporterOTLPEndpoint is
	// empty, the tracing subsystem installs a no-op tracer so the
	// server boots without an exporter wired up. Setting the
	// endpoint flips on the OTLP/HTTP exporter; the rest of the
	// fields tune the exporter and sampler around that. All values
	// are surfaced through tracing.LogStartup at server boot so an
	// operator can confirm the configuration without grep'ing the
	// pod's env.
	//
	// Endpoints follow the upstream OTEL_EXPORTER_OTLP_ENDPOINT
	// convention: a base URL like "https://otlp.example.com:4318"
	// (the package appends "/v1/traces"), or a full URL like
	// "https://otlp.example.com:4318/v1/traces" — both forms
	// work, the exporter normalises them at init time.
	OTELExporterOTLPEndpoint string
	// Headers is a comma-separated list of header_name=header_value
	// pairs, matching the W3C-style OTEL_EXPORTER_OTLP_HEADERS
	// env var (e.g. "x-honeycomb-team=KEY,x-tenant=acme"). Used
	// to authenticate against managed backends. Parsed at load
	// time so a malformed entry surfaces as a startup warning
	// rather than a silent silent-drop at the first trace export.
	OTELExporterOTLPHeaders map[string]string
	// Insecure disables TLS for the exporter. Only set for a
	// local collector running over plain HTTP — production should
	// always terminate TLS.
	OTELExporterOTLPInsecure bool
	// Compression: "gzip" (default) or "none". Matches the
	// upstream OTEL_EXPORTER_OTLP_COMPRESSION semantics.
	OTELExporterOTLPCompression string
	// ServiceName is the service.name resource attribute every
	// span carries. Defaults to "zk-drive" so spans from the
	// server and worker binaries land under the same logical
	// service in the backend.
	OTELServiceName string
	// DeploymentEnvironment is the deployment.environment
	// resource attribute (e.g. "production", "staging", "dev").
	// Defaults to empty so dev environments don't accidentally
	// tag themselves "production".
	OTELDeploymentEnvironment string
	// SamplerRatio is the parent-based ratio sampler value in
	// [0,1]. Defaults to 0.1 (10%) for production. Set to 1.0
	// for dev / debug; set to 0.0 to disable sampling entirely
	// (preferred to setting endpoint="" if you want the SDK
	// initialised but no traces exported).
	OTELSamplerRatio float64

	// Audit-log cold archival. When AuditArchiveEnabled
	// is false (the default), the audit-archiver binary refuses
	// to run — operators must explicitly opt in so a fresh
	// install can't accidentally start deleting audit history
	// before the operator has confirmed the S3 archive prefix
	// is writable and the retention window matches their
	// compliance posture.
	AuditArchiveEnabled bool
	// AuditLogRetentionDays is the hot-tier retention window
	// (rows older than now() - this many days are eligible for
	// archival). 90 days is the typical SOC2 Type II hot tier;
	// the value is clamped at config-load time by
	// clampAuditRetentionDays so a non-positive (<=0) input
	// silently becomes 90 (the operator most likely forgot to
	// set the var) and a value above 10 years also clamps to
	// 10 years (catches stray multiplier typos). The clamped
	// value is logged at startup so an operator can confirm
	// the effective setting. AUDIT_LOG_ARCHIVE_ENABLED=false
	// is the supported way to disable archival; setting
	// AUDIT_LOG_RETENTION_DAYS=0 does NOT disable archival —
	// it just runs at the 90-day default.
	AuditLogRetentionDays int
	// AuditArchivePrefix is the S3 key prefix every archive
	// object is written under. Defaults to "audit-archive/".
	// Operators wanting to use a dedicated bucket can set
	// AuditArchiveBucket; when empty the configured S3_BUCKET
	// is reused so a single-bucket deployment is the default.
	AuditArchivePrefix string
	// AuditArchiveBucket overrides S3_BUCKET for archive
	// writes. Empty means use S3_BUCKET. Useful when the
	// operator wants compliance-grade lifecycle policies (e.g.
	// Glacier transition, object-lock retention) on the
	// archive bucket independent of the live file store.
	AuditArchiveBucket string
	// AuditArchiveMaxRowsPerBatch caps the number of audit_log
	// rows packed into a single (workspace, month) JSONL.gz
	// upload. Defaults to 50000 — at ~512 bytes per audit
	// entry that's roughly a 25 MB uncompressed / 5 MB
	// compressed object, comfortably below S3's 5 GB single-
	// upload limit and small enough that a single PUT round-
	// trip stays under the 60s exporter context budget. When
	// a (workspace, month) batch exceeds this, the archiver
	// writes multiple JSONL.gz objects with distinct UUID
	// suffixes — the restore tool joins them transparently.
	AuditArchiveMaxRowsPerBatch int
	// AuditHMACKey is the 32-byte key used to HMAC the audit-log
	// hash chain (6.6): each audit_log row carries an HMAC over the
	// previous row's hash, so any insertion / deletion / mutation
	// is detectable by recomputing the chain — even by a DB admin,
	// because the key never lives in the database. It is derived at
	// load time, NEVER read from the DB:
	//   - When AUDIT_HMAC_KEY is set, it is HKDF-expanded to 32
	//     bytes (the raw env value is treated as input keying
	//     material; any length is accepted). Operators SHOULD set a
	//     dedicated key (ideally from a KMS / sealed secret) so the
	//     audit chain stays verifiable across a JWT_SECRET rotation
	//     and so a leaked JWT_SECRET cannot forge audit history.
	//   - When unset, the key is HKDF-derived from JWT_SECRET (a
	//     required, env-held secret) under a distinct info label so
	//     a fresh install is self-operating (NoOps) with no extra
	//     configuration while keeping the key out of the database.
	// AuditHMACKeySource records which of the two paths produced the
	// key so the server can log a one-line recommendation at
	// startup when running on the derived fallback under production.
	AuditHMACKey       []byte
	AuditHMACKeySource string

	// PerformanceCacheEnabled toggles the Redis-backed read-through
	// cache in front of permission resolution (and, in future
	// iterations, listing endpoints). Default true. The cache is a
	// pure read accelerator over the live Postgres tables — every
	// entry self-expires within PerformanceCacheTTL and is
	// proactively busted on permission and folder-topology
	// mutations via the permission service's BustWorkspace hook,
	// so operating with the cache off is identical in observable
	// behaviour to operating with it on (just slower). Setting it
	// false is the safe rollback knob if the cache layer ever
	// misbehaves in production.
	//
	// Requires REDIS_URL to be configured: when REDIS_URL is
	// empty, the wiring code logs a single startup warning and
	// silently falls back to the un-cached repository regardless
	// of this flag (the in-memory single-replica fallback for
	// sessions / rate limit already lives without Redis; the
	// permission cache deliberately does NOT add an in-memory
	// fallback because that would let a multi-replica deployment
	// drift per-replica and the resulting confusion is worse than
	// the lost cache hits).
	PerformanceCacheEnabled bool
	// PerformanceCacheTTL is the TTL written on every cache entry.
	// Sized so that even when the proactive bust hook misses a
	// mutation (e.g. an admin SQL-level change made outside the
	// service layer), stale grants self-expire within seconds.
	// Defaults to 30s — long enough for the common request burst
	// (a folder browse session typically reads the same resource
	// dozens of times within a few seconds) to enjoy the cache,
	// short enough that a forgotten admin change is behaviourally
	// indistinguishable from "not cached" after half a minute.
	// Set via PERFORMANCE_CACHE_TTL accepting the usual
	// time.ParseDuration syntax (e.g. "60s", "5m"). Values outside
	// [1s, 5m] are clamped by clampPerformanceCacheTTL so a typo
	// can't disable the TTL (0 -> no expiry would leak entries
	// forever) or force-expire on every read (sub-second TTLs
	// busy-loop the cache without serving hits).
	PerformanceCacheTTL time.Duration

	// Web Push (RFC 8030 + VAPID). VAPIDPublicKey / VAPIDPrivateKey
	// are the application-server key pair used to sign push messages
	// and identify the server to push services. Generate a pair with
	// `npx web-push generate-vapid-keys` (see docs/CONFIGURATION.md).
	// When EITHER value is empty, Web Push is disabled (graceful
	// degradation): the /api/push/* endpoints respond 501 Not
	// Implemented and the notification publisher skips the push
	// fan-out, so the in-app + WebSocket notification path keeps
	// working unchanged.
	VAPIDPublicKey  string
	VAPIDPrivateKey string

	// VAPIDSubscriber is the `sub` claim embedded in the VAPID JWT: a
	// mailto: or https: URI push services (FCM, Mozilla autopush) use to
	// contact the application-server operator about a misbehaving sender.
	// Optional — when empty the WebPushService keeps its built-in
	// placeholder. Operators running real push traffic should set this to
	// a monitored mailbox (e.g. "mailto:ops@yourdomain.com") so abuse
	// reports reach them.
	VAPIDSubscriber string

	// OnlyOfficeURL is the base URL of the ONLYOFFICE Document Server
	// (e.g. "https://onlyoffice.example.com"). When empty,
	// collaborative office-document editing is disabled: the
	// /api/files/{id}/editor-config endpoint reports the feature as
	// unavailable and the frontend hides the "Open in Editor" button
	// (graceful degradation). Set via ONLYOFFICE_URL.
	OnlyOfficeURL string
	// OnlyOfficeSecret is the shared JWT secret configured on the
	// ONLYOFFICE Document Server (its JWT_ENABLED / JWT_SECRET pair).
	// The server signs the editor config it hands the browser and
	// verifies the JWT on inbound Document Server save callbacks with
	// this value. When empty, the config is emitted unsigned and the
	// callback skips token verification — acceptable only for trusted
	// local development. Set via ONLYOFFICE_SECRET.
	OnlyOfficeSecret string
	// OnlyOfficeAllowInsecure explicitly opts in to running the
	// ONLYOFFICE integration WITHOUT a callback-verification secret
	// (ONLYOFFICE_URL set but ONLYOFFICE_SECRET empty). This leaves the
	// editor-callback endpoint unauthenticated, so it is refused by
	// default: Load returns an error unless this is set. Intended only
	// for trusted local development against a JWT-disabled Document
	// Server. Set via ONLYOFFICE_ALLOW_INSECURE.
	OnlyOfficeAllowInsecure bool
	// OnlyOfficeMaxDocumentBytes caps how many bytes the save callback
	// will read from the Document Server into memory before rejecting
	// the document as oversized. Set via ONLYOFFICE_MAX_DOCUMENT_MB
	// (megabytes); defaults to defaultOnlyOfficeMaxDocumentMB.
	OnlyOfficeMaxDocumentBytes int64
	// OnlyOfficeSaveMemoryBudgetBytes is the share of the API
	// container's memory the save path may buffer concurrently. The
	// concurrency cap (see OnlyOfficeMaxConcurrentSaves) is DERIVED as
	// budget / max-document so the worst case
	// (concurrency * max-document) stays within budget by construction.
	// Set via ONLYOFFICE_SAVE_MEMORY_BUDGET_MB (megabytes); defaults to
	// defaultOnlyOfficeSaveMemoryBudgetMB. Must be >= the per-document
	// cap or the server refuses to start (a budget below one document
	// would shed every save).
	OnlyOfficeSaveMemoryBudgetBytes int64

	// SuspensionFailClosed flips the workspace-suspension enforcement
	// posture from fail-OPEN (default) to fail-CLOSED. Suspension is an
	// availability control, so by default a suspension-lookup error
	// (e.g. a transient DB blip) lets the request proceed rather than
	// locking the fleet out. Deployments that use suspension for
	// compliance / legal holds — where a suspended workspace must NEVER
	// transact even during a database outage — can set
	// SUSPENSION_FAIL_CLOSED=true to return 503 on a lookup error
	// instead. Applies to both SuspensionGuard (REST/WS) and the
	// ONLYOFFICE save-callback write boundary.
	SuspensionFailClosed bool

	// Profile is the resolved ZKDRIVE_PROFILE deployment shape
	// ("compact", "production", "development", or "" for none). It is
	// applied BEFORE the other fields are read so its env-var defaults
	// (see internal/config/profiles.go) feed into the parsing below;
	// the value recorded here is purely for logging / validateProfile.
	Profile string

	// AutoMigrate makes the server apply pending migrations under the
	// schema advisory lock at startup, before it begins serving. It is
	// sourced from ZKDRIVE_AUTO_MIGRATE (default false) and the compact
	// profile defaults it to true. The cmd/server --auto-migrate flag
	// ORs with this. Production K8s leaves it off and runs the separate
	// migrate Job so schema changes are decoupled from pod rollout.
	AutoMigrate bool
}

// OnlyOfficeMaxConcurrentSaves derives how many save callbacks may
// buffer an edited document in memory at once from the configured
// memory budget and per-document cap. It is always >= 1 (Load
// validates budget >= per-document so the floor division never yields
// 0 for an enabled integration).
func (c *Config) OnlyOfficeMaxConcurrentSaves() int {
	if c.OnlyOfficeMaxDocumentBytes <= 0 {
		return 1
	}
	n := int(c.OnlyOfficeSaveMemoryBudgetBytes / c.OnlyOfficeMaxDocumentBytes)
	if n < 1 {
		return 1
	}
	return n
}

// WebPushEnabled reports whether both VAPID keys are configured. When
// false, Web Push is disabled and callers fall back to the in-app /
// WebSocket notification path only.
func (c *Config) WebPushEnabled() bool {
	return c.VAPIDPublicKey != "" && c.VAPIDPrivateKey != ""
}

// Load reads configuration from environment variables and returns a populated
// Config. It returns an error if any required variable is missing or empty.
//
// The S3 group (S3_ENDPOINT, S3_BUCKET, S3_ACCESS_KEY, S3_SECRET_KEY) is
// optional so the server can boot for metadata-only development without a
// running zk-object-fabric gateway. However, if S3_ENDPOINT is set, the
// bucket, access key, and secret key must also be set — a half-configured
// storage client would only fail at request time.
func Load() (*Config, error) {
	// Resolve ZKDRIVE_PROFILE first so its env-var defaults are in
	// place (only-if-unset) before buildConfigFromEnv reads them. An
	// unknown profile name fails closed here rather than silently
	// running with zero presets.
	if _, err := applyProfileDefaults(); err != nil {
		return nil, err
	}

	cfg := buildConfigFromEnv()

	var missing []string
	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if strings.TrimSpace(cfg.JWTSecret) == "" {
		missing = append(missing, "JWT_SECRET")
	}
	if len(missing) > 0 {
		return nil, errors.New("missing required environment variables: " + strings.Join(missing, ", "))
	}

	if err := validateS3Group(cfg); err != nil {
		return nil, err
	}
	if err := validateOnlyOfficeGroup(cfg); err != nil {
		return nil, err
	}
	if err := validateProfile(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// buildConfigFromEnv populates a *Config purely from environment
// variables WITHOUT applying required-variable validation. Shared
// between Load (which adds the DATABASE_URL + JWT_SECRET + S3-group
// checks) and LoadStorageOnly (which only validates the S3 group).
// Keep all per-field defaulting + parsing rules in this single
// function so adding a new field doesn't require touching multiple
// constructors.
func buildConfigFromEnv() *Config {
	// Read DB_MAX_CONNS once: DBMinConns is clamped against the same
	// resolved maximum, so re-reading the env var would be redundant.
	dbMaxConns := dbMaxConnsFromEnv()
	platformAdmins, invalidPlatformAdmins := platformAdminUserIDsFromEnv()
	profile := normaliseProfile(os.Getenv("ZKDRIVE_PROFILE"))
	auditKey, auditKeySource := deriveAuditHMACKey(os.Getenv("AUDIT_HMAC_KEY"), os.Getenv("JWT_SECRET"))
	return &Config{
		DatabaseURL:                            os.Getenv("DATABASE_URL"),
		DatabaseReadURL:                        os.Getenv("DATABASE_READ_URL"),
		JWTSecret:                              os.Getenv("JWT_SECRET"),
		Profile:                                string(profile),
		DBMaxConns:                             dbMaxConns,
		DBMinConns:                             dbMinConnsFromEnv(dbMaxConns),
		DBMaxConnIdleTime:                      parseDurationDefault(os.Getenv("DB_MAX_CONN_IDLE_TIME"), defaultDBMaxConnIdleTime),
		JWTAlgorithm:                           jwtAlgorithmFromEnv(os.Getenv("JWT_ALGORITHM"), profile),
		JWTKeyRefreshInterval:                  jwtKeyRefreshIntervalFromEnv(),
		PlatformAdminUserIDs:                   platformAdmins,
		PlatformAdminUserIDsInvalid:            invalidPlatformAdmins,
		ListenAddr:                             getEnvDefault("LISTEN_ADDR", ":8080"),
		S3Endpoint:                             os.Getenv("S3_ENDPOINT"),
		S3Bucket:                               os.Getenv("S3_BUCKET"),
		S3AccessKey:                            os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey:                            os.Getenv("S3_SECRET_KEY"),
		MigrationsDir:                          getEnvDefault("MIGRATIONS_DIR", "migrations"),
		NATSURL:                                os.Getenv("NATS_URL"),
		ClamAVAddress:                          os.Getenv("CLAMAV_ADDRESS"),
		GoogleClientID:                         os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret:                     os.Getenv("GOOGLE_CLIENT_SECRET"),
		GoogleRedirectURL:                      os.Getenv("GOOGLE_REDIRECT_URL"),
		MicrosoftClientID:                      os.Getenv("MICROSOFT_CLIENT_ID"),
		MicrosoftClientSecret:                  os.Getenv("MICROSOFT_CLIENT_SECRET"),
		MicrosoftRedirectURL:                   os.Getenv("MICROSOFT_REDIRECT_URL"),
		IAMCoreIssuerURL:                       strings.TrimSpace(os.Getenv("IAM_CORE_ISSUER_URL")),
		IAMCoreClientID:                        strings.TrimSpace(os.Getenv("IAM_CORE_CLIENT_ID")),
		IAMCoreClientSecret:                    os.Getenv("IAM_CORE_CLIENT_SECRET"),
		IAMCoreAudience:                        strings.TrimSpace(os.Getenv("IAM_CORE_AUDIENCE")),
		IAMCoreScopes:                          parseScopeList(os.Getenv("IAM_CORE_SCOPES")),
		IAMCoreCallbackURL:                     strings.TrimSpace(os.Getenv("IAM_CORE_CALLBACK_URL")),
		RateLimitPerUser:                       parseIntDefault(os.Getenv("RATE_LIMIT_PER_USER"), 0),
		RateLimitPerWorkspace:                  parseIntDefault(os.Getenv("RATE_LIMIT_PER_WORKSPACE"), 0),
		AuthFailureThreshold:                   parseIntDefault(os.Getenv("AUTH_FAILURE_THRESHOLD"), 0),
		AuthBlockDuration:                      parseDurationDefault(os.Getenv("AUTH_BLOCK_DURATION"), 0),
		AuthReputationRetention:                parseDurationDefault(os.Getenv("AUTH_REPUTATION_RETENTION"), 0),
		TrustedProxyDepth:                      parseNonNegativeIntDefault(os.Getenv("TRUSTED_PROXY_DEPTH"), defaultTrustedProxyDepth),
		PreviewBudgetPerWorkspaceHour:          parseIntDefault(os.Getenv("PREVIEW_BUDGET_PER_WORKSPACE_HOUR"), 100),
		PreviewPriorityWorkers:                 parseIntDefault(os.Getenv("PREVIEW_PRIORITY_WORKERS"), 6),
		PreviewStandardWorkers:                 parseIntDefault(os.Getenv("PREVIEW_STANDARD_WORKERS"), 2),
		PreviewLightweightWorkers:              parseIntDefault(os.Getenv("PREVIEW_LIGHTWEIGHT_WORKERS"), 8),
		PreviewHeavyWorkers:                    parseIntDefault(os.Getenv("PREVIEW_HEAVY_WORKERS"), 4),
		PreviewWorkerConcurrency:               parseNonNegativeIntDefault(os.Getenv("PREVIEW_WORKER_CONCURRENCY"), 0),
		PreviewHeavyQueueBackpressureThreshold: parseNonNegativeIntDefault(os.Getenv("PREVIEW_HEAVY_QUEUE_BACKPRESSURE_THRESHOLD"), 0),
		RedisURL:                               os.Getenv("REDIS_URL"),
		WSProxyMode:                            parseBoolDefault(os.Getenv("WS_PROXY_MODE"), false),
		FabricConsoleURL:                       os.Getenv("FABRIC_CONSOLE_URL"),
		FabricConsoleAdminToken:                os.Getenv("FABRIC_CONSOLE_ADMIN_TOKEN"),
		FabricBucketTemplate:                   getEnvDefault("FABRIC_BUCKET_TEMPLATE", "zk-drive-{tenant}"),
		FabricDefaultPlacementRef:              getEnvDefault("FABRIC_DEFAULT_PLACEMENT_REF", "b2c_pooled_default"),
		StaticDir:                              os.Getenv("STATIC_DIR"),
		StripeWebhookSecret:                    os.Getenv("STRIPE_WEBHOOK_SECRET"),
		StripeSecretKey:                        os.Getenv("STRIPE_SECRET_KEY"),
		StripePriceTierMap:                     parsePriceTierMap(os.Getenv("STRIPE_PRICE_TIER_MAP")),
		OllamaURL:                              os.Getenv("OLLAMA_URL"),
		OllamaModel:                            os.Getenv("OLLAMA_MODEL"),

		SecurityHeadersDisableHSTS:     parseBoolDefault(os.Getenv("SECURITY_HEADERS_DISABLE_HSTS"), false),
		SecurityHeadersCSPReportOnly:   parseBoolDefault(os.Getenv("SECURITY_HEADERS_CSP_REPORT_ONLY"), false),
		SecurityHeadersCSPReportURI:    os.Getenv("SECURITY_HEADERS_CSP_REPORT_URI"),
		SecurityHeadersCSPConnectExtra: parseCSVList(os.Getenv("SECURITY_HEADERS_CSP_CONNECT_EXTRA")),
		SecurityHeadersCSPImgExtra:     parseCSVList(os.Getenv("SECURITY_HEADERS_CSP_IMG_EXTRA")),
		SecurityHeadersCSPNonce:        parseBoolDefault(os.Getenv("SECURITY_HEADERS_CSP_NONCE"), true),
		// Expect-CT defaults on under the production profile, off
		// otherwise; an explicit env value wins either way.
		SecurityHeadersExpectCT: parseBoolDefault(os.Getenv("SECURITY_HEADERS_EXPECT_CT"), profile == ProfileProduction),

		// WorkerMetricsAddr uses LookupEnv (not getEnvDefault) so the
		// documented contract holds: unset → default :9091; explicitly
		// empty (WORKER_METRICS_ADDR=) → disabled. getEnvDefault would
		// collapse both into the default, breaking the operator's
		// expectation that `=` disables the server.
		WorkerMetricsAddr: workerMetricsAddrFromEnv(),

		PublicURL: strings.TrimSpace(os.Getenv("PUBLIC_URL")),

		SMTPHost:                  strings.TrimSpace(os.Getenv("SMTP_HOST")),
		SMTPPort:                  parseIntDefault(os.Getenv("SMTP_PORT"), 587),
		SMTPUsername:              os.Getenv("SMTP_USERNAME"),
		SMTPPassword:              os.Getenv("SMTP_PASSWORD"),
		SMTPFromAddress:           strings.TrimSpace(os.Getenv("SMTP_FROM_ADDRESS")),
		SMTPFromName:              os.Getenv("SMTP_FROM_NAME"),
		SMTPTLSMode:               getEnvDefault("SMTP_TLS_MODE", "starttls"),
		SMTPTLSServerName:         os.Getenv("SMTP_TLS_SERVER_NAME"),
		SMTPTLSInsecureSkipVerify: parseBoolDefault(os.Getenv("SMTP_TLS_INSECURE_SKIP_VERIFY"), false),

		OTELExporterOTLPEndpoint:    strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")),
		OTELExporterOTLPHeaders:     parseOTELHeaders(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS")),
		OTELExporterOTLPInsecure:    parseBoolDefault(os.Getenv("OTEL_EXPORTER_OTLP_INSECURE"), false),
		OTELExporterOTLPCompression: strings.ToLower(strings.TrimSpace(getEnvDefault("OTEL_EXPORTER_OTLP_COMPRESSION", "gzip"))),
		OTELServiceName:             getEnvDefault("OTEL_SERVICE_NAME", "zk-drive"),
		OTELDeploymentEnvironment:   os.Getenv("OTEL_DEPLOYMENT_ENVIRONMENT"),
		OTELSamplerRatio:            parseFloatDefault(os.Getenv("OTEL_TRACES_SAMPLER_ARG"), 0.1),

		AuditArchiveEnabled:         parseBoolDefault(os.Getenv("AUDIT_LOG_ARCHIVE_ENABLED"), false),
		AuditLogRetentionDays:       clampAuditRetentionDays(parseIntDefault(os.Getenv("AUDIT_LOG_RETENTION_DAYS"), 90)),
		AuditArchivePrefix:          normaliseArchivePrefix(getEnvDefault("AUDIT_LOG_ARCHIVE_PREFIX", "audit-archive/")),
		AuditArchiveBucket:          strings.TrimSpace(os.Getenv("AUDIT_LOG_ARCHIVE_BUCKET")),
		AuditArchiveMaxRowsPerBatch: clampAuditMaxRowsPerBatch(parseIntDefault(os.Getenv("AUDIT_LOG_ARCHIVE_MAX_ROWS_PER_BATCH"), defaultAuditArchiveMaxRowsPerBatch)),
		AuditHMACKey:                auditKey,
		AuditHMACKeySource:          auditKeySource,

		PerformanceCacheEnabled: parseBoolDefault(os.Getenv("PERFORMANCE_CACHE_ENABLED"), defaultPerformanceCacheEnabled),
		PerformanceCacheTTL:     clampPerformanceCacheTTL(parseDurationDefault(os.Getenv("PERFORMANCE_CACHE_TTL"), defaultPerformanceCacheTTL)),

		VAPIDPublicKey:  strings.TrimSpace(os.Getenv("VAPID_PUBLIC_KEY")),
		VAPIDPrivateKey: strings.TrimSpace(os.Getenv("VAPID_PRIVATE_KEY")),
		VAPIDSubscriber: strings.TrimSpace(os.Getenv("VAPID_SUBSCRIBER")),

		OnlyOfficeURL:                   strings.TrimSpace(os.Getenv("ONLYOFFICE_URL")),
		OnlyOfficeSecret:                os.Getenv("ONLYOFFICE_SECRET"),
		OnlyOfficeAllowInsecure:         parseBoolDefault(os.Getenv("ONLYOFFICE_ALLOW_INSECURE"), false),
		OnlyOfficeMaxDocumentBytes:      onlyOfficeBytesFromEnv("ONLYOFFICE_MAX_DOCUMENT_MB", defaultOnlyOfficeMaxDocumentMB),
		OnlyOfficeSaveMemoryBudgetBytes: onlyOfficeBytesFromEnv("ONLYOFFICE_SAVE_MEMORY_BUDGET_MB", defaultOnlyOfficeSaveMemoryBudgetMB),

		SuspensionFailClosed: parseBoolDefault(os.Getenv("SUSPENSION_FAIL_CLOSED"), false),

		AutoMigrate: parseBoolDefault(os.Getenv("ZKDRIVE_AUTO_MIGRATE"), false),
	}
}

// LoadStorageOnly reads configuration from environment variables and
// returns a populated Config WITHOUT enforcing DATABASE_URL or
// JWT_SECRET. The S3 group is still validated as a coherent set
// (S3_ENDPOINT requires S3_BUCKET + S3_ACCESS_KEY + S3_SECRET_KEY).
//
// Intended for read-only binaries that never touch Postgres or the
// HTTP request lifecycle — currently just cmd/audit-restore, which
// streams gzipped JSONL audit objects out of S3 for incident
// investigation. Forcing those operators to supply DATABASE_URL /
// JWT_SECRET (per the README workaround `JWT_SECRET=unused-but-required`)
// is friction during incident response: an on-call engineer may have
// S3 credentials in hand but not the running Postgres password.
//
// Any new binary that wants this slim variant should call
// LoadStorageOnly explicitly; the default Load remains strict so
// server / worker startup still fails fast if those env vars are
// missing.
func LoadStorageOnly() (*Config, error) {
	cfg := buildConfigFromEnv()
	if err := validateS3Group(cfg); err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.S3Endpoint) == "" {
		return nil, errors.New("audit-restore / storage-only binaries require S3_ENDPOINT to be configured")
	}
	return cfg, nil
}

// validateS3Group enforces the coherent-S3-group invariant: if
// S3_ENDPOINT is set, the bucket + access key + secret key must
// also be set. Shared between Load and LoadStorageOnly so the two
// entrypoints can't drift on S3 validation rules.
func validateS3Group(cfg *Config) error {
	if strings.TrimSpace(cfg.S3Endpoint) == "" {
		return nil
	}
	var missingS3 []string
	if strings.TrimSpace(cfg.S3Bucket) == "" {
		missingS3 = append(missingS3, "S3_BUCKET")
	}
	if strings.TrimSpace(cfg.S3AccessKey) == "" {
		missingS3 = append(missingS3, "S3_ACCESS_KEY")
	}
	if strings.TrimSpace(cfg.S3SecretKey) == "" {
		missingS3 = append(missingS3, "S3_SECRET_KEY")
	}
	if len(missingS3) > 0 {
		return errors.New("S3_ENDPOINT is set but missing required variables: " + strings.Join(missingS3, ", "))
	}
	return nil
}

// validateOnlyOfficeGroup fails closed on an unauthenticated ONLYOFFICE
// callback. When ONLYOFFICE_URL is configured the editor-callback
// endpoint is mounted and writes new file versions from whatever the
// Document Server POSTs; without ONLYOFFICE_SECRET that endpoint is
// unauthenticated, so a misconfigured deployment would expose an
// SSRF-able, spoofable write path to the public internet. We therefore
// refuse to start in that combination unless the operator explicitly
// opts in via ONLYOFFICE_ALLOW_INSECURE (trusted local dev only).
func validateOnlyOfficeGroup(cfg *Config) error {
	if strings.TrimSpace(cfg.OnlyOfficeURL) == "" {
		return nil
	}
	if strings.TrimSpace(cfg.OnlyOfficeSecret) == "" && !cfg.OnlyOfficeAllowInsecure {
		return errors.New("ONLYOFFICE_URL is set but ONLYOFFICE_SECRET is empty: the editor-callback endpoint would be unauthenticated. Set ONLYOFFICE_SECRET (recommended), or set ONLYOFFICE_ALLOW_INSECURE=true to permit the unauthenticated callback for trusted local development")
	}
	// The save concurrency cap is derived as budget / per-document, so a
	// budget below one document would floor to 0 and shed every save.
	// Refuse to start on that misconfiguration rather than silently
	// clamping to 1 (which would also break the worst-case-within-budget
	// invariant the budget exists to enforce).
	if cfg.OnlyOfficeSaveMemoryBudgetBytes < cfg.OnlyOfficeMaxDocumentBytes {
		return fmt.Errorf("ONLYOFFICE_SAVE_MEMORY_BUDGET_MB (%d MiB) must be >= ONLYOFFICE_MAX_DOCUMENT_MB (%d MiB): a budget below one document would shed every save",
			cfg.OnlyOfficeSaveMemoryBudgetBytes/bytesPerMB, cfg.OnlyOfficeMaxDocumentBytes/bytesPerMB)
	}
	return nil
}

func getEnvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// workerMetricsAddrFromEnv distinguishes "unset" (fall back to the
// default :9091) from "explicitly empty" (disable the worker metrics
// server entirely). getEnvDefault collapses both into the default,
// which silently breaks the documented `WORKER_METRICS_ADDR=`
// escape hatch — operators who follow the README and unset/empty
// the variable to disable the server would still get :9091. Using
// LookupEnv preserves the distinction.
func workerMetricsAddrFromEnv() string {
	v, ok := os.LookupEnv("WORKER_METRICS_ADDR")
	if !ok {
		return ":9091"
	}
	return v
}

// parsePriceTierMap parses a comma-separated list of
// `price_id:tier` pairs into a lookup map. Whitespace around each
// pair and the colon is tolerated; malformed pairs are skipped so a
// fat-fingered env var doesn't crash the server at startup.
func parsePriceTierMap(s string) map[string]string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	out := make(map[string]string)
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		idx := strings.Index(pair, ":")
		if idx <= 0 || idx == len(pair)-1 {
			continue
		}
		priceID := strings.TrimSpace(pair[:idx])
		tier := strings.TrimSpace(pair[idx+1:])
		if priceID == "" || tier == "" {
			continue
		}
		out[priceID] = tier
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// maxAuditRetentionDays caps AUDIT_LOG_RETENTION_DAYS at ten years
// (3650 days) so a typo like "9000000" (intended "90") doesn't
// degenerate into "archive nothing ever" and let the hot tier grow
// unboundedly with no operator-visible warning. Ten years exceeds
// every common compliance retention window (SOC2 = 1 year minimum,
// HIPAA = 6 years) and the clamp log makes the corrected value
// surface in startup logs.
const maxAuditRetentionDays = 3650

// minAuditRetentionDays mirrors internal/audit.MinRetentionDays (= 7)
// — the service-level floor that ArchiveService refuses to operate
// below. They MUST stay locked-step: a value that passes config but
// fails the service produces a confusing operator experience where
// the binary loads successfully and then aborts at archive start.
// internal/config/config_test.go pins the equality with a Go-level
// assertion so a future change in either constant fails CI.
const minAuditRetentionDays = 7

// clampAuditRetentionDays bounds AUDIT_LOG_RETENTION_DAYS at sensible
// limits. Returns the configured value when valid; otherwise:
//   - non-positive input clamps to 90 (the default — assume the
//     operator forgot to set it rather than intentionally set 0).
//   - input in the range [1, minAuditRetentionDays-1] (i.e. 1–6 days)
//     clamps UP to minAuditRetentionDays. Values that low almost
//     certainly indicate a typo and aggressively pruning legitimately-
//     recent audit history would compound the mistake. Clamping up
//     preserves audit history rather than deleting it.
//   - input > maxAuditRetentionDays clamps to maxAuditRetentionDays.
//
// The caller surfaces the clamped value at startup via the boot logs
// so an operator can confirm the effective setting.
func clampAuditRetentionDays(d int) int {
	if d <= 0 {
		return 90
	}
	if d < minAuditRetentionDays {
		return minAuditRetentionDays
	}
	if d > maxAuditRetentionDays {
		return maxAuditRetentionDays
	}
	return d
}

// maxAuditArchiveMaxRowsPerBatch caps AUDIT_LOG_ARCHIVE_MAX_ROWS_PER_BATCH
// at 1,000,000 rows so an operator typo like "10000000" (intended
// "100000") doesn't cause the archiver to try to encode 10M rows of
// JSONL.gz in memory and OOM-kill the CronJob pod
// (deploy/k8s/audit-archiver-cronjob.yaml sets a 512Mi memory limit;
// at ~500B per row, 1M rows = ~500MB uncompressed encoded, which
// gzip-streams comfortably within that ceiling while still being a
// dramatic upper bound vs the 50k default).
//
// Defense-in-depth: NewArchiveService already rejects non-positive
// values (defaultMaxRowsPerBatch substitution), but had no upper
// guard. This clamp matches the retention-days pattern — bound at
// both ends in config so the service receives only sane values.
const maxAuditArchiveMaxRowsPerBatch = 1_000_000

// defaultAuditArchiveMaxRowsPerBatch mirrors
// internal/audit.defaultMaxRowsPerBatch so the env-unset default and
// the service-level fallback never drift. Kept locally to avoid an
// internal/config → internal/audit import cycle (audit already
// depends on config types via the ArchiveServiceConfig struct).
const defaultAuditArchiveMaxRowsPerBatch = 50000

// clampAuditMaxRowsPerBatch bounds AUDIT_LOG_ARCHIVE_MAX_ROWS_PER_BATCH
// at sensible limits. Returns the configured value when valid;
// otherwise:
//   - non-positive input clamps to defaultAuditArchiveMaxRowsPerBatch
//     (the default — assume the operator forgot to set it rather
//     than intentionally set 0, which would archive nothing).
//   - input > maxAuditArchiveMaxRowsPerBatch clamps to the ceiling
//     so a malformed env var can't OOM-kill the archiver pod.
//
// The caller surfaces the clamped value at startup via the boot
// logs so an operator can confirm the effective setting.
func clampAuditMaxRowsPerBatch(n int) int {
	if n <= 0 {
		return defaultAuditArchiveMaxRowsPerBatch
	}
	if n > maxAuditArchiveMaxRowsPerBatch {
		return maxAuditArchiveMaxRowsPerBatch
	}
	return n
}

// defaultPerformanceCacheEnabled is the default for the
// PERFORMANCE_CACHE_ENABLED flag. We default to true so a fresh
// deployment gets the perf win automatically; operators can set
// PERFORMANCE_CACHE_ENABLED=false to roll back to the un-cached
// path without re-deploying a different image.
const defaultPerformanceCacheEnabled = true

// defaultPerformanceCacheTTL is the default TTL written on every
// permission-cache entry. See the doc on Config.PerformanceCacheTTL
// for the rationale behind 30 seconds; see clampPerformanceCacheTTL
// for the legal range.
const defaultPerformanceCacheTTL = 30 * time.Second

// minPerformanceCacheTTL / maxPerformanceCacheTTL bound the legal
// range. The lower bound (1s) prevents a typo from busy-looping
// the cache (a 100ms TTL would re-fetch from Postgres on every
// keystroke of a folder browse and is worse than no cache at
// all). The upper bound (5m) prevents a typo from making the
// cache effectively permanent — even with proactive busting,
// admins making changes directly in psql have no path back to
// the application layer so a forgotten entry must self-expire
// in a window short enough that operators don't reach for
// FLUSHDB.
const (
	minPerformanceCacheTTL = time.Second
	maxPerformanceCacheTTL = 5 * time.Minute
)

// clampPerformanceCacheTTL bounds PERFORMANCE_CACHE_TTL at
// [minPerformanceCacheTTL, maxPerformanceCacheTTL]. Values below
// the floor or non-positive clamp to defaultPerformanceCacheTTL
// (assume the operator forgot to set the value rather than
// intentionally configured a TTL that breaks the cache). Values
// above the ceiling clamp to maxPerformanceCacheTTL so a stale
// entry can never linger longer than the operator's mental
// "I just changed that" window.
func clampPerformanceCacheTTL(d time.Duration) time.Duration {
	if d < minPerformanceCacheTTL {
		return defaultPerformanceCacheTTL
	}
	if d > maxPerformanceCacheTTL {
		return maxPerformanceCacheTTL
	}
	return d
}

// parseDurationDefault parses a time.Duration env value (e.g.
// "30s", "5m", "1h"). Empty input and parse failures fall through
// to def — keeping with the rest of the parseFooDefault family
// where a typo silently uses the documented default rather than
// failing the boot.
func parseDurationDefault(s string, def time.Duration) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

// DB connection-pool defaults and bounds. The pool is created by
// internal/database.ConnectWithPool; these mirror the documented
// defaults in docs/CONFIGURATION.md.
const (
	defaultDBMaxConns = 20
	minDBMaxConns     = 2
	maxDBMaxConns     = 200
	defaultDBMinConns = 2

	defaultDBMaxConnIdleTime = 30 * time.Minute
)

// ONLYOFFICE save-path memory sizing (megabytes). The per-document cap
// is deliberately generous (real office documents are 1–50 MB); the
// budget is half the production API container (512 MiB in
// deploy/docker-compose.prod.yml) so the save path can never claim the
// whole container. The derived concurrency (budget / per-document) is
// 256 / 100 = 2 by default — see Config.OnlyOfficeMaxConcurrentSaves.
const (
	defaultOnlyOfficeMaxDocumentMB      = 100
	defaultOnlyOfficeSaveMemoryBudgetMB = 256
	bytesPerMB                          = 1 << 20
)

// onlyOfficeBytesFromEnv parses a megabyte-valued env var into bytes,
// falling back to defMB when unset / non-positive / malformed. Kept
// permissive (clamp rather than error) so a typo degrades to the safe
// default; the budget >= per-document invariant is enforced in
// validateOnlyOfficeGroup, which fails the server start.
func onlyOfficeBytesFromEnv(key string, defMB int) int64 {
	mb := parseIntDefault(os.Getenv(key), defMB)
	if mb <= 0 {
		mb = defMB
	}
	return int64(mb) * bytesPerMB
}

// dbMaxConnsFromEnv parses DB_MAX_CONNS and clamps the result to
// [minDBMaxConns, maxDBMaxConns]. An unset / non-positive / malformed
// value falls back to defaultDBMaxConns (operators most likely forgot
// to set it), and an out-of-range value clamps to the nearest bound
// so a typo can neither starve the pool nor exhaust Postgres'
// max_connections.
func dbMaxConnsFromEnv() int32 {
	n := parseIntDefault(os.Getenv("DB_MAX_CONNS"), defaultDBMaxConns)
	if n < minDBMaxConns {
		n = minDBMaxConns
	}
	if n > maxDBMaxConns {
		n = maxDBMaxConns
	}
	return int32(n)
}

// dbMinConnsFromEnv parses DB_MIN_CONNS and clamps it to
// [0, maxConns]. Unlike parseIntDefault it honours an explicit 0
// (a zero floor is legal — the pool simply opens connections lazily),
// so the parse is done directly here. A value above maxConns clamps
// down to maxConns so MinConns can never exceed MaxConns (pgxpool
// rejects that configuration at NewWithConfig time).
func dbMinConnsFromEnv(maxConns int32) int32 {
	// n stays non-negative by construction: it starts at the default and
	// is only reassigned from an explicitly-parsed value when v >= 0, so a
	// negative DB_MIN_CONNS is ignored (default retained) rather than
	// clamped — no separate floor check is needed.
	n := defaultDBMinConns
	if s := strings.TrimSpace(os.Getenv("DB_MIN_CONNS")); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v >= 0 {
			n = v
		}
	}
	if int32(n) > maxConns {
		return maxConns
	}
	return int32(n)
}

// IsProduction reports whether the server is running under the
// hardened production profile (ZKDRIVE_PROFILE=production). The profile
// machinery itself — the Profile type, normaliseProfile, the profile
// env-var defaults, and validateProfile — lives in profiles.go.
func (c *Config) IsProduction() bool {
	return Profile(c.Profile) == ProfileProduction
}

// Audit HMAC key derivation constants (6.6). The key length is the
// SHA-256 output size; the info labels are domain separators so the
// explicit-key and JWT_SECRET-derived keys are distinct even if an
// operator (mis)uses the same secret material for both.
const (
	auditHMACKeyLen            = sha256.Size
	auditHMACInfoExplicit      = "zk-drive/audit-log-hmac/explicit/v1"
	auditHMACInfoDerived       = "zk-drive/audit-log-hmac/derived-from-jwt-secret/v1"
	AuditHMACKeySourceExplicit = "explicit"
	AuditHMACKeySourceDerived  = "derived"
)

// deriveAuditHMACKey produces the 32-byte audit-chain HMAC key and
// reports its provenance. An explicit AUDIT_HMAC_KEY (any length) is
// HKDF-expanded; otherwise the key is HKDF-derived from JWT_SECRET
// under a distinct info label. The key is intentionally derived in
// process from env-held material and never persisted, so a DB admin
// cannot forge the chain. HKDF is keyed by SHA-256; the only error it
// can return is for an absurd output length, which is impossible with
// the fixed 32-byte constant — but we fall back to a plain SHA-256 of
// (label || secret) defensively rather than returning a nil key.
func deriveAuditHMACKey(explicit, jwtSecret string) (key []byte, source string) {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		return hkdfExpand([]byte(explicit), auditHMACInfoExplicit), AuditHMACKeySourceExplicit
	}
	return hkdfExpand([]byte(jwtSecret), auditHMACInfoDerived), AuditHMACKeySourceDerived
}

func hkdfExpand(secret []byte, info string) []byte {
	k, err := hkdf.Key(sha256.New, secret, nil, info, auditHMACKeyLen)
	if err != nil {
		sum := sha256.Sum256(append([]byte(info+"\x00"), secret...))
		return sum[:]
	}
	return k
}

// jwtAlgorithmFromEnv resolves the effective JWT signing algorithm
// from an explicit JWT_ALGORITHM value and the active profile. An
// explicit, recognised value always wins. When JWT_ALGORITHM is unset
// (or unrecognised) the default is profile-dependent: "ES256" under
// the production profile (asymmetric-only signing is mandatory there)
// and "auto" otherwise.
func jwtAlgorithmFromEnv(raw string, profile Profile) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "ES256":
		return "ES256"
	case "HS256":
		return "HS256"
	case "AUTO":
		return "auto"
	default:
		// Unset or unrecognised: fall back to the profile default.
		if profile == ProfileProduction {
			return "ES256"
		}
		return "auto"
	}
}

// JWT signing-key background-refresh defaults and bounds. Mirrors the
// documented values in docs/CONFIGURATION.md.
const (
	defaultJWTKeyRefreshInterval = 60 * time.Second
	minJWTKeyRefreshInterval     = 10 * time.Second
	maxJWTKeyRefreshInterval     = time.Hour
)

// jwtKeyRefreshIntervalFromEnv parses JWT_KEY_REFRESH_INTERVAL. An
// unset or malformed value falls back to defaultJWTKeyRefreshInterval.
// An explicit non-positive value (e.g. "0") disables the background
// refresh and is returned as 0 — appropriate for single-replica
// deployments where rotation already reloads locally. Any positive
// value is clamped to [minJWTKeyRefreshInterval, maxJWTKeyRefreshInterval]
// so an over-eager "1s" cannot hammer the database and a "24h" typo
// cannot effectively defeat cross-replica propagation.
func jwtKeyRefreshIntervalFromEnv() time.Duration {
	s := strings.TrimSpace(os.Getenv("JWT_KEY_REFRESH_INTERVAL"))
	if s == "" {
		return defaultJWTKeyRefreshInterval
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return defaultJWTKeyRefreshInterval
	}
	if d <= 0 {
		return 0
	}
	if d < minJWTKeyRefreshInterval {
		return minJWTKeyRefreshInterval
	}
	if d > maxJWTKeyRefreshInterval {
		return maxJWTKeyRefreshInterval
	}
	return d
}

// normaliseArchivePrefix ensures the prefix ends with exactly one
// trailing slash so callers can concatenate the per-workspace key
// suffix without bookkeeping. Empty input falls back to the default
// "audit-archive/" — never an empty prefix, because writing to the
// bucket root would tangle archive objects with live file objects.
func normaliseArchivePrefix(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "audit-archive/"
	}
	// Strip every trailing slash first, then re-add exactly one,
	// so "audit/", "audit//", and "audit///" all normalise to
	// "audit/".
	s = strings.TrimRight(s, "/")
	if s == "" {
		// Was just slashes — fall back to default rather than
		// returning the bucket root.
		return "audit-archive/"
	}
	return s + "/"
}

// parseBoolDefault parses the common boolean env-var values
// ("1", "true", "yes", "on") case-insensitively. Anything else
// (including the empty string) falls through to def, which
// keeps a typo from silently flipping a security knob.
func parseBoolDefault(s string, def bool) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

// platformAdminUserIDsFromEnv parses the LEGACY PLATFORM_ADMIN_USER_IDS
// env var (comma-separated user UUIDs). It once gated the per-workspace
// admin JWT-rotation endpoint, but rotation moved to the platform
// control plane (POST /api/platform/jwt/rotate, keys:manage) and the
// admin endpoint was removed, so these IDs no longer gate anything. The
// value is still parsed so cmd/server can warn the operator at startup
// that the var is obsolete and can be dropped. Blank and unparseable
// entries are still separated out for the same warning path; the second
// return value carries any entries that failed to parse.
func platformAdminUserIDsFromEnv() (ids []uuid.UUID, invalid []string) {
	raw := parseCSVList(os.Getenv("PLATFORM_ADMIN_USER_IDS"))
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]uuid.UUID, 0, len(raw))
	for _, s := range raw {
		id, err := uuid.Parse(s)
		if err != nil {
			invalid = append(invalid, s)
			continue
		}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil, invalid
	}
	return out, invalid
}

// parseCSVList splits a comma-separated env-var value into a
// trimmed slice, dropping empty entries. Returns nil for empty
// input so the caller's omit-empty default kicks in.
func parseCSVList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseScopeList parses an OAuth2 scope list that may be delimited by
// spaces (the canonical OAuth2 form, "openid email profile") or commas
// ("openid,email,profile"), tolerating either so operators don't have
// to remember which the deployment expects. Returns nil when empty so
// internal/iamcore.Config falls back to its DefaultScopes.
func parseScopeList(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func parseIntDefault(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil || v <= 0 {
		return def
	}
	return v
}

// defaultTrustedProxyDepth is the assumed number of trusted reverse
// proxies in front of the server when TRUSTED_PROXY_DEPTH is unset.
// One matches the common single-load-balancer deployment. The
// IP-allowlist middleware consumes the resolved value; this package
// owns the default because it owns env-var resolution.
const defaultTrustedProxyDepth = 1

// parseNonNegativeIntDefault is like parseIntDefault but treats an
// explicit 0 as a valid value rather than falling back to def. Only an
// unset/empty var, a parse error, or a negative value yields def. This
// matters for TRUSTED_PROXY_DEPTH where 0 ("trust no proxy; use the raw
// peer address") is a meaningful, documented setting distinct from the
// default of 1.
func parseNonNegativeIntDefault(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 {
		return def
	}
	return v
}

// parseFloatDefault parses a non-negative float env-var value. Empty
// strings, parse failures, and negative results fall back to def —
// the typical use is parsing a sampler ratio in [0,1], so silently
// clamping a typo back to the default is preferable to crashing.
// Values above 1.0 are passed through unchanged; the consumer
// (tracing.Init) clamps to the legal sampler range with its own
// startup warning so the limit is documented at one site.
func parseFloatDefault(s string, def float64) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return def
	}
	return v
}

// parseOTELHeaders parses a W3C-style comma-separated list of
// `key=value` pairs into a header map. Matches the upstream
// OTEL_EXPORTER_OTLP_HEADERS conventions, including treating
// surrounding whitespace as ignorable. Returns nil for empty input
// so the consumer can distinguish "no headers configured" from
// "an empty map I should still iterate". Malformed pairs (no `=`,
// empty key, empty value) are skipped rather than crashing — the
// tracing package's startup log warns when the input was non-empty
// but the parsed result is empty so a typo doesn't silently strip
// auth headers from the exporter.
func parseOTELHeaders(s string) map[string]string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	out := make(map[string]string)
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		idx := strings.Index(pair, "=")
		if idx <= 0 || idx == len(pair)-1 {
			continue
		}
		k := strings.TrimSpace(pair[:idx])
		v := strings.TrimSpace(pair[idx+1:])
		if k == "" || v == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
