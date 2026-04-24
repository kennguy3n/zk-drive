package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/kennguy3n/zk-drive/api/auth"
	"github.com/kennguy3n/zk-drive/api/drive"
	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/activity"
	"github.com/kennguy3n/zk-drive/internal/config"
	"github.com/kennguy3n/zk-drive/internal/database"
	"github.com/kennguy3n/zk-drive/internal/file"
	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/jobs"
	"github.com/kennguy3n/zk-drive/internal/notification"
	"github.com/kennguy3n/zk-drive/internal/permission"
	"github.com/kennguy3n/zk-drive/internal/preview"
	"github.com/kennguy3n/zk-drive/internal/search"
	"github.com/kennguy3n/zk-drive/internal/sharing"
	"github.com/kennguy3n/zk-drive/internal/storage"
	"github.com/kennguy3n/zk-drive/internal/user"
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
		log.Printf("storage: presigned-URL client wired to %s (bucket=%s)", cfg.S3Endpoint, cfg.S3Bucket)
	} else {
		log.Printf("storage: S3_ENDPOINT not set, upload/download-url endpoints will return 501")
	}

	permissionSvc := permission.NewService(permission.NewPostgresRepository(pool))
	activitySvc := activity.NewService(activity.NewPostgresRepository(pool))
	defer activitySvc.Close()

	sharingSvc := sharing.NewService(sharing.NewPostgresRepository(pool), permissionGranterAdapter{permissionSvc})
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

	notificationSvc := notification.NewService(notification.NewPostgresRepository(pool))
	previewRepo := preview.NewPostgresRepository(pool)

	authHandler := auth.NewHandler(pool, userSvc, wsSvc, cfg.JWTSecret)
	driveHandler := drive.NewHandler(pool, wsSvc, folderSvc, fileSvc, userSvc, storageClient, permissionSvc, activitySvc).
		WithSharing(sharingSvc).
		WithSearch(searchSvc).
		WithClientRooms(clientRoomSvc).
		WithJobs(jobPublisher).
		WithNotifications(notificationSvc).
		WithPreviews(previewRepo)

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
			r.Post("/logout", authHandler.Logout)

			r.Group(func(r chi.Router) {
				r.Use(middleware.AuthMiddleware(cfg.JWTSecret))
				r.Post("/refresh", authHandler.Refresh)
			})
		})

		r.Group(func(r chi.Router) {
			r.Use(middleware.AuthMiddleware(cfg.JWTSecret))
			r.Use(middleware.TenantGuard())

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
			r.Get("/client-rooms/{id}", driveHandler.GetClientRoom)
			r.Delete("/client-rooms/{id}", driveHandler.DeleteClientRoom)

			r.Get("/search", driveHandler.Search)

			r.Get("/notifications", driveHandler.ListNotifications)
			r.Post("/notifications/read-all", driveHandler.MarkAllNotificationsRead)
			r.Post("/notifications/{id}/read", driveHandler.MarkNotificationRead)

			r.Get("/activity", driveHandler.ListActivity)
		})

		// Public share-link resolution — deliberately outside the auth
		// group so anyone holding a token can resolve the link
		// (ARCHITECTURE.md §7.3). Password / expiry / download-cap
		// checks run in the sharing service.
		r.Get("/share-links/{token}", driveHandler.ResolveShareLink)
		r.Post("/share-links/{token}", driveHandler.ResolveShareLink)
	})

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

// permissionGranterAdapter bridges *permission.Service to
// sharing.PermissionGranter. The sharing package can't import
// permission directly without creating a dependency loop in future
// packages that want to use both sides, so we keep the adapter at the
// cmd/server layer where the full dependency graph is already visible.
type permissionGranterAdapter struct {
	svc *permission.Service
}

func (a permissionGranterAdapter) Grant(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, granteeType string, granteeID uuid.UUID, role string, expiresAt *time.Time) (sharing.PermissionRef, error) {
	p, err := a.svc.Grant(ctx, workspaceID, resourceType, resourceID, granteeType, granteeID, role, expiresAt)
	if err != nil {
		return sharing.PermissionRef{}, err
	}
	return sharing.PermissionRef{ID: p.ID}, nil
}

func (a permissionGranterAdapter) Revoke(ctx context.Context, workspaceID, permID uuid.UUID) error {
	return a.svc.Revoke(ctx, workspaceID, permID)
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
