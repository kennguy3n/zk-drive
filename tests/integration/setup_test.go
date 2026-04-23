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

	"github.com/kennguy3n/zk-drive/api/auth"
	"github.com/kennguy3n/zk-drive/api/drive"
	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/database"
	"github.com/kennguy3n/zk-drive/internal/file"
	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/storage"
	"github.com/kennguy3n/zk-drive/internal/user"
	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// testJWTSecret is the HS256 secret used by every integration test. Shared
// so tests can compose their own calls where needed.
const testJWTSecret = "integration-test-secret"

// testEnv bundles everything a test needs to exercise the API: a live
// httptest server, the pgx pool, and the initialised services. Callers use
// testEnv.ResetTables between tests.
type testEnv struct {
	t       *testing.T
	pool    *pgxpool.Pool
	server  *httptest.Server
	storage *storage.Client
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

	authHandler := auth.NewHandler(pool, userSvc, wsSvc, testJWTSecret)
	driveHandler := drive.NewHandler(pool, wsSvc, folderSvc, fileSvc, userSvc, storageClient)

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
		})
	})

	srv := httptest.NewServer(r)
	env := &testEnv{t: t, pool: pool, server: srv, storage: storageClient}
	t.Cleanup(func() {
		srv.Close()
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
		`TRUNCATE activity_log, permissions, file_versions, files, folders, users, workspaces RESTART IDENTITY CASCADE`,
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
