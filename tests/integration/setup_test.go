package integration

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/zk-drive/api/admin"
	apikchat "github.com/kennguy3n/zk-drive/api/kchat"
	apiwebhooks "github.com/kennguy3n/zk-drive/api/webhooks"
	"github.com/kennguy3n/zk-drive/api/auth"
	"github.com/kennguy3n/zk-drive/api/drive"
	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/api/ws"
	"github.com/kennguy3n/zk-drive/internal/activity"
	"github.com/kennguy3n/zk-drive/internal/ai"
	"github.com/kennguy3n/zk-drive/internal/audit"
	"github.com/kennguy3n/zk-drive/internal/billing"
	"github.com/kennguy3n/zk-drive/internal/database"
	"github.com/kennguy3n/zk-drive/internal/fabric"
	"github.com/kennguy3n/zk-drive/internal/file"
	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/health"
	"github.com/kennguy3n/zk-drive/internal/kchat"
	"github.com/kennguy3n/zk-drive/internal/notification"
	"github.com/kennguy3n/zk-drive/internal/permission"
	"github.com/kennguy3n/zk-drive/internal/preview"
	"github.com/kennguy3n/zk-drive/internal/retention"
	"github.com/kennguy3n/zk-drive/internal/search"
	"github.com/kennguy3n/zk-drive/internal/session"
	"github.com/kennguy3n/zk-drive/internal/sharing"
	"github.com/kennguy3n/zk-drive/internal/wiring"
	"github.com/kennguy3n/zk-drive/internal/storage"
	"github.com/kennguy3n/zk-drive/internal/totp"
	"github.com/kennguy3n/zk-drive/internal/user"
	"github.com/kennguy3n/zk-drive/internal/webhooks"
	"github.com/kennguy3n/zk-drive/internal/workspace"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/google/uuid"
)

// testJWTSecret is the HS256 secret used by every integration test. Shared
// so tests can compose their own calls where needed.
const testJWTSecret = "integration-test-secret"

// testEnv bundles everything a test needs to exercise the API: a live
// httptest server, the pgx pool, and the initialised services. Callers use
// testEnv.ResetTables between tests.
type testEnv struct {
	t            *testing.T
	pool         *pgxpool.Pool
	server       *httptest.Server
	storage      *storage.Client
	provisioner  *fabric.Provisioner
	miniredis    *miniredis.Miniredis
	sessionStore *session.RedisSessionStore
	webhooks     *webhookCapture
}

// webhookCapture is the test-only publisher used by the integration
// harness to assert outbound-webhook emission without standing up a
// real NATS/JetStream server. Records every call in order, guarded
// by a mutex so tests that exercise concurrent handler paths (e.g.
// bulk operations) can read the slice safely.
//
// Implements three narrow interfaces — api/drive.WebhookEventPublisher
// (file + permission events) and api/admin.MemberEventPublisher
// (member events) — so a single capturer can be wired into both the
// drive handler and the admin handler. This mirrors how the
// concrete *webhooks.Publisher in internal/webhooks satisfies both
// in production.
type webhookCapture struct {
	mu           sync.Mutex
	FileEvents   []capturedFileEvent
	PermEvents   []capturedPermEvent
	MemberEvents []capturedMemberEvent
}

type capturedFileEvent struct {
	Type        webhooks.EventType
	WorkspaceID uuid.UUID
	ActorID     *uuid.UUID
	Data        webhooks.FileEventData
}

type capturedPermEvent struct {
	Type        webhooks.EventType
	WorkspaceID uuid.UUID
	ActorID     *uuid.UUID
	Data        webhooks.PermissionEventData
}

type capturedMemberEvent struct {
	Type        webhooks.EventType
	WorkspaceID uuid.UUID
	ActorID     *uuid.UUID
	Data        webhooks.MemberEventData
}

func (c *webhookCapture) PublishFileEvent(ctx context.Context, t webhooks.EventType, workspaceID uuid.UUID, actorID *uuid.UUID, data webhooks.FileEventData) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.FileEvents = append(c.FileEvents, capturedFileEvent{Type: t, WorkspaceID: workspaceID, ActorID: actorID, Data: data})
	return nil
}

func (c *webhookCapture) PublishPermissionEvent(ctx context.Context, t webhooks.EventType, workspaceID uuid.UUID, actorID *uuid.UUID, data webhooks.PermissionEventData) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.PermEvents = append(c.PermEvents, capturedPermEvent{Type: t, WorkspaceID: workspaceID, ActorID: actorID, Data: data})
	return nil
}

func (c *webhookCapture) PublishMemberEvent(ctx context.Context, t webhooks.EventType, workspaceID uuid.UUID, actorID *uuid.UUID, data webhooks.MemberEventData) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.MemberEvents = append(c.MemberEvents, capturedMemberEvent{Type: t, WorkspaceID: workspaceID, ActorID: actorID, Data: data})
	return nil
}

// fileEventsByType returns a copy of the captured file events filtered by
// type. Safe for concurrent test inspection.
func (c *webhookCapture) fileEventsByType(t webhooks.EventType) []capturedFileEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []capturedFileEvent
	for _, e := range c.FileEvents {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

// memberEventsByType returns a copy of the captured member events
// filtered by type. Safe for concurrent test inspection.
func (c *webhookCapture) memberEventsByType(t webhooks.EventType) []capturedMemberEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []capturedMemberEvent
	for _, e := range c.MemberEvents {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

// setupEnv connects to Postgres, runs migrations, wires the API, and starts
// an httptest server. The function calls t.Skip if TEST_DATABASE_URL is not
// set so unit-test runs on machines without a database pass cleanly.
func setupEnv(t *testing.T) *testEnv {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := database.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}

	migrationsDir := findMigrationsDir(t)
	if err := database.Migrate(ctx, pool, migrationsDir); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}

	userSvc := user.NewService(user.NewPostgresRepository(pool))
	wsSvc := workspace.NewService(workspace.NewPostgresRepository(pool))
	// Mirror production wiring (cmd/server/main.go): new root folders
	// inherit the workspace's default_encryption_mode (6.4). Without
	// this the harness would silently diverge from production and mask
	// the Strict-ZK-as-default behaviour.
	folderSvc := folder.NewService(folder.NewPostgresRepository(pool), folder.WithWorkspaceDefaults(wsSvc))
	fileSvc := file.NewService(file.NewPostgresRepository(pool))

	storageClient := buildTestStorageClient(t)

	permissionSvc := permission.NewService(permission.NewPostgresRepository(pool))
	activitySvc := activity.NewService(activity.NewPostgresRepository(pool))

	sharingSvc := sharing.NewService(sharing.NewPostgresRepository(pool), testPermissionGranter{svc: permissionSvc})
	searchSvc := search.NewService(pool)
	clientRoomSvc := sharing.NewClientRoomService(
		sharing.NewPostgresClientRoomRepository(pool),
		testFolderCreator{svc: folderSvc},
		sharingSvc,
	)
	notificationSvc := notification.NewService(notification.NewPostgresRepository(pool))
	previewRepo := preview.NewPostgresRepository(pool)
	auditSvc := audit.NewService(audit.NewPostgresRepository(pool))
	retentionSvc := retention.NewService(retention.NewPostgresRepository(pool), pool)
	billingRepo := billing.NewPostgresRepository(pool)
	billingSvc := billing.NewService(billingRepo)
	// Stripe service is wired without secrets so the integration
	// harness exercises the real route registrations: the webhook
	// route returns 400 (signature missing/invalid) instead of 404,
	// and the admin checkout/portal routes return 501 (not 404).
	// Tests that need real Stripe behaviour swap in a fake API.
	stripeService := billing.NewStripeService(billingSvc, billingRepo, "", "", nil)

	// WebSocket hub mirrors cmd/server/main.go's wiring so the gate
	// tests can dial /api/ws and verify the upgrade succeeds.
	hubCtx, hubCancel := context.WithCancel(context.Background())
	hub := ws.NewHub()
	go hub.Run(hubCtx)
	wsHandler := ws.NewHandler(hub)

	// Session store backed by miniredis so the logout-revocation
	// integration tests can exercise the real Redis-mediated
	// invalidation path without spinning up a real Redis container.
	// The store is wired into both the auth handler (RevokeUser on
	// Logout, IsRevoked on Refresh) and AuthMiddleware (IsRevoked on
	// every authenticated request) below.
	mr := miniredis.RunT(t)
	t.Cleanup(mr.Close)
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	sessionStore := session.NewRedisSessionStore(redisClient)

	// A real TOTP service is wired through the integration
	// harness so the full enroll -> challenge -> verify -> disable
	// lifecycle exercises the same code path as production. The
	// codec uses identity (no-op) encryption -- acceptable for
	// tests because we don't need to validate the at-rest
	// ciphertext shape end-to-end here (the crypto package has its
	// own dedicated unit tests for that).
	totpSvc := totp.NewService(totp.NewPostgresRepository(pool), identityCodec{}, "zk-drive-test")

	authHandler := auth.NewHandler(pool, userSvc, wsSvc, testJWTSecret).
		WithAudit(auditSvc).
		WithTOTP(totpSvc).
		WithSessionRevoker(sessionStore)
	webhookCap := &webhookCapture{}
	// Wire the AI tag-suggestion + query-expansion services so the
	// integration harness exercises the same code paths that
	// cmd/server/main.go does at lines 629-645. Without this wiring,
	// the /files/{id}/tag-suggestions and /search/expand endpoints
	// would respond 501 in the harness and the end-to-end pipeline
	// (handler → SuggestionService/ExpansionService → DB → rule-
	// based scaffold and optional LLM refinement) would only be
	// covered by unit tests in internal/ai. Both services are
	// language-resolver-aware (matching production wiring) so the
	// multilingual prompt path is exercised end-to-end here too.
	// Devin Review ANALYSIS_0002 on PR #85.
	//
	// A single OllamaClient is constructed up front and shared
	// across all three AI services (tag-suggest, query-expand,
	// summary). This mirrors cmd/server/main.go:632-638 exactly
	// and avoids the per-service NewOllamaClient calls that an
	// earlier iteration of this harness used. OllamaClient is
	// stateless (just endpoint URL + http.Client) so the previous
	// shape had no behavioural impact, but a single instance is
	// easier to reason about and matches production verbatim.
	// Devin Review ANALYSIS_0003 on PR #85 flagged the divergence.
	var ollamaClient ai.LLMClient
	if endpoint := os.Getenv("OLLAMA_URL"); endpoint != "" {
		llm, err := ai.NewOllamaClient(endpoint, os.Getenv("OLLAMA_MODEL"))
		if err != nil {
			t.Fatalf("ai/ollama: %v", err)
		}
		ollamaClient = llm
	}
	tagSuggestSvc := ai.NewSuggestionService(pool).WithLanguageResolver(wsSvc)
	queryExpandSvc := ai.NewExpansionService(pool).WithLanguageResolver(wsSvc)
	if ollamaClient != nil {
		tagSuggestSvc = tagSuggestSvc.WithLLM(ollamaClient)
		queryExpandSvc = queryExpandSvc.WithLLM(ollamaClient)
	}
	driveHandler := drive.NewHandler(pool, wsSvc, folderSvc, fileSvc, userSvc, storageClient, permissionSvc, activitySvc).
		WithSharing(sharingSvc).
		WithSearch(searchSvc).
		WithClientRooms(clientRoomSvc).
		WithNotifications(notificationSvc).
		WithPreviews(previewRepo).
		WithAudit(auditSvc).
		WithBilling(billingSvc).
		WithWebhooks(webhookCap).
		WithTagSuggester(tagSuggestSvc).
		WithQueryExpander(queryExpandSvc)
	// Wire a Postgres-backed fabric provisioner with no console URL
	// so admin endpoints that only need persistence (CMK) work; the
	// FabricClient interface is left nil because no test fakes the
	// upstream console yet.
	provisioner := fabric.NewProvisioner(pool, fabric.Config{})
	adminHandler := admin.NewHandler(pool, userSvc, auditSvc, retentionSvc).
		WithBilling(billingSvc).
		WithStripe(stripeService).
		WithFabric(nil, provisioner, nil).
		WithWorkspaces(wsSvc).
		WithWebhooks(webhookCap)

	// Outbound-webhook subscription admin handler. Mounted
	// alongside adminHandler under /admin/webhooks so the
	// integration harness exercises the same CRUD surface that
	// cmd/server/main.go does (Create / List / Get / Delete /
	// ListDeliveries / Resume) against the real Postgres
	// PostgresRepository — the in-process webhookCap fake covers
	// emission, this covers the admin CRUD path the bot flagged as
	// unreachable from the integration harness. POST /test wires
	// the synchronous TestDispatcher so admin-driven sanity checks
	// against a single subscription also work end-to-end.
	webhookRepo := webhooks.NewPostgresRepository(pool)
	// Relaxed validator for the harness: a fixed resolver returning
	// a public IP for any hostname (so subscription-create works
	// without an outbound DNS query), AllowLoopback so the
	// TestDispatcher can deliver to an httptest.Server bound to
	// 127.0.0.1:port, and AllowHTTP so we don't need a TLS-
	// terminating fixture. Mirrors the testValidator helper in
	// api/webhooks/handler_test.go so the two surfaces have
	// identical SSRF behaviour in tests.
	webhookValidator := webhooks.NewURLValidator()
	webhookValidator.Resolver = staticIPResolver{ip: net.ParseIP("1.1.1.1")}
	webhookValidator.AllowHTTP = true
	webhookValidator.AllowLoopback = true
	webhookTester, err := webhooks.NewTestDispatcher(webhookRepo, webhooks.NewDeliveryClient(webhookValidator, webhooks.DefaultDeliveryTimeout))
	if err != nil {
		t.Fatalf("webhooks/test-dispatcher: %v", err)
	}
	webhookHandler := apiwebhooks.NewHandler(webhookRepo).
		WithValidator(webhookValidator).
		WithTestDispatcher(webhookTester).
		WithAudit(auditSvc)

	// KChat service: same wiring as cmd/server/main.go, with a fallback
	// storage factory so AttachmentUploadURL can mint signed URLs in
	// tests even without per-workspace credentials.
	kchatStorageFactory := storage.NewClientFactory(nil, storageClient, nil)
	kchatSvc := kchat.NewRoomService(
		kchat.NewPostgresRepository(pool),
		wiring.NewKChatFolderCreator(folderSvc),
		wiring.NewKChatPermissionGranter(permissionSvc),
		wiring.NewKChatFileCreator(fileSvc),
		wiring.NewKChatPresignResolver(kchatStorageFactory),
		wiring.KChatObjectKey,
		wiring.KChatObjectKeyValidator,
	)
	// Wire the workspace search-language resolver so the multilingual
	// prompt path matches cmd/server/main.go's production wiring.
	// Without this, the integration harness would always exercise the
	// English-fallback branch and the workspace.SearchLanguage →
	// PromptLanguageFor codepath would only be covered by unit tests in
	// internal/ai. Devin Review WS6 prompt-language change.
	//
	// Reuses the ollamaClient built earlier so the harness has the
	// same single-instance shape as cmd/server/main.go:632-638.
	summarySvc := ai.NewSummaryService(pool).WithLanguageResolver(wsSvc)
	if ollamaClient != nil {
		summarySvc = summarySvc.WithLLM(ollamaClient)
	}
	kchatHandler := apikchat.NewHandler(kchatSvc, summarySvc)

	r := chi.NewRouter()
	r.Use(chimw.Recoverer)

	// Mirror production wiring for the readiness probe. Postgres is
	// the only fully-live dependency in the integration harness —
	// the fake S3 endpoint is reachable as a presign target but does
	// NOT serve HeadBucket, so registering a real StorageChecker
	// against it would yield spurious 503s. Instead we register
	// NewStorageChecker(nil) on purpose: that exercises the
	// constructor's typed-nil short-circuit (the path that broke in
	// production when cfg.S3Endpoint was unset, fixed by passing the
	// concrete *storage.Client through the constructor rather than
	// upcasting to the storageProbe interface) and pins it as an
	// integration-level regression. Redis and NATS are similarly
	// wired as nil to exercise the optional-dep contract end-to-end.
	r.Get("/readyz", health.NewService(
		[]health.Checker{
			health.NewPostgresChecker(pool),
			health.NewStorageChecker(nil),
			health.NewRedisChecker(nil),
			health.NewNATSChecker(nil),
		},
		health.DefaultCheckTimeout,
	).ReadyHandler())

	r.Route("/api", func(r chi.Router) {
		r.Route("/auth", func(r chi.Router) {
			r.Post("/signup", authHandler.Signup)
			r.Post("/login", authHandler.Login)
			// /logout sits inside the AuthMiddleware group so the
			// handler can read claims from the bearer token and
			// record a per-user revocation cutoff. Without the
			// middleware Logout would 204 without revoking
			// anything — the bug the Redis revoker layer exists to fix.
			r.Group(func(r chi.Router) {
				r.Use(middleware.AuthMiddleware(testJWTSecret, sessionStore))
				r.Post("/logout", authHandler.Logout)
				r.Post("/refresh", authHandler.Refresh)
			})

			// TOTP routes mirror cmd/server/main.go wiring so
			// integration tests exercise the same purpose-token
			// chokepoint that production enforces.
			totpHandler := auth.NewTOTPHandler(authHandler)
			if totpHandler != nil {
				r.Route("/totp", func(r chi.Router) {
					r.Group(func(r chi.Router) {
						r.Use(middleware.AuthMiddleware(testJWTSecret, sessionStore))
						r.Post("/enroll/begin", totpHandler.EnrollBegin)
						r.Post("/enroll/finalize", totpHandler.EnrollFinalize)
						r.Post("/disable", totpHandler.Disable)
						r.Get("/status", totpHandler.Status)
					})
					r.Group(func(r chi.Router) {
						r.Use(middleware.PurposeMiddleware(testJWTSecret, middleware.PurposeMFAEnroll))
						r.Post("/enroll/begin/required", totpHandler.EnrollBegin)
						r.Post("/enroll/finalize/required", totpHandler.EnrollFinalize)
					})
					r.Group(func(r chi.Router) {
						r.Use(middleware.PurposeMiddleware(testJWTSecret, middleware.PurposeMFAChallenge))
						r.Post("/verify", totpHandler.Verify)
					})
				})
			}
		})

		// WebSocket endpoint mirrors cmd/server/main.go: behind the
		// auth middleware (so the hub gets workspace + user IDs from
		// JWT claims) but outside the rate limiter / tenant guard
		// group, since long-lived connections must not be charged
		// per-frame and TenantGuard's HTTP-method assumptions trip on
		// the upgrade handshake.
		r.Group(func(r chi.Router) {
			r.Use(middleware.AuthMiddleware(testJWTSecret, sessionStore))
			r.Get("/ws", wsHandler.ServeWS)
		})

		// Stripe webhook is deliberately outside the auth middleware
		// — Stripe authenticates itself via the Stripe-Signature
		// header rather than a JWT.
		r.Post("/webhooks/stripe", stripeService.HandleWebhook)

		r.Group(func(r chi.Router) {
			r.Use(middleware.AuthMiddleware(testJWTSecret, sessionStore))
			r.Use(middleware.TenantGuard())
			// Permissive rate-limit (PerUser/PerWorkspace=0) so tests
			// don't have to worry about throttling, but still exercise
			// the middleware's request-counting code path.
			r.Use(middleware.RateLimiter(middleware.RateLimitConfig{
				PerUser:      0,
				PerWorkspace: 0,
			}))

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

			r.Get("/search", driveHandler.Search)
			r.Get("/search/expand", driveHandler.ExpandSearchQuery)

			r.Get("/client-rooms", driveHandler.ListClientRooms)
			r.Post("/client-rooms", driveHandler.CreateClientRoom)
			r.Get("/client-rooms/templates", driveHandler.ListClientRoomTemplates)
			r.Post("/client-rooms/from-template", driveHandler.CreateClientRoomFromTemplate)
			r.Get("/client-rooms/{id}", driveHandler.GetClientRoom)
			r.Delete("/client-rooms/{id}", driveHandler.DeleteClientRoom)

			r.Get("/notifications", driveHandler.ListNotifications)
			r.Post("/notifications/read-all", driveHandler.MarkAllNotificationsRead)
			r.Post("/notifications/{id}/read", driveHandler.MarkNotificationRead)

			r.Get("/activity", driveHandler.ListActivity)
		})

		r.Route("/admin", func(r chi.Router) {
			r.Use(middleware.AuthMiddleware(testJWTSecret, sessionStore))
			r.Use(middleware.TenantGuard())
			r.Use(middleware.AdminOnly())
			adminHandler.RegisterRoutes(r)
			// Outbound-webhook subscription admin surface.
			// Mounted here so the integration harness exercises the
			// real Create / List / Get / Delete / ListDeliveries /
			// Resume routes against the Postgres repository, not
			// just the in-process emission fakes. The POST /test
			// route is wired via WithTestDispatcher below so the
			// synchronous test-fan-out works end-to-end without
			// JetStream. The repo lives next to the production
			// PostgresRepository so the integration tests catch
			// migration drift the unit tests can't.
			r.Route("/webhooks", func(r chi.Router) {
				webhookHandler.RegisterRoutes(r)
			})
		})

		r.Route("/kchat", func(r chi.Router) {
			r.Use(middleware.AuthMiddleware(testJWTSecret, sessionStore))
			r.Use(middleware.TenantGuard())
			kchatHandler.RegisterRoutes(r)
		})

		r.Get("/share-links/{token}", driveHandler.ResolveShareLink)
		r.Post("/share-links/{token}", driveHandler.ResolveShareLink)
	})

	srv := httptest.NewServer(r)
	env := &testEnv{
		t:            t,
		pool:         pool,
		server:       srv,
		storage:      storageClient,
		provisioner:  provisioner,
		miniredis:    mr,
		sessionStore: sessionStore,
		webhooks:     webhookCap,
	}
	t.Cleanup(func() {
		srv.Close()
		// Stop the WS hub goroutine before closing the pool so
		// no further Broadcasts can race against shutdown.
		hubCancel()
		// Close activity / audit before the pool so any final drain
		// writes still find a live connection; otherwise the worker
		// goroutine leaks and blocks shutdown of the test binary.
		activitySvc.Close()
		auditSvc.Close()
		pool.Close()
	})
	env.ResetTables()
	return env
}

// ResetTables truncates all application tables in a single transaction so
// tests can share a database without leaking rows. The miniredis backing
// the session store is flushed as part of the same reset so revocation
// cutoffs from a prior test (or sub-test re-using the same testEnv)
// cannot leak forward and cause confusing 401s on tokens that the new
// test thought it just issued cleanly.
func (e *testEnv) ResetTables() {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stmts := []string{
		`ALTER TABLE workspaces DROP CONSTRAINT IF EXISTS fk_workspaces_owner`,
		// webhook_deliveries / webhook_subscriptions are listed
		// explicitly so the dependency is visible at the test
		// harness layer. They WOULD cascade from workspaces /
		// users via FK ON DELETE CASCADE, but pinning them here
		// guards against a future migration that drops or relaxes
		// the cascade leaving stale rows between tests and
		// producing flaky "subscription already exists" / counter
		// drift symptoms that are hard to diagnose. Same defensive
		// reason kchat_room_folders is listed even though it
		// cascades from client_rooms.
		`TRUNCATE webhook_deliveries, webhook_subscriptions, kchat_room_folders, workspace_storage_credentials, workspace_plans, usage_events, file_tags, retention_policies, audit_log, notifications, file_previews, client_rooms, guest_invites, share_links, activity_log, permissions, file_versions, files, folders, users, workspaces RESTART IDENTITY CASCADE`,
		`ALTER TABLE workspaces ADD CONSTRAINT fk_workspaces_owner FOREIGN KEY (owner_user_id) REFERENCES users(id)`,
	}
	for _, s := range stmts {
		if _, err := e.pool.Exec(ctx, s); err != nil {
			e.t.Fatalf("reset tables (%q): %v", s, err)
		}
	}
	if e.miniredis != nil {
		e.miniredis.FlushAll()
	}
}

// buildTestStorageClient returns a presign client pointed at S3_ENDPOINT
// when that variable is set (letting the upload round-trip test run against
// a real zk-object-fabric). When S3_ENDPOINT is unset, tests that only
// exercise URL generation use a stub endpoint so the SDK can still sign —
// no network traffic leaves the test unless a caller actually uses the URL.
func buildTestStorageClient(t *testing.T) *storage.Client {
	t.Helper()
	endpoint := os.Getenv("S3_ENDPOINT")
	bucket := getEnvDefault("S3_BUCKET", "zk-drive-test")
	accessKey := getEnvDefault("S3_ACCESS_KEY", "demo-access-key")
	secretKey := getEnvDefault("S3_SECRET_KEY", "demo-secret-key")
	if endpoint == "" {
		endpoint = "http://localhost:65535" // unused stub; signing only needs a parseable URL
	}
	client, err := storage.NewClient(storage.Config{
		Endpoint:  endpoint,
		Bucket:    bucket,
		AccessKey: accessKey,
		SecretKey: secretKey,
	})
	if err != nil {
		t.Fatalf("storage client: %v", err)
	}
	return client
}

func getEnvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// findMigrationsDir walks up from this test file looking for the migrations
// directory, which lets us run the suite from any working directory.
func findMigrationsDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, "migrations")
		if stat, err := os.Stat(candidate); err == nil && stat.IsDir() {
			return candidate
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate migrations directory")
	return ""
}

// httpRequest performs an HTTP request against the test server and returns
// the response status plus body bytes. It closes the response body.
func (e *testEnv) httpRequest(method, path, token string, payload any) (int, []byte) {
	e.t.Helper()
	var body io.Reader
	if payload != nil {
		buf, err := json.Marshal(payload)
		if err != nil {
			e.t.Fatalf("marshal payload: %v", err)
		}
		body = strings.NewReader(string(buf))
	}
	req, err := http.NewRequest(method, e.server.URL+path, body)
	if err != nil {
		e.t.Fatalf("new request: %v", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, b
}

// decodeJSON is a small wrapper around json.Unmarshal that fatals on error
// so test bodies stay readable.
func (e *testEnv) decodeJSON(body []byte, out any) {
	e.t.Helper()
	if err := json.Unmarshal(body, out); err != nil {
		e.t.Fatalf("decode json: %v (body=%s)", err, string(body))
	}
}

// signupAndLogin creates a new workspace + admin user and returns the
// resulting token payload.
func (e *testEnv) signupAndLogin(workspaceName, email, name, password string) tokenPayload {
	e.t.Helper()
	status, body := e.httpRequest(http.MethodPost, "/api/auth/signup", "", map[string]string{
		"workspace_name": workspaceName,
		"email":          email,
		"name":           name,
		"password":       password,
	})
	if status != http.StatusOK {
		e.t.Fatalf("signup: status=%d body=%s", status, string(body))
	}
	var tok tokenPayload
	e.decodeJSON(body, &tok)
	return tok
}

// login authenticates an existing user (e.g. a member previously
// invited by an admin) and returns the resulting token payload.
// Distinct from signupAndLogin so multi-role tests can exercise
// both the admin role and the member role inside one workspace.
func (e *testEnv) login(email, password string) tokenPayload {
	e.t.Helper()
	status, body := e.httpRequest(http.MethodPost, "/api/auth/login", "", map[string]string{
		"email":    email,
		"password": password,
	})
	if status != http.StatusOK {
		e.t.Fatalf("login: status=%d body=%s", status, string(body))
	}
	var tok tokenPayload
	e.decodeJSON(body, &tok)
	return tok
}

type tokenPayload struct {
	Token       string `json:"token"`
	UserID      string `json:"user_id"`
	WorkspaceID string `json:"workspace_id"`
	Role        string `json:"role"`
}

// staticIPResolver is a webhooks.Resolver implementation that returns
// a fixed public IP for every host. Used by the webhook admin CRUD
// integration tests so subscription-create can validate URLs like
// https://hooks.example.com/x without performing an actual outbound
// DNS lookup (which would couple test reliability to the host's
// recursive resolver state). Returns a public IP rather than a
// private one so the validator's SSRF guard passes.
type staticIPResolver struct {
	ip net.IP
}

func (r staticIPResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: r.ip}}, nil
}

// identityCodec is a no-op totp.Encryptor for the integration tests:
// it round-trips the plaintext unchanged. The crypto package has its
// own unit tests for the real AES-GCM path; here we just exercise
// the enroll / verify / disable lifecycle against a real Postgres.
type identityCodec struct{}

func (identityCodec) Encrypt(_ context.Context, plaintext string) (string, error) {
	return plaintext, nil
}

func (identityCodec) Decrypt(_ context.Context, ciphertext string) (string, error) {
	return ciphertext, nil
}

// testPermissionGranter mirrors the production
// permissionGranterAdapter in cmd/server/main.go so the integration
// tests exercise the same dependency graph as the live server without
// pulling in cmd/server's main package.
type testPermissionGranter struct {
	svc *permission.Service
}

func (a testPermissionGranter) Grant(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, granteeType string, granteeID uuid.UUID, role string, expiresAt *time.Time) (sharing.PermissionRef, error) {
	p, err := a.svc.Grant(ctx, workspaceID, resourceType, resourceID, granteeType, granteeID, role, expiresAt)
	if err != nil {
		return sharing.PermissionRef{}, err
	}
	return sharing.PermissionRef{ID: p.ID}, nil
}

func (a testPermissionGranter) Revoke(ctx context.Context, workspaceID, permID uuid.UUID) error {
	return a.svc.Revoke(ctx, workspaceID, permID)
}

// testFolderCreator bridges folder.Service to sharing.FolderCreator,
// mirroring cmd/server/main.go's folderCreatorAdapter so client-room
// creation in tests exercises the same path as production.
type testFolderCreator struct {
	svc *folder.Service
}

func (a testFolderCreator) Create(ctx context.Context, workspaceID uuid.UUID, parentID *uuid.UUID, name string, createdBy uuid.UUID) (sharing.FolderRef, error) {
	f, err := a.svc.Create(ctx, workspaceID, parentID, name, createdBy)
	if err != nil {
		return sharing.FolderRef{}, err
	}
	return sharing.FolderRef{ID: f.ID}, nil
}
