package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
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
	"github.com/kennguy3n/zk-drive/internal/kchat"
	"github.com/kennguy3n/zk-drive/internal/jobs"
	"github.com/kennguy3n/zk-drive/internal/notification"
	"github.com/kennguy3n/zk-drive/internal/permission"
	"github.com/kennguy3n/zk-drive/internal/preview"
	"github.com/kennguy3n/zk-drive/internal/retention"
	"github.com/kennguy3n/zk-drive/internal/search"
	"github.com/kennguy3n/zk-drive/internal/session"
	"github.com/kennguy3n/zk-drive/internal/sharing"
	"github.com/kennguy3n/zk-drive/internal/storage"
	"github.com/kennguy3n/zk-drive/internal/user"
	"github.com/kennguy3n/zk-drive/internal/wiring"
	"github.com/kennguy3n/zk-drive/internal/workspace"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("server exited: %v", err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()

	if err := database.Migrate(ctx, pool, cfg.MigrationsDir); err != nil {
		return fmt.Errorf("migrate: %w", err)
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
		log.Printf("storage: fallback presigned-URL client wired to %s (bucket=%s)", cfg.S3Endpoint, cfg.S3Bucket)
	} else {
		log.Printf("storage: S3_ENDPOINT not set; per-workspace credentials must be provisioned via fabric")
	}

	credentialCodec, err := cryptopkg.LoadFromEnv()
	if err != nil {
		return fmt.Errorf("credential codec: %w", err)
	}
	log.Printf("crypto: credential encryption mode=%s", credentialCodec.Mode())

	storageFactory := storage.NewClientFactory(pool, storageClient, credentialCodec)

	provisioner := fabric.NewProvisioner(pool, fabric.Config{
		ConsoleURL:       cfg.FabricConsoleURL,
		BucketTemplate:   cfg.FabricBucketTemplate,
		DefaultPolicyRef: cfg.FabricDefaultPlacementRef,
		Encryptor:        credentialCodec,
	})
	if cfg.FabricConsoleURL != "" {
		log.Printf("fabric: tenant provisioning enabled, console=%s", cfg.FabricConsoleURL)
	} else {
		log.Printf("fabric: console URL not set, signup will skip tenant provisioning")
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
	var jobPublisher *jobs.Publisher
	if natsURL := os.Getenv("NATS_URL"); natsURL != "" {
		nc, nerr := nats.Connect(natsURL,
			nats.Name("zk-drive-server"),
			nats.MaxReconnects(-1),
			nats.ReconnectWait(2*time.Second),
		)
		if nerr != nil {
			log.Printf("nats: connect %s failed, post-upload jobs disabled: %v", natsURL, nerr)
		} else {
			js, jerr := nc.JetStream()
			if jerr != nil {
				log.Printf("nats: jetstream context failed: %v", jerr)
				nc.Close()
			} else {
				jobPublisher = jobs.NewPublisher(js)
				log.Printf("nats: connected to %s, post-upload jobs enabled", natsURL)
				defer nc.Drain() //nolint:errcheck // best-effort drain
			}
		}
	}

	// Optional Redis client. When REDIS_URL is set, the rate
	// limiter and session store switch to a shared backend so
	// both behave correctly behind multiple replicas. When unset,
	// the in-memory implementations are used (single-replica).
	var redisClient *redis.Client
	var sessionStore *session.RedisSessionStore
	if cfg.RedisURL != "" {
		opts, perr := redis.ParseURL(cfg.RedisURL)
		if perr != nil {
			return fmt.Errorf("parse REDIS_URL: %w", perr)
		}
		redisClient = redis.NewClient(opts)
		defer redisClient.Close()
		if perr := redisClient.Ping(ctx).Err(); perr != nil {
			log.Printf("redis: ping %s failed, continuing with in-memory fallbacks: %v", cfg.RedisURL, perr)
			redisClient = nil
		} else {
			sessionStore = session.NewRedisSessionStore(redisClient)
			log.Printf("redis: connected to %s, rate limiter and session store backed by Redis", cfg.RedisURL)
		}
	} else {
		log.Printf("redis: REDIS_URL not set, using in-memory rate limiter (single-replica only)")
	}
	_ = sessionStore // session store is wired into auth handlers in a follow-up

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

	notificationSvc := notification.NewService(notification.NewPostgresRepository(pool))
	previewRepo := preview.NewPostgresRepository(pool)
	auditSvc := audit.NewService(audit.NewPostgresRepository(pool))
	defer auditSvc.Close()
	retentionSvc := retention.NewService(retention.NewPostgresRepository(pool), pool)

	authHandler := auth.NewHandler(pool, userSvc, wsSvc, cfg.JWTSecret).
		WithAudit(auditSvc).
		WithPostSignupHook(func(ctx context.Context, workspaceID uuid.UUID, workspaceName string) {
			// Best-effort: provision a fabric tenant for the new
			// workspace. Errors are logged and swallowed so signup
			// stays durable even when the console is unreachable.
			if cfg.FabricConsoleURL == "" {
				return
			}
			if _, err := provisioner.Provision(ctx, workspaceID, workspaceName); err != nil {
				log.Printf("fabric: provision workspace=%s: %v", workspaceID, err)
				return
			}
			storageFactory.Invalidate(workspaceID)
			log.Printf("fabric: provisioned workspace=%s", workspaceID)
		})
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
		log.Printf("billing: stripe webhook signature verification enabled")
	} else {
		log.Printf("billing: STRIPE_WEBHOOK_SECRET not set, /api/webhooks/stripe will reject all requests")
	}
	if cfg.StripeSecretKey != "" {
		log.Printf("billing: stripe checkout / portal session creation enabled")
	} else {
		log.Printf("billing: STRIPE_SECRET_KEY not set, /api/admin/billing/{checkout,portal}-session will respond 501")
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
		WithFabric(fabricClient, provisioner, storageFactory)

	kchatSvc := kchat.NewRoomService(
		kchat.NewPostgresRepository(pool),
		wiring.NewKChatFolderCreator(folderSvc),
		wiring.NewKChatPermissionGranter(permissionSvc),
		wiring.NewKChatFileCreator(fileSvc),
		wiring.NewKChatPresignResolver(storageFactory),
		wiring.KChatObjectKey,
	)
	summarySvc := ai.NewSummaryService(pool)
	kchatHandler := apikchat.NewHandler(kchatSvc, summarySvc)

	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Use(chimw.RealIP)
	r.Use(chimw.Logger)
	r.Use(chimw.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	r.Route("/api", func(r chi.Router) {
		r.Route("/auth", func(r chi.Router) {
			r.Post("/signup", authHandler.Signup)
			r.Post("/login", authHandler.Login)
			r.Route("/oauth", func(r chi.Router) {
				oauthHandler.RegisterRoutes(r)
			})

			r.Group(func(r chi.Router) {
				r.Use(middleware.AuthMiddleware(cfg.JWTSecret))
				r.Post("/logout", authHandler.Logout)
				r.Post("/refresh", authHandler.Refresh)
			})
		})

		r.Group(func(r chi.Router) {
			r.Use(middleware.AuthMiddleware(cfg.JWTSecret))
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
			r.Use(middleware.AuthMiddleware(cfg.JWTSecret))
			r.Use(middleware.TenantGuard())
			r.Use(middleware.AdminOnly())
			r.Use(rateLimiter())
			adminHandler.RegisterRoutes(r)
		})

		r.Route("/kchat", func(r chi.Router) {
			r.Use(middleware.AuthMiddleware(cfg.JWTSecret))
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
			log.Printf("static: serving SPA assets from %s", cfg.StaticDir)
			r.NotFound(spaHandler(cfg.StaticDir))
		} else {
			log.Printf("static: STATIC_DIR=%q is not a readable directory, skipping SPA serving", cfg.StaticDir)
		}
	}

	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("zk-drive server listening on %s", cfg.ListenAddr)
		errCh <- srv.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Printf("received signal %s, shutting down", sig)
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
// a hard refresh. The `/api` and `/healthz` namespaces are already
// handled before this NotFound handler runs, so we only see SPA paths
// here. We deliberately reject `..` traversal to keep the handler safe
// when STATIC_DIR is a sibling of sensitive files.
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
