package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/kennguy3n/zk-drive/api/admin"
	"github.com/kennguy3n/zk-drive/api/auth"
	"github.com/kennguy3n/zk-drive/api/drive"
	apikchat "github.com/kennguy3n/zk-drive/api/kchat"
	"github.com/kennguy3n/zk-drive/api/middleware"
	apiplatform "github.com/kennguy3n/zk-drive/api/platform"
	apiwebhooks "github.com/kennguy3n/zk-drive/api/webhooks"
	"github.com/kennguy3n/zk-drive/api/ws"
	"github.com/kennguy3n/zk-drive/internal/activity"
	"github.com/kennguy3n/zk-drive/internal/ai"
	"github.com/kennguy3n/zk-drive/internal/audit"
	"github.com/kennguy3n/zk-drive/internal/billing"
	"github.com/kennguy3n/zk-drive/internal/changefeed"
	"github.com/kennguy3n/zk-drive/internal/collab"
	"github.com/kennguy3n/zk-drive/internal/config"
	cryptopkg "github.com/kennguy3n/zk-drive/internal/crypto"
	"github.com/kennguy3n/zk-drive/internal/database"
	"github.com/kennguy3n/zk-drive/internal/document"
	"github.com/kennguy3n/zk-drive/internal/email"
	"github.com/kennguy3n/zk-drive/internal/fabric"
	"github.com/kennguy3n/zk-drive/internal/feature"
	"github.com/kennguy3n/zk-drive/internal/file"
	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/health"
	"github.com/kennguy3n/zk-drive/internal/iamcore"
	"github.com/kennguy3n/zk-drive/internal/jobs"
	"github.com/kennguy3n/zk-drive/internal/kchat"
	"github.com/kennguy3n/zk-drive/internal/logging"
	"github.com/kennguy3n/zk-drive/internal/metrics"
	"github.com/kennguy3n/zk-drive/internal/notification"
	"github.com/kennguy3n/zk-drive/internal/permission"
	"github.com/kennguy3n/zk-drive/internal/platform"
	"github.com/kennguy3n/zk-drive/internal/preview"
	"github.com/kennguy3n/zk-drive/internal/responsecache"
	"github.com/kennguy3n/zk-drive/internal/retention"
	"github.com/kennguy3n/zk-drive/internal/search"
	"github.com/kennguy3n/zk-drive/internal/session"
	"github.com/kennguy3n/zk-drive/internal/sharing"
	"github.com/kennguy3n/zk-drive/internal/storage"
	"github.com/kennguy3n/zk-drive/internal/totp"
	"github.com/kennguy3n/zk-drive/internal/tracing"
	"github.com/kennguy3n/zk-drive/internal/user"
	"github.com/kennguy3n/zk-drive/internal/version"
	"github.com/kennguy3n/zk-drive/internal/webhooks"
	"github.com/kennguy3n/zk-drive/internal/wiring"
	"github.com/kennguy3n/zk-drive/internal/workspace"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	logging.Init("server")
	slog.Info("zk-drive server starting", "version", version.Version)

	// --auto-migrate applies pending migrations under the schema
	// advisory lock before the HTTP listener comes up. It ORs with
	// ZKDRIVE_AUTO_MIGRATE (which the compact profile defaults to
	// true) so either the flag or the env var enables it. Production
	// K8s leaves both off and runs the separate migrate Job so schema
	// changes stay decoupled from pod rollout. A dedicated FlagSet
	// (rather than the global flag.CommandLine) keeps server flag
	// parsing self-contained and testable.
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	autoMigrateFlag := fs.Bool("auto-migrate", false, "apply pending database migrations on startup before serving (advisory-locked; safe with multiple replicas)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	autoMigrate := cfg.AutoMigrate || *autoMigrateFlag

	ctx, cancel := context.WithCancel(context.Background())

	// Tracing is initialised BEFORE any other subsystem so spans
	// emitted by database.Connect, the Redis ping, NATS subscribe,
	// and downstream handlers all flow through the same provider.
	// When OTEL_EXPORTER_OTLP_ENDPOINT is unset, tracing.Init
	// returns a no-op provider — span calls remain valid (so
	// instrumented code stays compileable) but nothing exports.
	// LogStartup announces the effective state in the same boot
	// log slot as the database / redis / SMTP "X enabled/disabled"
	// lines so operators see all three pillars at a glance.
	traceProvider, err := tracing.Init(ctx, tracing.BuildFromOperatorConfig(tracing.OperatorConfig{
		Endpoint:              cfg.OTELExporterOTLPEndpoint,
		Headers:               cfg.OTELExporterOTLPHeaders,
		Insecure:              cfg.OTELExporterOTLPInsecure,
		Compression:           cfg.OTELExporterOTLPCompression,
		ServiceName:           cfg.OTELServiceName,
		DeploymentEnvironment: cfg.OTELDeploymentEnvironment,
		SamplerRatio:          cfg.OTELSamplerRatio,
	}, version.Version))
	if err != nil {
		cancel()
		return fmt.Errorf("init tracing: %w", err)
	}
	traceProvider.LogStartup(ctx)
	// Tracer shutdown defers BEFORE pool.Close so any spans
	// emitted from a final pool close (rare but observed in
	// pgxpool's draining path) still reach the exporter. Using a
	// dedicated 10s context so a wedged exporter doesn't block
	// process exit past the existing shutdown grace period.
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		if err := traceProvider.Shutdown(shutCtx); err != nil {
			slog.Warn("tracing shutdown returned error", "err", err)
		}
	}()

	pool, err := database.ConnectWithPool(ctx, cfg.DatabaseURL, database.PoolConfig{
		MaxConns:        cfg.DBMaxConns,
		MinConns:        cfg.DBMinConns,
		MaxConnIdleTime: cfg.DBMaxConnIdleTime,
	})
	if err != nil {
		cancel()
		return fmt.Errorf("connect postgres: %w", err)
	}
	// readPool is the optional read-replica pool (DATABASE_READ_URL).
	// Declared here — like redisClient — so its Close defer registers in
	// the right LIFO position relative to the primary pool teardown. nil
	// when no replica is configured, in which case the ReadWriteSplitter
	// routes reads to the primary.
	var readPool *pgxpool.Pool
	// redisClient is declared here (rather than inline at the Redis
	// init block below) so the Close defer can be registered in the
	// right LIFO position relative to the WaitGroup and pool teardown.
	var redisClient *redis.Client
	// Defer order matters — defers run LIFO, so the target shutdown
	// sequence is:
	//
	//   1. cancel()              — signals long-running goroutines
	//                              (WebSocket hub, Redis pubsub loop)
	//                              to exit.
	//   2. bgGoroutines.Wait     — blocks until those goroutines have
	//                              observed ctx.Done() and returned,
	//                              so no one is mid-Acquire on the
	//                              pool or mid-Subscribe on Redis.
	//   3. redisClient.Close()   — closes Redis AFTER the subscribe
	//                              goroutine has exited; otherwise
	//                              the goroutine sees "redis: client
	//                              is closed" before observing
	//                              ctx.Done() and we log spurious
	//                              shutdown noise.
	//   4. pool.Close()          — closes the pool against a fully
	//                              quiescent set of consumers.
	//
	// Registered in reverse of that order. HTTP server shutdown is
	// handled by srv.Shutdown(shutdownCtx) below — not through this
	// WaitGroup — because chi handlers may still need pool conns to
	// flush their in-flight responses.
	var bgGoroutines sync.WaitGroup
	defer pool.Close()
	defer func() {
		if readPool != nil {
			readPool.Close()
		}
	}()
	defer func() {
		if redisClient != nil {
			_ = redisClient.Close()
		}
	}()
	defer bgGoroutines.Wait()
	defer cancel()

	// When --auto-migrate (or ZKDRIVE_AUTO_MIGRATE / the compact
	// profile) is set, apply pending migrations before the readiness
	// check. database.Migrate serialises on a session-scoped Postgres
	// advisory lock, so even if several server replicas start with
	// auto-migrate enabled they race safely: the first holder applies
	// the schema and the rest block, then observe an up-to-date
	// database and no-op. Default (off) preserves the production
	// contract below — migrations run out-of-band via the migrate
	// Job and the server only verifies the schema is current.
	if autoMigrate {
		start := time.Now()
		if err := database.Migrate(ctx, pool, cfg.MigrationsDir); err != nil {
			return fmt.Errorf("auto-migrate: %w", err)
		}
		slog.Info("auto-migrate completed",
			"duration", time.Since(start).Round(time.Millisecond).String(),
			"migrations_dir", cfg.MigrationsDir,
		)
	}

	// Migrations are applied out-of-band by the `migrate` binary (a
	// Kubernetes Job, a Compose service, or an operator command) so
	// the server pods can scale up / down independently of the
	// schema lifecycle. Here we only verify the database is at or
	// above the minimum version this binary needs; if not we fail
	// fast with a clear error so the orchestrator surfaces "deploy
	// is stale" rather than letting the pod 500 on missing columns.
	if err := database.RequireMinMigrationVersion(ctx, pool); err != nil {
		return fmt.Errorf("startup precondition: %w", err)
	}

	// Optional Postgres read replica. When DATABASE_READ_URL is set and
	// distinct from the primary, open a second pgxpool against it; the
	// ReadWriteSplitter then routes SELECT-family statements to the
	// replica and every mutation / transaction to the primary. Read-heavy
	// repositories (folder tree walks, file listings) are wired against
	// the splitter; everything that does read-your-write work stays on
	// the primary pool. A replica connect failure is fatal — an operator
	// who configured a replica wants to know immediately if it is
	// unreachable rather than silently serving every read off the
	// primary.
	if rd := strings.TrimSpace(cfg.DatabaseReadURL); rd != "" && rd != strings.TrimSpace(cfg.DatabaseURL) {
		// Read pool sizes via the DB_READ_* knobs, which inherit the
		// primary's values when unset (so an un-tuned deployment opens
		// an identically-sized pool, as before). Idle reaping reuses the
		// shared DBMaxConnIdleTime.
		readPool, err = database.ConnectWithPool(ctx, rd, database.PoolConfig{
			MaxConns:        cfg.DBReadMaxConns,
			MinConns:        cfg.DBReadMinConns,
			MaxConnIdleTime: cfg.DBMaxConnIdleTime,
		})
		if err != nil {
			return fmt.Errorf("connect read replica: %w", err)
		}
		slog.Info("postgres read replica connected; SELECT traffic routed to replica",
			"read_url_set", true,
			"read_max_conns", cfg.DBReadMaxConns,
			"read_min_conns", cfg.DBReadMinConns,
		)
	}
	// dbRouter is a Querier: a *ReadWriteSplitter when a replica is
	// configured, otherwise a thin wrapper that routes every read back to
	// the primary (readPool == nil → splitter falls back to primary).
	dbRouter := database.NewReadWriteSplitter(pool, readPool)

	userRepo := user.NewPostgresRepository(pool)
	userSvc := user.NewService(userRepo)

	wsRepo := workspace.NewPostgresRepository(pool)
	wsSvc := workspace.NewService(wsRepo)

	folderRepo := folder.NewPostgresRepository(dbRouter)
	folderSvc := folder.NewService(folderRepo)

	fileRepo := file.NewPostgresRepository(dbRouter)
	fileSvc := file.NewService(fileRepo)

	documentRepo := document.NewPostgresRepository(pool)
	documentSvc := document.NewService(documentRepo, folderSvc)

	// YjsRuntime: a pooled wazero runtime that runs the embedded
	// Rust-compiled wasm yrs CRDT to merge collab updates into a
	// compact single-update snapshot during compaction (the
	// production fold for managed_encrypted folders).
	//
	// Failure to initialise is a hard boot error: the wasm binary
	// is committed to the repo and compiled into the Go artefact,
	// so the only realistic failure modes are a corrupted binary
	// or a wazero compile bug — both of which the operator wants
	// to see immediately at startup, not as silently-broken
	// compaction later.
	//
	// Close ordering: deferred so the runtime survives every
	// in-flight compaction. collabHub.Shutdown (called by the
	// graceful-shutdown handler later in this function) drains
	// in-flight compaction goroutines via its compactWG before
	// returning, so by the time this defer runs no fold is still
	// touching the runtime.
	yjsRuntime, err := collab.NewYjsRuntime(ctx)
	if err != nil {
		return fmt.Errorf("init yjs wasm runtime: %w", err)
	}
	defer func() {
		_ = yjsRuntime.Close(context.Background())
	}()
	slog.Info("yjs wasm runtime ready", "max_instances", collab.DefaultYjsRuntimeMaxInstances)

	// Collab hub: per-document WebSocket rooms for the Yjs editor.
	// Multi-replica fan-out via Redis (see RedisCollabRelay wiring
	// further down). The compaction scheduler runs
	// documentSvc.Compact with the folder-appropriate FoldFunc:
	// YjsMergeFold for managed_encrypted (real CRDT merge via
	// wasm), nil for strict_zk (no server-side fold possible).
	// Wire AFTER documentSvc + yjsRuntime so the hub captures
	// the live service pointer and runtime.
	//
	// The relay is attached further down once the Redis client
	// (if any) is constructed — collabHub.WithRelay is fluent so
	// we set it post-construction. The hub treats a nil relay as
	// single-replica mode.
	collabHub := collab.NewDocumentHub(documentSvc).
		WithCompactionScheduler(func(workspaceID, documentID uuid.UUID) {
			// Detached context: the WS goroutine that triggered
			// us may close mid-fold, but compaction is independent
			// of any single editor session. Bound to the server's
			// lifecycle ctx so graceful shutdown cancels in-flight
			// compactions.
			doc, parent, err := documentSvc.GetMetadata(ctx, workspaceID, documentID)
			if err != nil {
				slog.Warn("collab compaction lookup failed", "err", err, "document_id", documentID)
				return
			}
			fold := collab.FoldFor(collab.FromDocumentCapability(document.ResolveCapability(parent.EncryptionMode)), yjsRuntime)
			if fold == nil {
				// strict_zk: no server-side compaction (see
				// collab.FoldFor doc comment). Drop the signal.
				return
			}
			if _, err := documentSvc.Compact(ctx, workspaceID, documentID, fold); err != nil {
				slog.Warn("collab compaction failed",
					"err", err,
					"workspace_id", workspaceID,
					"document_id", documentID,
					"document_name", doc.Name,
				)
			}
		})

	var storageClient *storage.Client
	if cfg.S3Endpoint != "" {
		storageClient, err = storage.NewClient(storage.Config{
			Endpoint:  cfg.S3Endpoint,
			Bucket:    cfg.S3Bucket,
			AccessKey: cfg.S3AccessKey,
			SecretKey: cfg.S3SecretKey,
		})
		if err != nil {
			return fmt.Errorf("storage client: %w", err)
		}
		slog.Info("storage fallback presigned-URL client wired", "endpoint", cfg.S3Endpoint, "bucket", cfg.S3Bucket)
	} else {
		slog.Info("storage S3_ENDPOINT not set; per-workspace credentials must be provisioned via fabric")
	}

	credentialCodec, err := cryptopkg.LoadFromEnv()
	if err != nil {
		return fmt.Errorf("credential codec: %w", err)
	}
	slog.Info("crypto credential encryption mode", "mode", credentialCodec.Mode())

	// Session-token signing. The KeyManager signs/verifies with ES256
	// when an active asymmetric key exists in jwt_signing_keys
	// (migration 034), and otherwise falls back to HS256 using
	// cfg.JWTSecret. Verification always accepts both, so rotating to
	// ES256 (POST /api/platform/jwt/rotate) never invalidates sessions
	// issued before the cutover. JWT_ALGORITHM forces a mode; the
	// default ("auto") picks ES256-when-available.
	jwtKeyManager, err := cryptopkg.NewKeyManager(
		ctx,
		cryptopkg.NewPostgresSigningKeyStore(pool),
		credentialCodec,
		cfg.JWTSecret,
		cfg.JWTAlgorithm,
	)
	if err != nil {
		cancel()
		return fmt.Errorf("jwt key manager: %w", err)
	}
	slog.Info("jwt signing", "algorithm", jwtKeyManager.Algorithm())

	// Cross-replica key-rotation propagation. RotateKey only reloads
	// the replica that served POST /api/platform/jwt/rotate; every other
	// replica must re-read jwt_signing_keys to learn the new key's
	// public half, or it would 401 tokens signed by it. This loop
	// polls the table every cfg.JWTKeyRefreshInterval (default 60s)
	// and exits on ctx cancellation via the shared bgGoroutines
	// WaitGroup, so shutdown drains it deterministically. A
	// non-positive interval disables it (single-replica deployments).
	if cfg.JWTKeyRefreshInterval > 0 {
		bgGoroutines.Add(1)
		go func() {
			defer bgGoroutines.Done()
			jwtKeyManager.RefreshLoop(ctx, cfg.JWTKeyRefreshInterval)
		}()
		slog.Info("jwt signing-key auto-refresh enabled", "interval", cfg.JWTKeyRefreshInterval)
	}

	storageFactory := storage.NewClientFactory(pool, storageClient, credentialCodec)

	provisioner := fabric.NewProvisioner(pool, fabric.Config{
		ConsoleURL:       cfg.FabricConsoleURL,
		BucketTemplate:   cfg.FabricBucketTemplate,
		DefaultPolicyRef: cfg.FabricDefaultPlacementRef,
		Encryptor:        credentialCodec,
	})
	if cfg.FabricConsoleURL != "" {
		slog.Info("fabric tenant provisioning enabled", "console", cfg.FabricConsoleURL)
	} else {
		slog.Info("fabric console URL not set, signup will skip tenant provisioning")
	}

	// permRepo is constructed once and held so we can attach
	// the DB observer to it (for the zkdrive_db_query_duration
	// histogram) AFTER metricsSurface is built below, without
	// reconstructing the service. The cache layer wraps this
	// same repo via permissionSvc.WithCache, so the
	// observer is attached to the un-cached delegate — i.e. it
	// fires only on cache misses, which is exactly what
	// operators want to measure (the histogram answers "how
	// slow is the post-cache fall-through?").
	permRepo := permission.NewPostgresRepository(pool)
	permissionSvc := permission.NewService(permRepo)
	activitySvc := activity.NewService(activity.NewPostgresRepository(pool))
	defer activitySvc.Close()

	sharingSvc := sharing.NewService(sharing.NewPostgresRepository(pool), wiring.NewPermissionGranter(permissionSvc)).
		WithDisplayResolvers(
			func(ctx context.Context, workspaceID uuid.UUID) (string, error) {
				ws, err := wsSvc.GetByID(ctx, workspaceID)
				if err != nil {
					return "", err
				}
				// nil-without-err is a real data-integrity signal:
				// it means the invite row references a workspace
				// the workspace service can't find (e.g. workspace
				// hard-deleted out from under an outstanding invite,
				// or a foreign-key drift). The Resolve* fallbacks in
				// internal/sharing.Service render a generic "your
				// workspace" string so the email still goes out
				// gracefully — but the operator needs visibility so
				// the drift gets reconciled. Emitting a warn here
				// (instead of upstream in Resolve*) keeps the signal
				// at the actual data-source boundary; the sharing
				// package can't tell "service returned (nil, nil)"
				// apart from "resolver was never wired" without
				// leaking presence-checks into its public surface.
				if ws == nil {
					slog.Warn("guest-invite display resolver: workspace not found",
						"workspace_id", workspaceID)
					return "", nil
				}
				return ws.Name, nil
			},
			func(ctx context.Context, workspaceID, folderID uuid.UUID) (string, error) {
				f, err := folderSvc.GetByID(ctx, workspaceID, folderID)
				if err != nil {
					return "", err
				}
				if f == nil {
					slog.Warn("guest-invite display resolver: folder not found",
						"workspace_id", workspaceID,
						"folder_id", folderID)
					return "", nil
				}
				return f.Name, nil
			},
		)
	searchSvc := search.NewService(pool)
	clientRoomSvc := sharing.NewClientRoomService(
		sharing.NewPostgresClientRoomRepository(pool),
		folderCreatorAdapter{folderSvc},
		sharingSvc,
	)

	// NATS JetStream is optional: when NATS_URL is unset the drive
	// handler gets a nil publisher and ConfirmUpload's job fan-out
	// becomes a no-op. Logging a best-effort connect failure (rather
	// than returning it) keeps the API plane working in local dev
	// stacks that don't run NATS.
	//
	// natsConn is retained even after JetStream is wired so the
	// /readyz deep health-check can observe NATS reachability via
	// nc.Status(). A nil natsConn signals "NATS not configured" to
	// the health checker (which then short-circuits with OK).
	var jobPublisher *jobs.Publisher
	var webhookPublisher *webhooks.Publisher
	var natsConn *nats.Conn
	if natsURL := os.Getenv("NATS_URL"); natsURL != "" {
		nc, nerr := nats.Connect(natsURL,
			nats.Name("zk-drive-server"),
			nats.MaxReconnects(-1),
			nats.ReconnectWait(2*time.Second),
		)
		if nerr != nil {
			slog.Warn("nats connect failed, post-upload jobs disabled", "url", natsURL, "err", nerr)
		} else {
			js, jerr := nc.JetStream()
			if jerr != nil {
				slog.Warn("nats jetstream context failed", "err", jerr)
				nc.Close()
			} else {
				jobPublisher = jobs.NewPublisher(js).WithHeavyBackpressure(cfg.PreviewHeavyQueueBackpressureThreshold)
				webhookPublisher = webhooks.NewPublisher(js)
				natsConn = nc
				slog.Info("nats connected, post-upload jobs enabled", "url", natsURL)
				defer nc.Drain() //nolint:errcheck // best-effort drain
			}
		}
	}

	// Optional Redis client. When REDIS_URL is set, the rate
	// limiter, session store, and WebSocket pub/sub switch to a
	// shared backend so all three behave correctly behind multiple
	// replicas. When unset, the in-memory / single-process
	// implementations are used. redisClient itself is declared with
	// the rest of the teardown plumbing further up so the Close defer
	// runs in the right LIFO position; here we just initialise it.
	var sessionStore *session.RedisSessionStore
	if cfg.RedisURL != "" {
		opts, perr := redis.ParseURL(cfg.RedisURL)
		if perr != nil {
			return fmt.Errorf("parse REDIS_URL: %w", perr)
		}
		redisClient = redis.NewClient(opts)
		// Install the OpenTelemetry redis hook BEFORE the first
		// command (the Ping below). The hook is global to the
		// client — every command issued through redisClient or
		// any tx/pipeline derived from it emits a span tagged
		// with db.system=redis, db.statement (the command),
		// network.peer.{address,port}. When tracing.Init wired
		// the no-op tracer, the hook still runs but allocates
		// nothing, matching the pgx tracer policy at
		// internal/database/postgres.go.
		//
		// Failure to install the hook does NOT abort startup —
		// Redis tracing is observability-grade infrastructure,
		// not a correctness guard. A misconfigured hook surfaces
		// as a one-time warning so the operator sees it without
		// the server failing to boot.
		if herr := redisotel.InstrumentTracing(redisClient); herr != nil {
			slog.Warn("redis tracing hook install failed, redis spans will be missing", "err", herr)
		}
		if perr := redisClient.Ping(ctx).Err(); perr != nil {
			slog.Warn("redis ping failed, continuing with in-memory fallbacks", "url", cfg.RedisURL, "err", perr)
			// Close the broken client right now so it doesn't
			// leak between here and the deferred close, then nil
			// it so downstream code skips Redis-backed features.
			_ = redisClient.Close()
			redisClient = nil
		} else {
			sessionStore = session.NewRedisSessionStore(redisClient)
			slog.Info("redis connected, rate limiter, session store, and ws pub/sub backed by Redis", "url", cfg.RedisURL)
		}
	} else {
		slog.Info("redis REDIS_URL not set, using in-memory rate limiter and single-process ws (single-replica only)")
	}
	// sessionStore (when non-nil) is the SessionChecker the auth
	// middleware consults on every authenticated request to honour
	// out-of-band revocations (logout, password reset, admin
	// force-sign-out). The auth handler also uses it to (a) record
	// per-user logout cutoffs and (b) consult them on Refresh.
	//
	// Both bindings go through `if sessionStore != nil` rather than
	// passing the typed-nil pointer straight into the interface
	// variable / setter — Go's "typed nil interface" trap would
	// otherwise leave h.sessions != nil but its method calls
	// panicking on a nil receiver. nil when REDIS_URL is unset —
	// in that (single-process dev) mode tokens behave like stateless
	// JWTs and only expire on their natural TTL.
	var sessionChecker middleware.SessionChecker
	if sessionStore != nil {
		sessionChecker = sessionStore
	}

	rateLimiter := func() func(http.Handler) http.Handler {
		if redisClient != nil {
			return middleware.RedisRateLimiter(redisClient, middleware.RedisRateLimiterConfig{
				PerUser:      cfg.RateLimitPerUser,
				PerWorkspace: cfg.RateLimitPerWorkspace,
			})
		}
		return middleware.RateLimiter(middleware.RateLimitConfig{
			PerUser:      cfg.RateLimitPerUser,
			PerWorkspace: cfg.RateLimitPerWorkspace,
		})
	}

	// wsProxyMode is the EFFECTIVE proxy decision: the operator asked for
	// it (WS_PROXY_MODE) AND Redis is wired (the API and the external
	// proxy tier communicate through Redis pub/sub). If WS_PROXY_MODE is
	// set without REDIS_URL we cannot reach a proxy, so we log and fall
	// back to the in-process hub rather than silently dropping every
	// real-time event.
	wsProxyMode := cfg.WSProxyMode && redisClient != nil
	if cfg.WSProxyMode && redisClient == nil {
		slog.Warn("WS_PROXY_MODE set but REDIS_URL is empty; falling back to in-process WebSocket fan-out")
	}

	// WebSocket hub fans real-time notification events to connected
	// clients. The hub itself is always in-process; when redisClient
	// is non-nil we additionally subscribe to ws:* so notifications
	// produced on any replica reach every replica's clients. The
	// notification service receives the publisher and is nil-safe,
	// so a misconfigured Redis still leaves the rest of the API
	// working — we just log and fall back to local fan-out.
	//
	// In WS proxy mode the in-process hub holds no client connections
	// (an external Centrifugo/Pusher tier does), so we still construct
	// and Run it — it is the LocalBroadcaster the web-push wrapper and
	// the IsConnected presence check expect — but we never subscribe it
	// to Redis. Events flow API → Redis (ws:* channels) → external proxy
	// → clients.
	hub := ws.NewHub()
	bgGoroutines.Add(1)
	go func() {
		defer bgGoroutines.Done()
		hub.Run(ctx)
	}()
	wsHandler := ws.NewHandler(hub)

	var notificationPublisher notification.WSPublisher = notification.NewLocalPublisher(hub)
	if redisClient != nil {
		rp := notification.NewRedisPublisher(redisClient)
		notificationPublisher = rp
		if wsProxyMode {
			slog.Info("WS proxy mode enabled; publishing real-time events to Redis ws:* for the external proxy tier (in-process /api/ws disabled)")
		} else {
			// Single in-process fan-out tier: subscribe the local hub to
			// ws:* so events produced on any replica reach this replica's
			// own clients. Skipped in proxy mode (the external proxy is
			// the subscriber).
			bgGoroutines.Add(1)
			go func() {
				defer bgGoroutines.Done()
				if err := rp.Subscribe(ctx, hub); err != nil && !errors.Is(err, context.Canceled) {
					slog.Error("redis ws subscribe loop exited", "err", err)
				}
			}()
		}
	}

	// notifRepo backs both the notification service and the web-push
	// service; they read/write the same Postgres tables via the shared
	// pool, so one repository instance is reused rather than allocating
	// a second identical wrapper. The credential codec encrypts the
	// web-push p256dh / auth key material at rest (no-op pass-through
	// when CREDENTIAL_ENCRYPTION is "none").
	notifRepo := notification.NewPostgresRepository(pool).
		WithSubscriptionCipher(credentialCodec)

	// Web Push (RFC 8030 + VAPID). Constructed only when both VAPID
	// keys are configured; otherwise webPushSvc stays nil and the
	// /api/push/* endpoints respond 501. When enabled, wrap the
	// notification publisher so a "notification" event for a user
	// with no live WebSocket connection on this replica also fans out
	// as a browser push message.
	var webPushSvc *notification.WebPushService
	if cfg.WebPushEnabled() {
		webPushSvc = notification.NewWebPushService(
			notifRepo,
			cfg.VAPIDPublicKey, cfg.VAPIDPrivateKey,
		).WithSubscriber(cfg.VAPIDSubscriber).
			WithEndpointValidator(webhooks.NewURLValidator())
		notificationPublisher = notification.NewWebPushPublisher(notificationPublisher, hub, webPushSvc).
			WithWaitGroup(&bgGoroutines)
		slog.Info("web push enabled, offline notifications will fan out via VAPID")
		if wsProxyMode {
			// In proxy mode the in-process hub holds no client
			// connections, so the replica-local IsConnected presence
			// check always reports "not connected" and the web-push
			// fallback fires for EVERY notification — even for users
			// with a live socket on the external proxy tier. Operators
			// who enable both will see push volume spike. The fix is to
			// drive presence from the proxy (Centrifugo exposes
			// presence); warn loudly so this is not a silent surprise.
			// See deploy/WEBSOCKET_PROXY.md ("Presence / web-push
			// fallback").
			slog.Warn("WS_PROXY_MODE and web push are both enabled: in-process presence is always 'not connected' in proxy mode, so web push fires for every notification (including users connected to the proxy). Drive presence from the proxy tier to suppress redundant pushes — see deploy/WEBSOCKET_PROXY.md")
		}
	} else {
		slog.Warn("web push disabled (VAPID_PUBLIC_KEY / VAPID_PRIVATE_KEY unset), /api/push/* will respond 501")
	}

	// Native mobile push (APNs for iOS, FCM for Android). Each provider is
	// built only when its credentials are configured; a build failure
	// disables that provider (logged) without aborting startup. When at
	// least one provider is configured, wrap the publisher so a
	// "notification" event also fans out to the recipient's phones, and
	// expose POST/DELETE /api/push/register-device. Wrap order is
	// inner → WebPush → Mobile, so one publish reaches WebSocket, Web Push
	// and native push.
	mobilePushSvc := notification.NewMobilePushService(notifRepo)
	if cfg.FCMConfigured() {
		saJSON, err := loadCredentialMaterial(cfg.FCMServiceAccountJSON, cfg.FCMServiceAccountFile)
		if err != nil {
			slog.Error("fcm push disabled: cannot read service account", "err", err)
		} else if fcm, err := notification.NewFCMProvider(saJSON); err != nil {
			slog.Error("fcm push disabled: invalid service account", "err", err)
		} else {
			mobilePushSvc.WithProvider(fcm)
			slog.Info("fcm push enabled, Android notifications will fan out via FCM HTTP v1")
		}
	}
	if cfg.APNsConfigured() {
		p8, err := loadCredentialMaterial(cfg.APNsAuthKey, cfg.APNsAuthKeyFile)
		if err != nil {
			slog.Error("apns push disabled: cannot read auth key", "err", err)
		} else if apns, err := notification.NewAPNsProvider(p8, cfg.APNsKeyID, cfg.APNsTeamID, cfg.APNsTopic, cfg.APNsProduction); err != nil {
			slog.Error("apns push disabled: invalid auth key", "err", err)
		} else {
			mobilePushSvc.WithProvider(apns)
			slog.Info("apns push enabled, iOS notifications will fan out via APNs", "production", cfg.APNsProduction)
		}
	}
	if mobilePushSvc.Enabled() {
		notificationPublisher = notification.NewMobilePushPublisher(notificationPublisher, mobilePushSvc).
			WithWaitGroup(&bgGoroutines)
	} else {
		// Keep nil so drive's WithMobilePush sees a disabled service and the
		// register endpoint responds 501.
		mobilePushSvc = nil
		slog.Warn("mobile push disabled (no FCM / APNs credentials), /api/push/register-device will respond 501")
	}

	notificationSvc := notification.NewService(notifRepo).
		WithPublisher(notificationPublisher)

	// Change-feed service. Powers the GET /api/changes catch-up
	// endpoint and the workspace-wide WS push consumed by the
	// desktop sync SDK. The publisher path mirrors notifications:
	// fall back to LocalPublisher in single-replica deployments,
	// upgrade to RedisPublisher when REDIS_URL is configured so
	// every replica's hub gets every workspace's change events.
	var changefeedPublisher changefeed.WSPublisher = changefeed.NewLocalPublisher(hub)
	if redisClient != nil {
		cfRP := changefeed.NewRedisPublisher(redisClient)
		changefeedPublisher = cfRP
		// Mirror the notification publisher above: keep publishing change
		// events to Redis (the external proxy tier consumes them), but in
		// proxy mode do NOT subscribe the in-process hub — the /api/ws
		// endpoint is disabled there so the hub holds no changefeed
		// clients, and a local subscribe loop would only burn Redis I/O
		// and hub-mutex time delivering to nobody.
		if !wsProxyMode {
			bgGoroutines.Add(1)
			go func() {
				defer bgGoroutines.Done()
				if err := cfRP.Subscribe(ctx, hub); err != nil && !errors.Is(err, context.Canceled) {
					slog.Error("redis changefeed subscribe loop exited", "err", err)
				}
			}()
		}
	}
	changefeedSvc := changefeed.NewService(changefeed.NewPostgresRepository(pool)).
		WithPublisher(changefeedPublisher)

	// Collab relay: Redis pub/sub multi-replica fan-out for the
	// document collab hub. Mirrors the notification / changefeed
	// publisher pattern (PSubscribe on `collab:*`, parse the
	// document UUID, deliver to local room members via
	// hub.BroadcastFromRelay). When redisClient is nil, the relay
	// constructor returns nil and the hub treats that as single-
	// replica mode (PublishFrame is a no-op).
	collabRelay := collab.NewRedisCollabRelay(redisClient)
	// Guard the WithRelay call with an explicit nil check: a
	// nil *RedisCollabRelay wrapped in the CollabRelayPublisher
	// interface produces a non-nil interface value whose
	// underlying pointer is nil. Without this guard the hub
	// would observe `h.relay != nil` and call PublishFrame on
	// every sync update, only for PublishFrame to no-op on the
	// nil receiver path. Skipping WithRelay entirely keeps the
	// hub's relay==nil fast path cleanly engaged in single-
	// replica mode.
	if collabRelay != nil {
		collabHub.WithRelay(collabRelay)
		bgGoroutines.Add(1)
		go func() {
			defer bgGoroutines.Done()
			if err := collabRelay.Subscribe(ctx, collabHub); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("redis collab relay subscribe loop exited", "err", err)
			}
		}()
		slog.Info("collab redis relay active, multi-replica collab fan-out enabled")
	}

	// emailSvc owns transactional email delivery (guest-invite
	// notifications today; future password-reset / MFA notices
	// follow the same call shape). When SMTP_HOST is unset the
	// service boots into a NoopClient mode so dev environments
	// keep working — LogStartup emits a single startup warning
	// so operators see the disabled state at deploy time, not
	// when their first invitee fails to receive an email.
	emailSvc, err := buildEmailService(cfg)
	if err != nil {
		return fmt.Errorf("build email service: %w", err)
	}
	emailSvc.LogStartup(ctx)

	previewRepo := preview.NewPostgresRepository(pool)
	auditSvc := audit.NewService(audit.NewPostgresRepository(pool))
	defer auditSvc.Close()
	retentionSvc := retention.NewService(retention.NewPostgresRepository(pool), pool)

	totpRepo := totp.NewPostgresRepository(pool)
	// Issuer is the human-readable label rendered by authenticator
	// apps (e.g. "zk-drive" → Google Authenticator shows
	// "zk-drive:alice@example.com"). Hardcoded rather than env-
	// driven because changing it would break every already-enrolled
	// user's authenticator entry (the entry is keyed on issuer +
	// account label).
	totpSvc := totp.NewService(totpRepo, credentialCodec, "zk-drive")

	authHandler := auth.NewHandler(pool, userSvc, wsSvc, cfg.JWTSecret).
		WithAudit(auditSvc).
		WithSigner(jwtKeyManager).
		WithTOTP(totpSvc).
		WithPostSignupHook(func(ctx context.Context, workspaceID uuid.UUID, workspaceName string) {
			// Best-effort: provision a fabric tenant for the new
			// workspace. Errors are logged and swallowed so signup
			// stays durable even when the console is unreachable.
			if cfg.FabricConsoleURL == "" {
				return
			}
			if _, err := provisioner.Provision(ctx, workspaceID, workspaceName); err != nil {
				slog.Error("fabric provision workspace failed", "workspace_id", workspaceID, "err", err)
				return
			}
			storageFactory.Invalidate(workspaceID)
			slog.Info("fabric provisioned workspace", "workspace_id", workspaceID)
		})
	if sessionStore != nil {
		authHandler = authHandler.WithSessionRevoker(sessionStore)
	}
	oauthHandler := auth.NewOAuthHandler(authHandler, auth.OAuthConfig{
		GoogleClientID:        cfg.GoogleClientID,
		GoogleClientSecret:    cfg.GoogleClientSecret,
		GoogleRedirectURL:     cfg.GoogleRedirectURL,
		MicrosoftClientID:     cfg.MicrosoftClientID,
		MicrosoftClientSecret: cfg.MicrosoftClientSecret,
		MicrosoftRedirectURL:  cfg.MicrosoftRedirectURL,
	}).WithAudit(auditSvc)

	// Authentication front-end for the data plane (/api/* drive,
	// admin, kchat, and the WebSocket upgrade). Defaults to the
	// built-in session-JWT middleware; when iam-core is configured
	// (IAM_CORE_ISSUER_URL set) it is replaced by the OIDC middleware,
	// which binds the identical (workspaceID, userID, role) request
	// context so every downstream handler and guard works unchanged.
	// The /api/auth password routes are simultaneously swapped for an
	// SSO-only responder so password authentication cannot be used
	// while an external IdP is the source of truth.
	dataPlaneAuth := middleware.AuthMiddlewareWithKeys(jwtKeyManager, sessionChecker)
	var iamCoreClient *iamcore.Client
	if cfg.IAMCoreIssuerURL != "" {
		client, err := iamcore.NewClient(ctx, iamcore.Config{
			IssuerURL:    cfg.IAMCoreIssuerURL,
			ClientID:     cfg.IAMCoreClientID,
			ClientSecret: cfg.IAMCoreClientSecret,
			Audience:     cfg.IAMCoreAudience,
			Scopes:       cfg.IAMCoreScopes,
			CallbackURL:  cfg.IAMCoreCallbackURL,
		}, nil)
		if err != nil {
			cancel()
			return fmt.Errorf("iam-core client: %w", err)
		}
		iamCoreClient = client
		iamCoreMW := iamcore.NewMiddleware(client.NewVerifier(), iamcore.NewTenantMapper(pool, wsSvc), userSvc).
			WithAudit(auditSvc)
		dataPlaneAuth = iamCoreMW.Handler
		slog.Info("auth provider: iam-core OIDC", "issuer", client.Discovery().Issuer, "client_id", cfg.IAMCoreClientID)
		if cfg.IAMCoreAudience != "" {
			slog.Info("iam-core access-token audience validation enabled", "audience", cfg.IAMCoreAudience)
		} else {
			slog.Warn("iam-core IAM_CORE_AUDIENCE not set: access-token audience validation is DISABLED — a token minted for another relying party in the same iam-core tenant could be replayed against zk-drive; set IAM_CORE_AUDIENCE in production")
		}
	} else {
		slog.Info("auth provider: built-in (password + optional Google/Microsoft SSO)")
	}

	billingRepo := billing.NewPostgresRepository(pool)
	billingSvc := billing.NewService(billingRepo)
	// Progressive feature disclosure: the active feature set for a
	// workspace is its tier defaults (resolved from the billing plan)
	// with per-workspace overrides layered on top. Backs GET /api/features.
	featureSvc := feature.NewService(
		feature.NewPostgresRepository(pool),
		feature.NewBillingTierResolver(billingSvc),
	)
	stripeService := billing.NewStripeService(
		billingSvc,
		billingRepo,
		cfg.StripeWebhookSecret,
		cfg.StripeSecretKey,
		cfg.StripePriceTierMap,
	)
	if cfg.StripeWebhookSecret != "" {
		slog.Info("billing stripe webhook signature verification enabled")
	} else {
		slog.Warn("billing STRIPE_WEBHOOK_SECRET not set, /api/webhooks/stripe will reject all requests")
	}
	if cfg.StripeSecretKey != "" {
		slog.Info("billing stripe checkout / portal session creation enabled")
	} else {
		slog.Warn("billing STRIPE_SECRET_KEY not set, /api/admin/billing/{checkout,portal}-session will respond 501")
	}
	// Workspace-scoped response cache for hot read endpoints (folder
	// listings, search results, storage-usage aggregation). Nil-safe:
	// responsecache.New returns a nil *Cache when Redis is unconfigured,
	// and every call site treats that as a permanent miss (compute on
	// every request). Invalidation is wired through the changefeed
	// content-cache buster below so any persisted mutation invalidates
	// the affected workspace immediately; per-entry TTLs are the
	// backstop. A single instance is shared across handlers so they
	// observe the same generation counters.
	respCache := responsecache.New(redisClient)
	if respCache.Enabled() {
		slog.Info("response cache enabled (folder listings, search, storage usage)")
	}

	driveHandler := drive.NewHandler(pool, wsSvc, folderSvc, fileSvc, userSvc, storageClient, permissionSvc, activitySvc).
		WithStorageFactory(storageFactory).
		WithResponseCache(respCache).
		WithDocuments(documentSvc).
		WithCollab(collabHub).
		WithSharing(sharingSvc).
		WithSearch(searchSvc).
		WithClientRooms(clientRoomSvc).
		WithJobs(jobPublisher).
		WithNotifications(notificationSvc).
		WithWebPush(webPushSvc).
		WithMobilePush(mobilePushSvc).
		WithChangefeed(changefeedSvc).
		WithEmail(emailSvc).
		WithPreviews(previewRepo).
		WithAudit(auditSvc).
		WithBilling(billingSvc).
		WithFeatures(featureSvc).
		WithWebhooks(webhookPublisher).
		WithOnlyOffice(cfg.OnlyOfficeURL, cfg.OnlyOfficeSecret, cfg.PublicURL).
		WithOnlyOfficeSaveLimits(cfg.OnlyOfficeSaveMemoryBudgetBytes, cfg.OnlyOfficeMaxDocumentBytes).
		WithOnlyOfficeStreamSaveConcurrency(cfg.OnlyOfficeStreamSaveMaxConcurrent)
	if cfg.OnlyOfficeURL != "" {
		// 0 == unlimited: the constant-memory streaming path is
		// intentionally unbounded unless an operator opts into a cap.
		streamCap := "unlimited"
		if cfg.OnlyOfficeStreamSaveMaxConcurrent > 0 {
			streamCap = strconv.Itoa(cfg.OnlyOfficeStreamSaveMaxConcurrent)
		}
		slog.Info("onlyoffice save concurrency cap derived from memory budget",
			"max_concurrent_saves", cfg.OnlyOfficeMaxConcurrentSaves(),
			"save_memory_budget_mb", cfg.OnlyOfficeSaveMemoryBudgetBytes>>20,
			"max_document_mb", cfg.OnlyOfficeMaxDocumentBytes>>20,
			"stream_save_max_concurrent", streamCap)
		if cfg.OnlyOfficeSecret != "" {
			slog.Info("onlyoffice integration enabled with callback JWT verification")
		} else {
			slog.Warn("onlyoffice ONLYOFFICE_SECRET not set: editor-callback JWT verification is disabled, so a forged callback could make the server fetch an attacker-supplied url (SSRF) — set ONLYOFFICE_SECRET outside trusted local dev")
		}
		if cfg.PublicURL == "" {
			slog.Warn("onlyoffice ONLYOFFICE_URL set but PUBLIC_URL empty: the editor callbackUrl resolves to a relative path the Document Server cannot reach, so save callbacks will fail and edits will be lost on editor close — set PUBLIC_URL to ZK Drive's externally reachable base URL")
		}
	}
	var fabricClient admin.FabricClient
	if cfg.FabricConsoleURL != "" {
		fabricClient = fabric.NewClient(fabric.ClientConfig{
			BaseURL:    cfg.FabricConsoleURL,
			AdminToken: cfg.FabricConsoleAdminToken,
		})
	}
	// Per-workspace IP allowlisting (conditional access). The
	// service caches the allowlist in Redis when available
	// (redisClient may be nil — caching simply disabled) and reads
	// through to Postgres otherwise. Shared between the admin CRUD
	// handler and the enforcement middleware mounted on the
	// authenticated route group below.
	// Pass a true nil interface (not a typed-nil *redis.Client) when
	// Redis is unconfigured so the service's nil-check disables
	// caching cleanly instead of dereferencing a nil client.
	var ipAllowRedis redis.UniversalClient
	if redisClient != nil {
		ipAllowRedis = redisClient
	}
	ipAllowSvc := workspace.NewIPAllowService(workspace.NewPostgresIPAllowStore(pool), ipAllowRedis)

	adminHandler := admin.NewHandler(pool, userSvc, auditSvc, retentionSvc).
		WithBilling(billingSvc).
		WithStripe(stripeService).
		WithFabric(fabricClient, provisioner, storageFactory).
		WithWorkspaces(wsSvc).
		WithWebhooks(webhookPublisher).
		WithIPAllow(ipAllowSvc).
		WithResponseCache(respCache)

	// Platform control plane (Session 9): fleet-wide tenant management
	// authenticated by platform API keys, mounted under /api/platform
	// SEPARATELY from the workspace JWT chain. The same service backs
	// the suspended-workspace 503 guard applied to the tenant groups
	// below.
	platformKeyStore := platform.NewAPIKeyStore(pool)
	platformSvc := platform.NewService(pool, wsSvc, userSvc, billingSvc).
		WithProvisioner(provisioner).
		WithURLValidator(webhooks.NewURLValidator())
	if sessionStore != nil {
		platformSvc = platformSvc.WithSessions(sessionStore)
	}
	// JWT signing-key rotation is a fleet-wide operation (the platform
	// signing key has workspace_id IS NULL), so it lives on the platform
	// control plane behind the keys:manage capability rather than the
	// per-workspace admin API. The PLATFORM_ADMIN_USER_IDS allowlist that
	// origin/main's #100 added to gate the (now-removed) admin-API
	// rotation is therefore redundant here: the platform key's
	// keys:manage capability already restricts rotation to fleet
	// operators, so we surface a startup hint if the legacy env var is
	// still set rather than silently ignoring it.
	if len(cfg.PlatformAdminUserIDs) > 0 || len(cfg.PlatformAdminUserIDsInvalid) > 0 {
		slog.Warn("PLATFORM_ADMIN_USER_IDS is set but no longer used: JWT key rotation moved to the platform control plane (POST /api/platform/jwt/rotate), gated by the keys:manage platform-API-key capability")
	}
	platformHandler := apiplatform.NewHandler(platformSvc, platformKeyStore).
		WithJWTRotator(jwtKeyManager)

	// Outbound-webhook subscription admin handler. Mounted
	// under /api/admin/webhooks so it inherits the admin-only +
	// tenant-guarded middleware stack. CRUD on subscriptions still
	// works without NATS; POST /test additionally requires a
	// TestDispatcher (which only needs the repo + a DeliveryClient,
	// not JetStream — tests dispatch synchronously to the single
	// targeted subscription, see internal/webhooks/test_dispatch.go).
	webhookRepo := webhooks.NewPostgresRepository(pool)
	// A single URLValidator is shared between (a) the admin
	// handler's create-time SSRF check and (b) the TestDispatcher's
	// per-delivery DNS-rebinding re-check. Both run inside the same
	// API process and the validator is stateless (no caches, no
	// connection pools), so a single instance is functionally
	// identical to two independent instances. The previous
	// arrangement created one via NewHandler's default constructor
	// and a second inline at NewDeliveryClient(NewURLValidator(),
	// ...); a future change to validator defaults (e.g. adding an
	// allow-list flag) would have had to land in both places to
	// avoid drift. Sharing here eliminates that coupling risk. The
	// worker (cmd/worker/main.go) still constructs its own validator
	// because it runs in a separate process.
	webhookValidator := webhooks.NewURLValidator()
	webhookTester, err := webhooks.NewTestDispatcher(webhookRepo, webhooks.NewDeliveryClient(webhookValidator, webhooks.DefaultDeliveryTimeout))
	if err != nil {
		return fmt.Errorf("webhooks/test-dispatcher: %w", err)
	}
	webhookHandler := apiwebhooks.NewHandler(webhookRepo).
		WithPublisher(webhookPublisher).
		WithTestDispatcher(webhookTester).
		WithValidator(webhookValidator).
		WithAudit(auditSvc)

	kchatSvc := kchat.NewRoomService(
		kchat.NewPostgresRepository(pool),
		wiring.NewKChatFolderCreator(folderSvc),
		wiring.NewKChatPermissionGranter(permissionSvc),
		wiring.NewKChatFileCreator(fileSvc),
		wiring.NewKChatPresignResolver(storageFactory),
		wiring.KChatObjectKey,
		wiring.KChatObjectKeyValidator,
	)
	summarySvc := ai.NewSummaryService(pool).WithLanguageResolver(wsSvc)
	// Auto-tag suggestions and query expansion share the same
	// privacy-respecting LLM pattern: rule-based scaffold floor,
	// optional on-device LLM refinement when OLLAMA_URL is set.
	// All three services share the same workspace language
	// resolver so the prompt language stays consistent across
	// summarisation, tagging, and expansion within a workspace.
	tagSuggestSvc := ai.NewSuggestionService(pool).WithLanguageResolver(wsSvc)
	queryExpandSvc := ai.NewExpansionService(pool).WithLanguageResolver(wsSvc)
	if cfg.OllamaURL != "" {
		llm, err := ai.NewOllamaClient(cfg.OllamaURL, cfg.OllamaModel)
		if err != nil {
			return fmt.Errorf("ai/ollama: %w", err)
		}
		summarySvc = summarySvc.WithLLM(llm)
		tagSuggestSvc = tagSuggestSvc.WithLLM(llm)
		queryExpandSvc = queryExpandSvc.WithLLM(llm)
		slog.Info("ai local LLM enabled", "endpoint", cfg.OllamaURL, "model", llm.Model())
	} else {
		slog.Info("ai OLLAMA_URL not set, AI summaries / tag suggestions / query expansion use rule-based scaffold (no external API calls)")
	}
	driveHandler = driveHandler.
		WithTagSuggester(tagSuggestSvc).
		WithQueryExpander(queryExpandSvc).
		// The ONLYOFFICE save callback runs outside the session-auth /
		// SuspensionGuard group (the Document Server holds no JWT), so
		// give the handler the same suspension checker to re-enforce the
		// freeze at the write boundary. Wired here rather than at the
		// initial NewHandler chain above because platformSvc is built
		// after it. The fail-closed posture mirrors SuspensionGuard so
		// the callback write boundary and the REST/WS middleware agree.
		WithSuspensionChecker(platformSvc).
		WithSuspensionFailClosed(cfg.SuspensionFailClosed)
	if cfg.SuspensionFailClosed {
		slog.Info("workspace suspension enforcement is fail-CLOSED: a suspension-lookup error returns 503 (compliance-hold posture)")
	}
	kchatHandler := apikchat.NewHandler(kchatSvc, summarySvc)

	// metrics owns a private prometheus.Registry, the HTTP
	// middleware, and the pgxpool / redis pool collectors.
	// /metrics is mounted at the root alongside /healthz and
	// /readyz so an operator scraping the server gets the full
	// triad (liveness + readiness + telemetry) from one process
	// without an extra port to firewall.
	metricsSurface := metrics.New()
	metricsSurface.RegisterPgxPoolCollector(pool)
	metricsSurface.RegisterRedisPoolCollector(redisClient)
	// email.Service.WithMetrics uses a pointer receiver and
	// returns the same *Service for ergonomic chaining; we use it
	// as a fluent setter (return value intentionally discarded —
	// the s.metrics field is already mutated through the pointer)
	// because emailSvc was constructed inside buildEmailService
	// where the metrics surface is not yet available. Reassigning
	// would be a no-op staticcheck/SA4006 (same pointer comes back),
	// so we keep the discard form.
	emailSvc.WithMetrics(metricsSurface)

	// Install DB query observer on the permission repository
	// for the zkdrive_db_query_duration_seconds histogram +
	// zkdrive_db_queries_total counter. The observer is on
	// the *un-cached* delegate (held in permRepo above), so
	// every recorded query corresponds to a cache miss when
	// the cache is enabled — letting operators measure the
	// effective hit ratio by comparing
	// zkdrive_cache_ops_total{op="read",result="miss"} against
	// zkdrive_db_queries_total{op="permission.check_access_with_inheritance"}.
	permRepo.WithObserver(metricsSurface)

	// Wire the Redis-backed read-through cache in front of
	// permission resolution. Default-on in production
	// (cfg.PerformanceCacheEnabled defaults to true) but
	// silently no-ops when REDIS_URL is unset — a multi-
	// replica deployment without Redis can't safely cache
	// permission resolutions (per-replica drift), so the
	// safer behaviour is to skip caching altogether and serve
	// from Postgres on every check. The cache invalidation
	// hook is wired below via changefeedSvc.WithCacheBuster
	// so any permission / folder-topology mutation persisted
	// through the changefeed triggers a workspace-scoped
	// invalidation before the WS broadcast lands. Cache
	// observability lives in zkdrive_cache_ops_total (see
	// internal/metrics/cache.go).
	if cfg.PerformanceCacheEnabled && redisClient != nil {
		// ORDERING DEPENDENCY: WithCache MUST precede
		// WithCacheBuster.
		//
		// permissionSvc.BustWorkspace internally does
		// s.repo.(*CachedRepository) — if WithCache hasn't yet
		// wrapped the un-cached *PostgresRepository, the
		// assertion fails and BustWorkspace becomes a silent
		// no-op. The changefeed would then keep firing busts
		// against a permissionSvc that doesn't know about the
		// cache, leaving stale entries to expire only via TTL
		// (~30s) — a correctness regression that produces no
		// compile error and no runtime panic.
		//
		// We keep both calls inside the same `if` block so
		// they can't be split independently, and the comment
		// below pins the dependency for future refactors.
		permissionSvc.WithCache(redisClient, cfg.PerformanceCacheTTL, metricsSurface)
		changefeedSvc.WithCacheBuster(permissionSvc) // must follow WithCache; see ORDERING DEPENDENCY above
		slog.Info("permission cache enabled",
			"ttl", cfg.PerformanceCacheTTL,
		)
	} else {
		slog.Info("permission cache disabled",
			"flag_enabled", cfg.PerformanceCacheEnabled,
			"redis_configured", redisClient != nil,
		)
	}

	// Wire the content response cache (folder listings, search) onto the
	// changefeed so every persisted mutation invalidates the affected
	// workspace's generation before the WS broadcast lands. Independent
	// of the permission-cache flag/ordering above: the content cache has
	// no WithCache-before-buster dependency (it keys directly off Redis),
	// so it is wired whenever Redis is configured. A disabled (nil) cache
	// leaves this unset and the listings recompute every request.
	if respCache.Enabled() {
		changefeedSvc.WithContentCacheBuster(respCache)
	}

	r := chi.NewRouter()
	// chimw.RequestID is intentionally omitted: the request_id
	// and the request-scoped *slog.Logger are seeded by
	// logging.AccessLog at the http.Server.Handler boundary
	// (see srv := &http.Server{Handler: logging.AccessLog(r)}
	// below). Installing chimw.RequestID here would generate a
	// SECOND id inside the chi router that diverges from the
	// one AccessLog already attached, breaking correlation
	// between the access log line and handler-emitted logs.
	// AccessLog also writes the chosen id to chimw.RequestIDKey
	// so handlers calling chimw.GetReqID continue to get the
	// right value.
	r.Use(chimw.RealIP)
	// otelChiSpanRenamer reads the fully-resolved chi route
	// pattern AFTER dispatch (via defer) and renames the
	// otelhttp-created span (whose initial name is "http.server")
	// to `GET /api/files/{id}` style bounded-cardinality names.
	// Chi populates RoutePattern() incrementally through nested
	// sub-routers, so reading must happen post-dispatch — the
	// same timing used by metrics.HTTPMiddleware and AccessLog.
	// Without this rename, a backend would see millions of
	// distinct `/api/files/<uuid>` spans instead of one
	// `GET /api/files/{id}` aggregate.
	r.Use(otelChiSpanRenamer)
	// SecurityHeaders runs BEFORE Recoverer so a panic in a
	// downstream handler still produces a 500 page with the
	// hardened header set — the Recoverer otherwise writes its
	// own response and we'd lose CSP / HSTS / X-Frame-Options
	// on the exact responses an attacker is most likely to probe.
	r.Use(middleware.SecurityHeaders(middleware.SecurityHeadersOptions{
		CSPReportOnly:   cfg.SecurityHeadersCSPReportOnly,
		CSPReportURI:    cfg.SecurityHeadersCSPReportURI,
		CSPConnectExtra: cfg.SecurityHeadersCSPConnectExtra,
		CSPImgExtra:     cfg.SecurityHeadersCSPImgExtra,
		DisableHSTS:     cfg.SecurityHeadersDisableHSTS,
	}))
	// HTTP metrics middleware runs BEFORE Recoverer so the
	// resulting chain is HTTPMiddleware(Recoverer(handler)).
	// HTTPMiddleware wraps the response writer in a chi
	// WrapResponseWriter and threads that wrapped writer down
	// to Recoverer, so Recoverer's 500-on-panic response is
	// observed via the wrapper and the post-dispatch metric
	// emission sees status="500". The reversed ordering would
	// leave Recoverer writing 500 to the original (unwrapped)
	// writer, which is invisible to ww.Status() — silently
	// dropping panicked requests from the metrics surface.
	// HTTPMiddleware additionally emits its counters from a
	// defer so even a panic that escapes Recoverer (e.g.
	// http.ErrAbortHandler, which Recoverer re-panics) still
	// records. RoutePattern is read off chi.RouteContext
	// post-dispatch — that's the bounded cardinality guard
	// documented in internal/metrics/http.go.
	r.Use(metricsSurface.HTTPMiddleware)
	r.Use(chimw.Recoverer)

	// /healthz is a SHALLOW liveness probe: "the process is alive
	// and HTTP is up." It never pings downstream dependencies
	// because k8s interprets a failing liveness probe as "restart
	// the pod," which is the wrong response to a transient Redis
	// / Postgres outage.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"version": version.Version,
		})
	})

	// /readyz is the DEEP readiness probe — it actually pings every
	// configured downstream dependency under a per-check timeout.
	// k8s should map this to the readiness probe so a Redis / NATS /
	// S3 outage takes the affected pod out of the service mesh
	// without triggering a restart loop. See internal/health for
	// the per-check contract.
	r.Get("/readyz", health.NewService(
		[]health.Checker{
			health.NewPostgresChecker(pool),
			health.NewRedisChecker(redisClient),
			health.NewStorageChecker(storageClient),
			health.NewNATSChecker(natsConn),
		},
		health.DefaultCheckTimeout,
	).ReadyHandler())

	// /metrics is the Prometheus scrape surface. Series name
	// inventory (same metric vectors are registered on every
	// binary via metrics.New, but only some of them have non-zero
	// observations on each binary):
	//   - go_*  / process_*               (default collectors)
	//   - zkdrive_http_*                  (server-side HTTP — populated here)
	//   - zkdrive_db_pool_*               (pgxpool live stats — populated here)
	//   - zkdrive_redis_pool_*            (redis client pool — populated here, if enabled)
	//   - zkdrive_worker_*                (registered with zero data here; populated on the worker's :9091 surface)
	//   - zkdrive_reconciler_*            (registered with zero data here; populated on the worker's :9091 surface, where the in-process reconciler loop runs — the standalone cmd/reconciler binary is one-shot and does NOT export /metrics)
	//
	// The unified registration is intentional: it keeps
	// metrics.New simple (no per-binary constructor matrix) and
	// makes federation queries portable across binaries. Zero-
	// valued series are harmless for alerting (no counter
	// increment, no histogram observations) but mean a scraper
	// will see the series names on every binary — operators who
	// want to silence the noise can drop unused families at the
	// scrape config layer.
	//
	// Posture: NOT authenticated. The endpoint is intentionally
	// public to the operator's metrics network (e.g. the Prometheus
	// scrape job inside the same VPC). Production deployments MUST
	// firewall this endpoint off from the public internet via a
	// Network Policy or Ingress allow-list — Go runtime + pool
	// stats are modest internal state but should not leak to
	// untrusted clients. See README "Deploying" for the recommended
	// posture.
	r.Get("/metrics", metricsSurface.Handler().ServeHTTP)

	// configHandler backs GET /api/config: a public, unauthenticated
	// endpoint the SPA fetches at startup to discover which auth mode
	// is active. In iam-core mode it returns the public OIDC parameters
	// the SPA needs to run the Authorization Code + PKCE flow itself
	// (authorize/token endpoints, client id, redirect uri, audience,
	// scopes). It NEVER exposes the client secret. In built-in mode it
	// returns {"auth_mode":"builtin"} so the SPA renders the password
	// form. Marked no-store: the active mode is deployment config that
	// must not be cached across an auth-provider switch.
	configHandler := func(w http.ResponseWriter, req *http.Request) {
		type payload struct {
			AuthMode     string   `json:"auth_mode"`
			Issuer       string   `json:"issuer,omitempty"`
			AuthorizeURL string   `json:"authorize_url,omitempty"`
			TokenURL     string   `json:"token_url,omitempty"`
			ClientID     string   `json:"client_id,omitempty"`
			RedirectURI  string   `json:"redirect_uri,omitempty"`
			Audience     string   `json:"audience,omitempty"`
			Scopes       []string `json:"scopes,omitempty"`
		}
		out := payload{AuthMode: "builtin"}
		if iamCoreClient != nil {
			d := iamCoreClient.Discovery()
			out = payload{
				AuthMode:     "iam-core",
				Issuer:       d.Issuer,
				AuthorizeURL: d.AuthorizationEndpoint,
				TokenURL:     d.TokenEndpoint,
				ClientID:     cfg.IAMCoreClientID,
				RedirectURI:  cfg.IAMCoreCallbackURL,
				Audience:     cfg.IAMCoreAudience,
				Scopes:       iamCoreClient.Scopes(),
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(out); err != nil {
			logging.FromContext(req.Context()).Error("encode /api/config failed", "err", err)
		}
	}

	// ssoOnlyAuthHandler replaces the built-in password login/signup
	// handlers when iam-core is the identity provider, so a client that
	// still POSTs credentials gets a clear, machine-readable signal to
	// switch to the SSO flow rather than a confusing 404.
	ssoOnlyAuthHandler := func(w http.ResponseWriter, _ *http.Request) {
		middleware.RespondError(w, http.StatusConflict, middleware.ErrCodeUnsupportedOp,
			"password authentication is disabled; this deployment authenticates via SSO (see GET /api/config)")
	}

	r.Route("/api", func(r chi.Router) {
		r.Get("/config", configHandler)
		r.Route("/auth", func(r chi.Router) {
			if iamCoreClient != nil {
				// iam-core is the identity provider: the built-in
				// password, OAuth-link, refresh/logout and TOTP
				// endpoints are all disabled. A single wildcard catches
				// every method and sub-path under /api/auth so any such
				// call — /login, /logout, /refresh, /oauth/*, /totp/* —
				// gets a clear 409 with an SSO-only hint rather than a
				// bare 404, telling stale clients to discover the
				// Universal Login via GET /api/config and present the
				// resulting iam-core access token as a bearer credential
				// on the data plane.
				r.Handle("/*", http.HandlerFunc(ssoOnlyAuthHandler))
				return
			}
			r.Post("/signup", authHandler.Signup)
			r.Post("/login", authHandler.Login)
			r.Route("/oauth", func(r chi.Router) {
				oauthHandler.RegisterRoutes(r)
			})

			r.Group(func(r chi.Router) {
				r.Use(middleware.AuthMiddlewareWithKeys(jwtKeyManager, sessionChecker))
				r.Post("/logout", authHandler.Logout)
				r.Post("/refresh", authHandler.Refresh)
			})

			// TOTP / 2FA routes. The three middleware
			// groups model the three valid token "purposes":
			//   - session token: enroll/begin, enroll/finalize
			//     (re-enrollment from settings), disable, status
			//   - mfa_enroll purpose: enroll/begin, enroll/finalize
			//     (initial enrollment under a workspace policy)
			//   - mfa_challenge purpose: verify
			// AuthMiddleware refuses purpose-scoped tokens for the
			// session group; PurposeMiddleware refuses everything
			// that doesn't carry the expected purpose. The two
			// surfaces never overlap, so an attacker who captures
			// one kind of token cannot replay it against the other.
			totpHandler := auth.NewTOTPHandler(authHandler)
			if totpHandler != nil {
				r.Route("/totp", func(r chi.Router) {
					// Session-authenticated routes: re-enroll
					// flow, disable, status. The user already has
					// a session JWT and is managing 2FA from
					// account settings.
					r.Group(func(r chi.Router) {
						r.Use(middleware.AuthMiddlewareWithKeys(jwtKeyManager, sessionChecker))
						r.Post("/enroll/begin", totpHandler.EnrollBegin)
						r.Post("/enroll/finalize", totpHandler.EnrollFinalize)
						r.Post("/disable", totpHandler.Disable)
						r.Get("/status", totpHandler.Status)
					})
					// must-enroll path: the user just authenticated
					// with password / OAuth on a workspace that
					// requires MFA, and they have no credential yet.
					// The enroll token authorises ONLY the
					// enrollment endpoints — no data plane.
					r.Group(func(r chi.Router) {
						r.Use(middleware.PurposeMiddlewareWithKeys(jwtKeyManager, middleware.PurposeMFAEnroll))
						r.Post("/enroll/begin/required", totpHandler.EnrollBegin)
						r.Post("/enroll/finalize/required", totpHandler.EnrollFinalize)
					})
					// Challenge-token path: complete the second
					// factor and exchange for a real session JWT.
					r.Group(func(r chi.Router) {
						r.Use(middleware.PurposeMiddlewareWithKeys(jwtKeyManager, middleware.PurposeMFAChallenge))
						r.Post("/verify", totpHandler.Verify)
					})
				})
			}
		})

		// WebSocket endpoint runs behind the auth middleware so the
		// hub gets (workspaceID, userID) from JWT claims, but
		// deliberately *outside* the rate limiter / tenant guard
		// group: long-lived connections must not be charged per
		// frame, and TenantGuard's HTTP-method assumptions trip on
		// the upgrade handshake.
		r.Group(func(r chi.Router) {
			r.Use(dataPlaneAuth)
			// Suspension enforcement applies to the WS upgrade too: a
			// suspended workspace must not be able to keep realtime sync
			// or collaborative editing alive when every REST call is
			// already returning 503. SuspensionGuard needs only the
			// workspace id (bound by the auth middleware above) and runs
			// on the initial HTTP request before the upgrade, so — unlike
			// TenantGuard/rateLimiter — it has no handshake or per-frame
			// cost concerns and is safe to mount here. Ordered before
			// IPAllowlist to match the REST data-plane group's precedence
			// (SuspensionGuard → IPAllowlist), so a suspended workspace
			// gets the same 503 regardless of transport rather than a 503
			// on REST but a 403 on the WS upgrade.
			r.Use(middleware.SuspensionGuard(platformSvc, cfg.SuspensionFailClosed))
			// IP allowlist enforcement applies to the WS upgrade too:
			// conditional access must gate EVERY entry into a
			// workspace, not just the REST data plane, else a blocked
			// network could still open the realtime sync / collab
			// channels. IPAllowlist only needs the workspace id, which
			// the auth middleware above binds from the JWT claims, so
			// it works without TenantGuard (which is skipped here for
			// the upgrade-handshake reasons noted below) and runs on
			// the initial HTTP request before the upgrade.
			r.Use(middleware.IPAllowlist(ipAllowSvc, cfg.TrustedProxyDepth))
			if wsProxyMode {
				// WS proxy mode: client connections terminate at the
				// external proxy tier (Centrifugo/Pusher), not here.
				// Respond 501 so a client still dialing the API directly
				// fails loudly instead of opening a socket the proxy
				// will never feed events into.
				r.Get("/ws", func(w http.ResponseWriter, _ *http.Request) {
					middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "websocket proxy mode enabled; connect via the external proxy tier")
				})
			} else {
				r.Get("/ws", wsHandler.ServeWS)
			}
			// Collab WS endpoint: per-document Yjs relay. Mounted
			// next to /ws because both are long-lived upgrade
			// endpoints that must skip TenantGuard / rateLimiter
			// (TenantGuard's HTTP-method assumptions trip on the
			// upgrade handshake; rateLimiter would charge per WS
			// frame which is the wrong cost model for a streaming
			// editor session). The handler performs its own
			// tenant + permission check on the upgrade path.
			r.Get("/documents/{id}/ws", driveHandler.ServeDocumentCollab)
		})

		r.Group(func(r chi.Router) {
			r.Use(dataPlaneAuth)
			r.Use(middleware.TenantGuard())
			r.Use(middleware.SuspensionGuard(platformSvc, cfg.SuspensionFailClosed))
			// IP allowlist enforcement runs after the tenant guard
			// has resolved the workspace. It is a no-op for any
			// workspace that has not enabled the feature. Mounted on
			// the data-plane group only — NOT the /admin group below
			// — so an admin who misconfigures the allowlist can still
			// reach the management endpoints to fix it and cannot
			// lock themselves out of their own workspace.
			r.Use(middleware.IPAllowlist(ipAllowSvc, cfg.TrustedProxyDepth))
			r.Use(rateLimiter())

			// Progressive feature disclosure: the SPA fetches this
			// once on login to gate UI behind the workspace's tier +
			// overrides (see frontend/src/hooks/useFeatures.ts).
			r.Get("/features", driveHandler.GetFeatures)

			// Resolved identity of the authenticated caller. Auth-mode
			// agnostic; the iam-core SPA calls it after token exchange
			// to learn its zk-drive user/workspace/role.
			r.Get("/me", driveHandler.Me)

			r.Get("/workspaces", driveHandler.ListWorkspaces)
			r.Post("/workspaces", driveHandler.CreateWorkspace)
			r.Get("/workspaces/{id}", driveHandler.GetWorkspace)
			r.Put("/workspaces/{id}", driveHandler.UpdateWorkspace)

			r.Get("/folders", driveHandler.ListFolders)
			r.Post("/folders", driveHandler.CreateFolder)
			r.Get("/folders/{id}", driveHandler.GetFolder)
			r.Put("/folders/{id}", driveHandler.RenameFolder)
			r.Delete("/folders/{id}", driveHandler.DeleteFolder)
			r.Post("/folders/{id}/move", driveHandler.MoveFolder)

			r.Get("/folders/{id}/documents", driveHandler.ListFolderDocuments)
			r.Post("/documents", driveHandler.CreateDocument)
			r.Get("/documents/{id}", driveHandler.GetDocument)
			r.Put("/documents/{id}", driveHandler.RenameDocument)
			r.Patch("/documents/{id}/collab-mode", driveHandler.SetDocumentCollabMode)
			r.Delete("/documents/{id}", driveHandler.DeleteDocument)
			r.Get("/documents/{id}/snapshot", driveHandler.GetDocumentSnapshot)
			r.Get("/documents/{id}/deltas", driveHandler.ListDocumentDeltas)
			r.Post("/documents/{id}/deltas", driveHandler.AppendDocumentDelta)

			r.Post("/files", driveHandler.CreateFile)
			r.Post("/files/upload-url", driveHandler.UploadURL)
			r.Post("/files/confirm-upload", driveHandler.ConfirmUpload)
			r.Get("/files/{id}", driveHandler.GetFile)
			r.Put("/files/{id}", driveHandler.UpdateFile)
			r.Delete("/files/{id}", driveHandler.DeleteFile)
			r.Post("/files/{id}/move", driveHandler.MoveFile)
			r.Get("/files/{id}/versions", driveHandler.ListFileVersions)
			r.Get("/files/{id}/download-url", driveHandler.DownloadURL)
			r.Get("/files/{id}/preview-url", driveHandler.PreviewURL)
			r.Get("/files/{id}/editor-config", driveHandler.EditorConfig)
			r.Get("/onlyoffice/status", driveHandler.OnlyOfficeStatus)
			r.Get("/files/{id}/tags", driveHandler.ListFileTags)
			r.Post("/files/{id}/tags", driveHandler.AddFileTag)
			r.Delete("/files/{id}/tags/{tag}", driveHandler.RemoveFileTag)
			r.Get("/files/{id}/tag-suggestions", driveHandler.SuggestFileTags)

			r.Post("/bulk/move", driveHandler.BulkMove)
			r.Post("/bulk/copy", driveHandler.BulkCopy)
			r.Post("/bulk/delete", driveHandler.BulkDelete)
			r.Post("/bulk/download", driveHandler.BulkDownload)

			r.Get("/permissions", driveHandler.ListPermissions)
			r.Post("/permissions", driveHandler.GrantPermission)
			r.Delete("/permissions/{id}", driveHandler.RevokePermission)

			r.Post("/share-links", driveHandler.CreateShareLink)
			r.Delete("/share-links/{id}", driveHandler.RevokeShareLink)

			r.Post("/guest-invites", driveHandler.CreateGuestInvite)
			r.Post("/guest-invites/{id}/accept", driveHandler.AcceptGuestInvite)
			r.Delete("/guest-invites/{id}", driveHandler.RevokeGuestInvite)

			r.Get("/client-rooms", driveHandler.ListClientRooms)
			r.Post("/client-rooms", driveHandler.CreateClientRoom)
			r.Get("/client-rooms/templates", driveHandler.ListClientRoomTemplates)
			r.Post("/client-rooms/from-template", driveHandler.CreateClientRoomFromTemplate)
			r.Get("/client-rooms/{id}", driveHandler.GetClientRoom)
			r.Delete("/client-rooms/{id}", driveHandler.DeleteClientRoom)

			r.Get("/search", driveHandler.Search)
			r.Get("/search/expand", driveHandler.ExpandSearchQuery)

			r.Get("/notifications", driveHandler.ListNotifications)
			r.Post("/notifications/read-all", driveHandler.MarkAllNotificationsRead)
			r.Post("/notifications/{id}/read", driveHandler.MarkNotificationRead)

			// Web Push (RFC 8030 + VAPID) subscription management.
			// Respond 501 when VAPID keys are unconfigured.
			r.Get("/push/vapid-public-key", driveHandler.VAPIDPublicKey)
			r.Post("/push/subscribe", driveHandler.SubscribePush)
			r.Delete("/push/subscribe", driveHandler.UnsubscribePush)

			// Native mobile push (APNs / FCM) device-token registration.
			// Respond 501 when no mobile push provider is configured.
			r.Post("/push/register-device", driveHandler.RegisterDevice)
			r.Delete("/push/register-device", driveHandler.UnregisterDevice)

			r.Get("/activity", driveHandler.ListActivity)

			// Change feed for desktop sync SDK. Live push rides
			// on the existing /ws hub (workspace-wide broadcast,
			// type="change"); these REST routes serve catch-up.
			r.Get("/changes", driveHandler.ListChanges)
			r.Get("/changes/latest", driveHandler.LatestChange)
		})

		r.Route("/admin", func(r chi.Router) {
			r.Use(dataPlaneAuth)
			r.Use(middleware.TenantGuard())
			r.Use(middleware.SuspensionGuard(platformSvc, cfg.SuspensionFailClosed))
			r.Use(middleware.AdminOnly())
			r.Use(rateLimiter())
			adminHandler.RegisterRoutes(r)
			// Outbound webhook subscription management
			// rides on the same admin-only middleware stack so
			// only workspace admins can create / delete /
			// inspect subscriptions.
			r.Route("/webhooks", func(r chi.Router) {
				webhookHandler.RegisterRoutes(r)
			})
		})

		r.Route("/kchat", func(r chi.Router) {
			r.Use(dataPlaneAuth)
			r.Use(middleware.TenantGuard())
			r.Use(middleware.SuspensionGuard(platformSvc, cfg.SuspensionFailClosed))
			// kchat is a data-plane feature (attachment uploads, room
			// creation, member sync), so it must honour the workspace IP
			// allowlist exactly like the main data-plane group above.
			r.Use(middleware.IPAllowlist(ipAllowSvc, cfg.TrustedProxyDepth))
			r.Use(rateLimiter())
			kchatHandler.RegisterRoutes(r)
		})

		// Platform control-plane routes. Authenticated by a platform
		// API key (Authorization: Bearer pk_...) via PlatformAuth —
		// deliberately NOT behind the workspace JWT AuthMiddleware /
		// TenantGuard chain, since these endpoints operate across the
		// whole fleet rather than within a single workspace.
		//
		// Because this group runs outside the per-user/-workspace
		// rate limiter (there's no JWT to key on), a per-client-IP
		// limiter runs FIRST as defense-in-depth: it bounds an
		// unauthenticated flood of bogus `pk_` tokens before any auth
		// work happens. Fails open if Redis is down so a hiccup never
		// locks operators out of the control plane.
		r.Route("/platform", func(r chi.Router) {
			// ipAllowRedis (not redisClient) is a true-nil interface when
			// Redis is unconfigured; passing the typed-nil *redis.Client
			// here would defeat IPRateLimiter's nil-check and skip its
			// in-memory fallback.
			r.Use(middleware.IPRateLimiter(ipAllowRedis, middleware.DefaultPlatformIPRate, cfg.TrustedProxyDepth))
			r.Use(middleware.PlatformAuth(platformHandler))
			platformHandler.RegisterRoutes(r)
		})

		// Public share-link resolution — deliberately outside the auth
		// group so anyone holding a token can resolve the link
		// (ARCHITECTURE.md §7.3). Password / expiry / download-cap
		// checks run in the sharing service.
		r.Get("/share-links/{token}", driveHandler.ResolveShareLink)
		r.Post("/share-links/{token}", driveHandler.ResolveShareLink)

		// Public guest-invite preview — mirrors the unauthenticated
		// share-link resolution route. The invite ID is a UUIDv4, so
		// guessing is infeasible; the response is the
		// GuestInvitePreview projection (workspace/folder names,
		// recipient email, role, expiry) with no secrets.
		r.Get("/guest-invites/{id}/preview", driveHandler.PreviewGuestInvite)

		// Stripe billing webhook — deliberately outside the auth
		// middleware group. Stripe authenticates itself via the
		// Stripe-Signature header rather than a JWT, which the
		// handler verifies against STRIPE_WEBHOOK_SECRET.
		r.Post("/webhooks/stripe", stripeService.HandleWebhook)

		// ONLYOFFICE Document Server save callback — outside the
		// session-auth group because the Document Server holds no ZK
		// Drive JWT. It is authenticated by the ONLYOFFICE-signed
		// body/header token (verified against ONLYOFFICE_SECRET) plus
		// the workspace_id query param the editor-config embedded in
		// the callbackUrl.
		r.Post("/files/{id}/editor-callback", driveHandler.EditorCallback)
	})

	if cfg.StaticDir != "" {
		if info, err := os.Stat(cfg.StaticDir); err == nil && info.IsDir() {
			slog.Info("static serving SPA assets", "dir", cfg.StaticDir)
			r.NotFound(spaHandler(cfg.StaticDir))
		} else {
			slog.Warn("static STATIC_DIR is not a readable directory, skipping SPA serving", "dir", cfg.StaticDir)
		}
	}

	srv := &http.Server{
		Addr: cfg.ListenAddr,
		// logging.AccessLog wraps the entire chi mux so it
		// observes both routed and unrouted requests. It logs a
		// single "http request" record per request AFTER the
		// handler finishes, with the resolved chi route pattern,
		// response status, byte count, and duration — the
		// canonical fields dashboards aggregate against.
		// Installed outside r.Use(...) on purpose: chi doesn't
		// expose the resolved RoutePattern until after routing,
		// so the access logger has to sit at the http.Handler
		// boundary to read it post-dispatch.
		//
		// otelhttp.NewHandler is the outermost wrapper so a
		// single span covers the WHOLE request lifecycle
		// (including AccessLog's pre/post-dispatch work). The
		// span starts with the generic name "http.server" and is
		// renamed post-dispatch by otelChiSpanRenamer (a chi
		// middleware that reads the resolved RoutePattern AFTER
		// chi finishes routing). This ensures spans group as
		// `/api/files/{id}` rather than per-UUID. otelhttp also
		// injects the active span into the request context, so
		// downstream packages (slog, pgx, redis, NATS publisher)
		// tag their child operations with the same trace id.
		Handler: otelhttp.NewHandler(
			logging.AccessLog(r),
			"http.server",
		),
		// ReadHeaderTimeout is the first line of defence against
		// slowloris-style attacks: caps how long a client can
		// dribble out request headers before the server abandons
		// the connection. 5s is enough for any well-behaved client
		// (browsers and curl complete header send in single-digit
		// ms over normal links) but tight enough that an attacker
		// holding hundreds of half-open connections can't pin a
		// goroutine each. ReadTimeout below is the broader cap
		// covering body read as well — ReadHeaderTimeout is the
		// narrower, header-only guard go vet / staticcheck prefer
		// to see explicitly set rather than inferred from
		// ReadTimeout. Mirrors the worker metrics server pattern
		// in cmd/worker/main.go:289-302.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("zk-drive server listening", "addr", cfg.ListenAddr)
		errCh <- srv.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		slog.Info("received signal, shutting down", "signal", sig.String())
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	// Shutdown order is load-bearing:
	//   1. srv.Shutdown drains in-flight HTTP requests and stops
	//      accepting new connections, but does NOT close hijacked
	//      WebSocket connections (gorilla/websocket Upgrade hijacks
	//      the underlying TCP conn, removing it from net/http's
	//      bookkeeping). We need step 2 to drain those.
	//   2. collabHub.Shutdown closes every active collab WS client
	//      (so the write pumps send a clean 1000 Normal Closure
	//      frame) and waits for in-flight compaction goroutines to
	//      return. Without this, a deploy/restart would RST the WS
	//      conns and clients would mis-classify a graceful restart
	//      as a network failure.
	//   3. The deferred bgGoroutines.Wait + pool.Close at the top of
	//      run() finish the shutdown after we return.
	shutdownErr := srv.Shutdown(shutdownCtx)
	collabHub.Shutdown(shutdownCtx)
	return shutdownErr
}

// spaHandler serves a Vite-built single-page app from `dir`. Concrete
// asset files (JS, CSS, the favicon, ...) are returned verbatim;
// anything else falls back to `index.html` so client-side routes survive
// a hard refresh. The `/api`, `/healthz`, and `/readyz` namespaces are
// already handled before this NotFound handler runs, so we only see SPA
// paths here. We deliberately reject `..` traversal to keep the handler
// safe when STATIC_DIR is a sibling of sensitive files.
func spaHandler(dir string) http.HandlerFunc {
	indexPath := filepath.Join(dir, "index.html")
	return func(w http.ResponseWriter, r *http.Request) {
		clean := filepath.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
		if strings.Contains(clean, "..") {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		candidate := filepath.Join(dir, clean)
		if rel, err := filepath.Rel(dir, candidate); err != nil || strings.HasPrefix(rel, "..") {
			http.ServeFile(w, r, indexPath)
			return
		}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			http.ServeFile(w, r, candidate)
			return
		}
		http.ServeFile(w, r, indexPath)
	}
}

// folderCreatorAdapter bridges *folder.Service to
// sharing.FolderCreator. Keeping the adapter here (rather than inside
// the sharing package) avoids an import cycle: sharing's
// client-room service needs to mint folders, but folder must not
// depend on sharing.
type folderCreatorAdapter struct {
	svc *folder.Service
}

func (a folderCreatorAdapter) Create(ctx context.Context, workspaceID uuid.UUID, parentID *uuid.UUID, name string, createdBy uuid.UUID) (sharing.FolderRef, error) {
	f, err := a.svc.Create(ctx, workspaceID, parentID, name, createdBy)
	if err != nil {
		return sharing.FolderRef{}, err
	}
	return sharing.FolderRef{ID: f.ID}, nil
}

// buildEmailService composes the transactional-email service from
// the loaded config. The graceful-degradation contract — "omit any
// one required env var to leave email disabled, server boots
// cleanly in disabled mode" — lives in email.BuildFromOperatorConfig
// so the contract is testable from the email-package test suite
// without requiring cmd/server to grow a test target. This wrapper
// is a thin adapter that maps the loaded *config.Config onto the
// email-package's OperatorConfig view.
func buildEmailService(cfg *config.Config) (*email.Service, error) {
	return email.BuildFromOperatorConfig(email.OperatorConfig{
		PublicURL:                 cfg.PublicURL,
		SMTPHost:                  cfg.SMTPHost,
		SMTPPort:                  cfg.SMTPPort,
		SMTPUsername:              cfg.SMTPUsername,
		SMTPPassword:              cfg.SMTPPassword,
		SMTPFromAddress:           cfg.SMTPFromAddress,
		SMTPFromName:              cfg.SMTPFromName,
		SMTPTLSMode:               cfg.SMTPTLSMode,
		SMTPTLSServerName:         cfg.SMTPTLSServerName,
		SMTPTLSInsecureSkipVerify: cfg.SMTPTLSInsecureSkipVerify,
	})
}

// loadCredentialMaterial resolves a credential supplied either inline or
// via a file path, used by the FCM / APNs provider wiring. The inline
// value takes precedence: an operator who sets both gets the inline one
// (and the file is ignored), matching the "inline wins" contract
// documented on the Config fields. Returns an error only when neither is
// usable (inline empty and the file cannot be read), so the caller can
// disable just that provider and continue.
func loadCredentialMaterial(inline, path string) ([]byte, error) {
	if strings.TrimSpace(inline) != "" {
		return []byte(inline), nil
	}
	if path == "" {
		return nil, errors.New("no inline value and no file path configured")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read credential file %q: %w", path, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("credential file %q is empty", path)
	}
	return data, nil
}

// otelChiSpanRenamer is a chi middleware that renames the
// otelhttp-created span to the resolved chi route pattern AFTER
// dispatch. Chi only fully populates RoutePattern() as the request
// passes through nested sub-routers, so reading it before
// next.ServeHTTP would return an incomplete pattern (e.g. "/api/*"
// instead of "/api/files/{id}"). The defer ensures the rename
// fires even if the handler panics.
//
// When otelhttp is wired but tracing.Init installed the no-op
// provider (OTEL_EXPORTER_OTLP_ENDPOINT unset), trace.SpanFromContext
// returns a no-op span whose SetName / SetAttributes are silent
// no-ops — so the middleware is safe to install unconditionally.
func otelChiSpanRenamer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			// Chi resolves RoutePattern incrementally during
			// its tree walk through nested routers, so the full
			// pattern is only available AFTER next.ServeHTTP
			// returns — same timing used by metrics.HTTPMiddleware
			// and logging.AccessLog. For 404s the pattern is
			// empty — keep the otelhttp default "http.server"
			// name.
			if rctx := chi.RouteContext(r.Context()); rctx != nil {
				if pattern := rctx.RoutePattern(); pattern != "" {
					tracing.RenameHTTPServerSpan(r.Context(), r.Method, pattern)
				}
			}
		}()
		next.ServeHTTP(w, r)
	})
}
