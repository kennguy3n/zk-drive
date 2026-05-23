package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/zk-drive/api/admin"
	apikchat "github.com/kennguy3n/zk-drive/api/kchat"
	"github.com/kennguy3n/zk-drive/api/auth"
	"github.com/kennguy3n/zk-drive/api/drive"
	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/api/ws"
	"github.com/kennguy3n/zk-drive/internal/activity"
	"github.com/kennguy3n/zk-drive/internal/ai"
	"github.com/kennguy3n/zk-drive/internal/audit"
	"github.com/kennguy3n/zk-drive/internal/billing"
	"github.com/kennguy3n/zk-drive/internal/config"
	cryptopkg "github.com/kennguy3n/zk-drive/internal/crypto"
	"github.com/kennguy3n/zk-drive/internal/database"
	"github.com/kennguy3n/zk-drive/internal/fabric"
	"github.com/kennguy3n/zk-drive/internal/file"
	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/health"
	"github.com/kennguy3n/zk-drive/internal/kchat"
	"github.com/kennguy3n/zk-drive/internal/jobs"
	"github.com/kennguy3n/zk-drive/internal/logging"
	"github.com/kennguy3n/zk-drive/internal/metrics"
	"github.com/kennguy3n/zk-drive/internal/notification"
	"github.com/kennguy3n/zk-drive/internal/permission"
	"github.com/kennguy3n/zk-drive/internal/preview"
	"github.com/kennguy3n/zk-drive/internal/retention"
	"github.com/kennguy3n/zk-drive/internal/search"
	"github.com/kennguy3n/zk-drive/internal/session"
	"github.com/kennguy3n/zk-drive/internal/sharing"
	"github.com/kennguy3n/zk-drive/internal/storage"
	"github.com/kennguy3n/zk-drive/internal/totp"
	"github.com/kennguy3n/zk-drive/internal/user"
	"github.com/kennguy3n/zk-drive/internal/version"
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

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		cancel()
		return fmt.Errorf("connect postgres: %w", err)
	}
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
		if redisClient != nil {
			_ = redisClient.Close()
		}
	}()
	defer bgGoroutines.Wait()
	defer cancel()

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

	userRepo := user.NewPostgresRepository(pool)
	userSvc := user.NewService(userRepo)

	wsRepo := workspace.NewPostgresRepository(pool)
	wsSvc := workspace.NewService(wsRepo)

	folderRepo := folder.NewPostgresRepository(pool)
	folderSvc := folder.NewService(folderRepo)

	fileRepo := file.NewPostgresRepository(pool)
	fileSvc := file.NewService(fileRepo)

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

	permissionSvc := permission.NewService(permission.NewPostgresRepository(pool))
	activitySvc := activity.NewService(activity.NewPostgresRepository(pool))
	defer activitySvc.Close()

	sharingSvc := sharing.NewService(sharing.NewPostgresRepository(pool), wiring.NewPermissionGranter(permissionSvc))
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
				jobPublisher = jobs.NewPublisher(js)
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

	// WebSocket hub fans real-time notification events to connected
	// clients. The hub itself is always in-process; when redisClient
	// is non-nil we additionally subscribe to ws:* so notifications
	// produced on any replica reach every replica's clients. The
	// notification service receives the publisher and is nil-safe,
	// so a misconfigured Redis still leaves the rest of the API
	// working — we just log and fall back to local fan-out.
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
		bgGoroutines.Add(1)
		go func() {
			defer bgGoroutines.Done()
			if err := rp.Subscribe(ctx, hub); err != nil && err != context.Canceled {
				slog.Error("redis ws subscribe loop exited", "err", err)
			}
		}()
	}

	notificationSvc := notification.NewService(notification.NewPostgresRepository(pool)).
		WithPublisher(notificationPublisher)
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
	billingRepo := billing.NewPostgresRepository(pool)
	billingSvc := billing.NewService(billingRepo)
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
	driveHandler := drive.NewHandler(pool, wsSvc, folderSvc, fileSvc, userSvc, storageClient, permissionSvc, activitySvc).
		WithStorageFactory(storageFactory).
		WithSharing(sharingSvc).
		WithSearch(searchSvc).
		WithClientRooms(clientRoomSvc).
		WithJobs(jobPublisher).
		WithNotifications(notificationSvc).
		WithPreviews(previewRepo).
		WithAudit(auditSvc).
		WithBilling(billingSvc)
	var fabricClient admin.FabricClient
	if cfg.FabricConsoleURL != "" {
		fabricClient = fabric.NewClient(fabric.ClientConfig{
			BaseURL:    cfg.FabricConsoleURL,
			AdminToken: cfg.FabricConsoleAdminToken,
		})
	}
	adminHandler := admin.NewHandler(pool, userSvc, auditSvc, retentionSvc).
		WithBilling(billingSvc).
		WithStripe(stripeService).
		WithFabric(fabricClient, provisioner, storageFactory).
		WithWorkspaces(wsSvc)

	kchatSvc := kchat.NewRoomService(
		kchat.NewPostgresRepository(pool),
		wiring.NewKChatFolderCreator(folderSvc),
		wiring.NewKChatPermissionGranter(permissionSvc),
		wiring.NewKChatFileCreator(fileSvc),
		wiring.NewKChatPresignResolver(storageFactory),
		wiring.KChatObjectKey,
		wiring.KChatObjectKeyValidator,
	)
	summarySvc := ai.NewSummaryService(pool)
	if cfg.OllamaURL != "" {
		llm, err := ai.NewOllamaClient(cfg.OllamaURL, cfg.OllamaModel)
		if err != nil {
			return fmt.Errorf("ai/ollama: %w", err)
		}
		summarySvc = summarySvc.WithLLM(llm)
		slog.Info("ai local LLM enabled", "endpoint", cfg.OllamaURL, "model", llm.Model())
	} else {
		slog.Info("ai OLLAMA_URL not set, AI summaries use rule-based scaffold (no external API calls)")
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

	r.Route("/api", func(r chi.Router) {
		r.Route("/auth", func(r chi.Router) {
			r.Post("/signup", authHandler.Signup)
			r.Post("/login", authHandler.Login)
			r.Route("/oauth", func(r chi.Router) {
				oauthHandler.RegisterRoutes(r)
			})

			r.Group(func(r chi.Router) {
				r.Use(middleware.AuthMiddleware(cfg.JWTSecret, sessionChecker))
				r.Post("/logout", authHandler.Logout)
				r.Post("/refresh", authHandler.Refresh)
			})

			// TOTP / 2FA routes (WS-19). The three middleware
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
						r.Use(middleware.AuthMiddleware(cfg.JWTSecret, sessionChecker))
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
						r.Use(middleware.PurposeMiddleware(cfg.JWTSecret, middleware.PurposeMFAEnroll))
						r.Post("/enroll/begin/required", totpHandler.EnrollBegin)
						r.Post("/enroll/finalize/required", totpHandler.EnrollFinalize)
					})
					// Challenge-token path: complete the second
					// factor and exchange for a real session JWT.
					r.Group(func(r chi.Router) {
						r.Use(middleware.PurposeMiddleware(cfg.JWTSecret, middleware.PurposeMFAChallenge))
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
			r.Use(middleware.AuthMiddleware(cfg.JWTSecret, sessionChecker))
			r.Get("/ws", wsHandler.ServeWS)
		})

		r.Group(func(r chi.Router) {
			r.Use(middleware.AuthMiddleware(cfg.JWTSecret, sessionChecker))
			r.Use(middleware.TenantGuard())
			r.Use(rateLimiter())

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
			r.Get("/files/{id}/tags", driveHandler.ListFileTags)
			r.Post("/files/{id}/tags", driveHandler.AddFileTag)
			r.Delete("/files/{id}/tags/{tag}", driveHandler.RemoveFileTag)

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

			r.Get("/notifications", driveHandler.ListNotifications)
			r.Post("/notifications/read-all", driveHandler.MarkAllNotificationsRead)
			r.Post("/notifications/{id}/read", driveHandler.MarkNotificationRead)

			r.Get("/activity", driveHandler.ListActivity)
		})

		r.Route("/admin", func(r chi.Router) {
			r.Use(middleware.AuthMiddleware(cfg.JWTSecret, sessionChecker))
			r.Use(middleware.TenantGuard())
			r.Use(middleware.AdminOnly())
			r.Use(rateLimiter())
			adminHandler.RegisterRoutes(r)
		})

		r.Route("/kchat", func(r chi.Router) {
			r.Use(middleware.AuthMiddleware(cfg.JWTSecret, sessionChecker))
			r.Use(middleware.TenantGuard())
			r.Use(rateLimiter())
			kchatHandler.RegisterRoutes(r)
		})

		// Public share-link resolution — deliberately outside the auth
		// group so anyone holding a token can resolve the link
		// (ARCHITECTURE.md §7.3). Password / expiry / download-cap
		// checks run in the sharing service.
		r.Get("/share-links/{token}", driveHandler.ResolveShareLink)
		r.Post("/share-links/{token}", driveHandler.ResolveShareLink)

		// Stripe billing webhook — deliberately outside the auth
		// middleware group. Stripe authenticates itself via the
		// Stripe-Signature header rather than a JWT, which the
		// handler verifies against STRIPE_WEBHOOK_SECRET.
		r.Post("/webhooks/stripe", stripeService.HandleWebhook)
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
		Handler: logging.AccessLog(r),
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
	return srv.Shutdown(shutdownCtx)
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
