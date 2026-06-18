// Package benchmark holds the performance benchmark suite.
//
// These are real, DB-backed benchmarks — they exercise the same
// service/repository layer the HTTP handlers call, against a live
// Postgres reached via TEST_DATABASE_URL (the same fixture the
// integration suite uses). A benchmark that cannot reach Postgres
// skips rather than fails, so `go test ./...` (which compiles but does
// not run benchmarks) and `go vet ./...` stay green in environments
// without a database, while `go test -bench` against a provisioned DB
// produces meaningful numbers.
//
// Performance targets are encoded as
// constants next to each benchmark and surfaced through b.ReportMetric
// so a run prints, e.g., the achieved ops/s alongside the target and a
// p95 latency where the spec calls for one. The benchmarks deliberately
// avoid network I/O that would dominate the measurement: presigned-URL
// generation signs locally (no S3 round-trip), and the WebSocket fan-out
// benchmark drives the in-memory Hub directly. Everything that touches
// Postgres goes through the production code path including row-level
// security (the tenant GUC is bound per connection via tenantctx).
package benchmark

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/database"
	"github.com/kennguy3n/zk-drive/internal/file"
	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/permission"
	"github.com/kennguy3n/zk-drive/internal/search"
	"github.com/kennguy3n/zk-drive/internal/storage"
	"github.com/kennguy3n/zk-drive/internal/tenantctx"
	"github.com/kennguy3n/zk-drive/internal/user"
	"github.com/kennguy3n/zk-drive/internal/workspace"

	"github.com/jackc/pgx/v5/pgxpool"
)

// benchEnv is the shared fixture for the benchmark suite: a migrated
// pool plus the production services, scoped to one freshly-created
// workspace and owner user. wsCtx carries the workspace id so every
// pooled connection binds the app.workspace_id GUC and exercises RLS
// exactly as production does.
type benchEnv struct {
	pool        *pgxpool.Pool
	wsID        uuid.UUID
	ownerID     uuid.UUID
	wsCtx       context.Context
	folders     *folder.Service
	files       *file.Service
	users       *user.Service
	workspaces  *workspace.Service
	permissions *permission.Service
	search      *search.Service
	storage     *storage.Client
}

// setupBench connects to TEST_DATABASE_URL, migrates, wires the
// production services, and creates an isolated workspace + owner user.
// It skips (not fails) when no database is configured so the suite
// compiles and is a no-op without infrastructure.
func setupBench(b *testing.B) *benchEnv {
	b.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		b.Skip("TEST_DATABASE_URL not set; skipping benchmark")
	}

	ctx := context.Background()
	pool, err := database.Connect(ctx, dsn)
	if err != nil {
		b.Fatalf("connect postgres: %v", err)
	}
	b.Cleanup(pool.Close)

	if err := database.Migrate(ctx, pool, findMigrationsDir(b)); err != nil {
		b.Fatalf("migrate: %v", err)
	}

	wsSvc := workspace.NewService(workspace.NewPostgresRepository(pool))
	userSvc := user.NewService(user.NewPostgresRepository(pool))

	// Workspace + owner are created with a bare (no-workspace) context:
	// the RLS policies fail open when app.workspace_id is unset, which
	// is exactly how signup bootstraps the very first tenant row before
	// any workspace id exists. Every subsequent per-tenant call uses
	// wsCtx below so the GUC is bound and RLS is enforced.
	ws, err := wsSvc.Create(ctx, "bench-"+uuid.NewString())
	if err != nil {
		b.Fatalf("create workspace: %v", err)
	}
	owner, err := userSvc.Create(ctx, ws.ID,
		fmt.Sprintf("owner+%s@bench.local", uuid.NewString()),
		"Bench Owner", "benchPassw0rd!", "admin")
	if err != nil {
		b.Fatalf("create owner: %v", err)
	}

	env := &benchEnv{
		pool:        pool,
		wsID:        ws.ID,
		ownerID:     owner.ID,
		wsCtx:       tenantctx.WithWorkspaceID(ctx, ws.ID),
		folders:     folder.NewService(folder.NewPostgresRepository(pool)),
		files:       file.NewService(file.NewPostgresRepository(pool)),
		users:       userSvc,
		workspaces:  wsSvc,
		permissions: permission.NewService(permission.NewPostgresRepository(pool)),
		search:      search.NewService(pool),
		storage:     buildBenchStorage(b),
	}
	return env
}

// buildBenchStorage builds a presign-only storage client. The endpoint
// is never dialled — GenerateDownloadURL signs locally — so a parseable
// placeholder URL and demo credentials are sufficient and the benchmark
// stays free of S3 network latency.
func buildBenchStorage(b *testing.B) *storage.Client {
	b.Helper()
	endpoint := os.Getenv("S3_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:65535"
	}
	client, err := storage.NewClient(storage.Config{
		Endpoint:  endpoint,
		Bucket:    envOr("S3_BUCKET", "zk-drive-bench"),
		AccessKey: envOr("S3_ACCESS_KEY", "demo-access-key"),
		SecretKey: envOr("S3_SECRET_KEY", "demo-secret-key"),
	})
	if err != nil {
		b.Fatalf("storage client: %v", err)
	}
	return client
}

// rootFolder creates and returns a single top-level folder for the
// workspace, used by benchmarks that need a place to hang files.
func (e *benchEnv) rootFolder(b *testing.B) uuid.UUID {
	b.Helper()
	f, err := e.folders.Create(e.wsCtx, e.wsID, nil, "bench-root-"+uuid.NewString(), e.ownerID)
	if err != nil {
		b.Fatalf("create root folder: %v", err)
	}
	return f.ID
}

// seedFiles inserts n files into folderID with names that all share
// `token` so a search for `token` matches every row. It returns the
// elapsed wall time for the seed so callers can log it; the seed is not
// part of the measured region (benchmarks call b.ResetTimer after).
func (e *benchEnv) seedFiles(b *testing.B, folderID uuid.UUID, n int, token string) {
	b.Helper()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("%s-doc-%06d.txt", token, i)
		if _, err := e.files.Create(e.wsCtx, e.wsID, folderID, name, "text/plain", e.ownerID); err != nil {
			b.Fatalf("seed file %d: %v", i, err)
		}
	}
}

// envOr returns the env var value or a default.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envInt reads an integer env var, returning def when unset or invalid.
// Used to scale seed sizes (e.g. BENCH_SEARCH_FILES) without recompiling.
func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
		return def
	}
	return n
}

// findMigrationsDir walks up from this file to locate the migrations
// directory so the suite runs from any working directory (mirrors the
// integration harness helper).
func findMigrationsDir(b *testing.B) string {
	b.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		b.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, "migrations")
		if stat, err := os.Stat(candidate); err == nil && stat.IsDir() {
			return candidate
		}
		dir = filepath.Dir(dir)
	}
	b.Fatal("could not locate migrations directory")
	return ""
}

// reportThroughput converts the standard ns/op the framework already
// tracks into ops/s and reports it next to the spec target so a run is
// self-documenting: the harness does not pass/fail on the target (a
// shared CI box is not a perf rig), it surfaces the achieved number for
// a human or a dedicated perf job to compare against `target`.
func reportThroughput(b *testing.B, target float64) {
	nsPerOp := float64(b.Elapsed().Nanoseconds()) / float64(b.N)
	if nsPerOp <= 0 {
		return
	}
	opsPerSec := 1e9 / nsPerOp
	b.ReportMetric(opsPerSec, "ops/s")
	b.ReportMetric(target, "target-ops/s")
}

// latencyRecorder collects per-operation latencies so a benchmark can
// report a p95 (the search spec is expressed as a p95 latency budget,
// which the default ns/op average cannot express).
type latencyRecorder struct {
	samples []time.Duration
}

func (r *latencyRecorder) record(d time.Duration) { r.samples = append(r.samples, d) }

// reportPercentiles reports p50/p95/p99 in milliseconds and echoes the
// target budget. Safe to call with no samples.
func (r *latencyRecorder) reportPercentiles(b *testing.B, targetP95 time.Duration) {
	if len(r.samples) == 0 {
		return
	}
	sort.Slice(r.samples, func(i, j int) bool { return r.samples[i] < r.samples[j] })
	pct := func(p float64) time.Duration {
		idx := int(p * float64(len(r.samples)))
		if idx >= len(r.samples) {
			idx = len(r.samples) - 1
		}
		return r.samples[idx]
	}
	toMS := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }
	b.ReportMetric(toMS(pct(0.50)), "p50-ms")
	b.ReportMetric(toMS(pct(0.95)), "p95-ms")
	b.ReportMetric(toMS(pct(0.99)), "p99-ms")
	b.ReportMetric(toMS(targetP95), "target-p95-ms")
}
