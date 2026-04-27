package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/zk-drive/api/admin"
	apikchat "github.com/kennguy3n/zk-drive/api/kchat"
	"github.com/kennguy3n/zk-drive/api/auth"
	"github.com/kennguy3n/zk-drive/api/drive"
	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/activity"
	"github.com/kennguy3n/zk-drive/internal/ai"
	"github.com/kennguy3n/zk-drive/internal/audit"
	"github.com/kennguy3n/zk-drive/internal/billing"
	"github.com/kennguy3n/zk-drive/internal/database"
	"github.com/kennguy3n/zk-drive/internal/fabric"
	"github.com/kennguy3n/zk-drive/internal/file"
	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/kchat"
	"github.com/kennguy3n/zk-drive/internal/notification"
	"github.com/kennguy3n/zk-drive/internal/permission"
	"github.com/kennguy3n/zk-drive/internal/preview"
	"github.com/kennguy3n/zk-drive/internal/retention"
	"github.com/kennguy3n/zk-drive/internal/search"
	"github.com/kennguy3n/zk-drive/internal/sharing"
	"github.com/kennguy3n/zk-drive/internal/wiring"
	"github.com/kennguy3n/zk-drive/internal/storage"
	"github.com/kennguy3n/zk-drive/internal/user"
	"github.com/kennguy3n/zk-drive/internal/workspace"

	"github.com/google/uuid"
)

// testJWTSecret is the HS256 secret used by every integration test. Shared
// so tests can compose their own calls where needed.
const testJWTSecret = "integration-test-secret"

// testEnv bundles everything a test needs to exercise the API: a live
// httptest server, the pgx pool, and the initialised services. Callers use
// testEnv.ResetTables between tests.
type testEnv struct {
	t           *testing.T
	pool        *pgxpool.Pool
	server      *httptest.Server
	storage     *storage.Client
	provisioner *fabric.Provisioner
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
	folderSvc := folder.NewService(folder.NewPostgresRepository(pool))
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
	billingSvc := billing.NewService(billing.NewPostgresRepository(pool))

	authHandler := auth.NewHandler(pool, userSvc, wsSvc, testJWTSecret).WithAudit(auditSvc)
	driveHandler := drive.NewHandler(pool, wsSvc, folderSvc, fileSvc, userSvc, storageClient, permissionSvc, activitySvc).
		WithSharing(sharingSvc).
		WithSearch(searchSvc).
		WithClientRooms(clientRoomSvc).
		WithNotifications(notificationSvc).
		WithPreviews(previewRepo).
		WithAudit(auditSvc).
		WithBilling(billingSvc)
	// Wire a Postgres-backed fabric provisioner with no console URL
	// so admin endpoints that only need persistence (CMK) work; the
	// FabricClient interface is left nil because no test fakes the
	// upstream console yet.
	provisioner := fabric.NewProvisioner(pool, fabric.Config{})
	adminHandler := admin.NewHandler(pool, userSvc, auditSvc, retentionSvc).
		WithBilling(billingSvc).
		WithFabric(nil, provisioner, nil)

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
	)
	summarySvc := ai.NewSummaryService(pool)
	kchatHandler := apikchat.NewHandler(kchatSvc, summarySvc)

	r := chi.NewRouter()
	r.Use(chimw.Recoverer)

	r.Route("/api", func(r chi.Router) {
		r.Route("/auth", func(r chi.Router) {
			r.Post("/signup", authHandler.Signup)
			r.Post("/login", authHandler.Login)
			r.Post("/logout", authHandler.Logout)
			r.Group(func(r chi.Router) {
				r.Use(middleware.AuthMiddleware(testJWTSecret))
				r.Post("/refresh", authHandler.Refresh)
			})
		})
		r.Group(func(r chi.Router) {
			r.Use(middleware.AuthMiddleware(testJWTSecret))
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
			r.Use(middleware.AuthMiddleware(testJWTSecret))
			r.Use(middleware.TenantGuard())
			r.Use(middleware.AdminOnly())
			adminHandler.RegisterRoutes(r)
		})

		r.Route("/kchat", func(r chi.Router) {
			r.Use(middleware.AuthMiddleware(testJWTSecret))
			r.Use(middleware.TenantGuard())
			kchatHandler.RegisterRoutes(r)
		})

		r.Get("/share-links/{token}", driveHandler.ResolveShareLink)
		r.Post("/share-links/{token}", driveHandler.ResolveShareLink)
	})

	srv := httptest.NewServer(r)
	env := &testEnv{t: t, pool: pool, server: srv, storage: storageClient, provisioner: provisioner}
	t.Cleanup(func() {
		srv.Close()
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
// tests can share a database without leaking rows.
func (e *testEnv) ResetTables() {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stmts := []string{
		`ALTER TABLE workspaces DROP CONSTRAINT IF EXISTS fk_workspaces_owner`,
		`TRUNCATE kchat_room_folders, workspace_storage_credentials, workspace_plans, usage_events, file_tags, retention_policies, audit_log, notifications, file_previews, client_rooms, guest_invites, share_links, activity_log, permissions, file_versions, files, folders, users, workspaces RESTART IDENTITY CASCADE`,
		`ALTER TABLE workspaces ADD CONSTRAINT fk_workspaces_owner FOREIGN KEY (owner_user_id) REFERENCES users(id)`,
	}
	for _, s := range stmts {
		if _, err := e.pool.Exec(ctx, s); err != nil {
			e.t.Fatalf("reset tables (%q): %v", s, err)
		}
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
	defer resp.Body.Close()
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

type tokenPayload struct {
	Token       string `json:"token"`
	UserID      string `json:"user_id"`
	WorkspaceID string `json:"workspace_id"`
	Role        string `json:"role"`
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
