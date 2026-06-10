// zk-drive compact binary — single-process SME supervisor.
//
// The compact binary is the all-in-one entrypoint for the
// single-command SME deployment (deploy/docker-compose.compact.yml). It
// turns one container into a complete zk-drive control + data plane by:
//
//  1. Forcing the "compact" config profile so the 50+ tunables collapse
//     to the handful that are genuinely site-specific (DATABASE_URL,
//     JWT_SECRET, S3_*) and everything else inherits SME defaults.
//  2. Running an embedded NATS JetStream server in-process (the
//     nats-server Go library) so there is no separate broker container.
//  3. Applying pending database migrations once, up front, under the
//     schema advisory lock — no separate migrate Job/binary.
//  4. Supervising the /app/worker and /app/server child processes that
//     the combined image already ships, restarting them with capped
//     backoff if they crash, and shutting them down gracefully (SIGTERM
//     then a bounded wait) on signal.
//
// Why supervise the existing binaries instead of importing their
// startup as goroutines: the server and worker each own ~1.5k lines of
// dependency wiring, signal handling, and LIFO teardown. Running them
// as child processes keeps that battle-tested startup path byte-for-byte
// identical to the production split-image deployment (so compact mode
// can never silently diverge), gives true fault isolation — a panic in
// the preview pipeline, which shells out to LibreOffice/FFmpeg, cannot
// take the API server down with it — and keeps this file free of merge
// coupling with the large cmd/server and cmd/worker files. The embedded
// NATS server and the migration step run in THIS process; the server
// and worker run as supervised children. Everything lands in one
// container, one command, well under the 512MB target.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/kennguy3n/zk-drive/internal/config"
	"github.com/kennguy3n/zk-drive/internal/database"
	"github.com/kennguy3n/zk-drive/internal/jobs"
	"github.com/kennguy3n/zk-drive/internal/logging"
	"github.com/kennguy3n/zk-drive/internal/version"
)

const (
	// childShutdownGrace bounds how long we wait for a child to exit
	// after SIGTERM before escalating to SIGKILL. The server and worker
	// both run a graceful drain (HTTP shutdown / NATS drain) that
	// completes well within this window under SME load.
	childShutdownGrace = 30 * time.Second

	// natsReadyTimeout bounds the wait for the embedded NATS server to
	// accept connections before we give up and fail startup.
	natsReadyTimeout = 15 * time.Second

	// natsShutdownTimeout bounds the wait for the embedded NATS server
	// to finish shutting down. WaitForShutdown() is otherwise unbounded;
	// if a stuck JetStream flush or consumer wedges it, a bare-metal
	// compact process (no tini/container stop timeout to fall back on)
	// would hang on exit forever. After this ceiling we log and proceed
	// to process exit regardless.
	natsShutdownTimeout = 10 * time.Second

	// jobsStreamReadyTimeout bounds how long we wait for the worker to
	// create the DRIVE_JOBS stream before starting the server anyway.
	// It is an availability ceiling, not a hard requirement: if the
	// worker is unhealthy we still bring the API up (the publisher
	// tolerates a transiently missing stream and the worker supervisor
	// keeps retrying), we just prefer to close the cold-start race when
	// the worker comes up promptly, which it does.
	jobsStreamReadyTimeout = 30 * time.Second

	// restartBackoffMin / restartBackoffMax bound the exponential
	// backoff between child restarts. A child that stays up longer than
	// restartStableAfter resets its backoff to the minimum.
	restartBackoffMin  = 1 * time.Second
	restartBackoffMax  = 30 * time.Second
	restartStableAfter = 60 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("compact supervisor exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	logging.Init("compact")
	slog.Info("zk-drive compact supervisor starting", "version", version.Version)

	// The compact binary IS the compact profile. Force it (only if the
	// operator hasn't explicitly chosen one) BEFORE config.Load so the
	// profile's env defaults are applied and so the values propagate to
	// the child processes via os.Environ() below.
	if _, ok := os.LookupEnv("ZKDRIVE_PROFILE"); !ok {
		if err := os.Setenv("ZKDRIVE_PROFILE", string(config.ProfileCompact)); err != nil {
			return fmt.Errorf("set ZKDRIVE_PROFILE: %w", err)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Signal-aware root context: SIGINT/SIGTERM cancels it, which tears
	// down (in order) the child processes and then the embedded NATS
	// server.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 1) Embedded NATS JetStream — must be up before the worker (which
	//    creates the DRIVE_JOBS stream) and the server (which publishes
	//    jobs) connect.
	ns, err := startEmbeddedNATS(cfg.NATSURL)
	if err != nil {
		return fmt.Errorf("start embedded NATS: %w", err)
	}
	// Shut NATS down LAST (after both children have drained their
	// in-flight acks/publishes), so this defer is registered first.
	defer func() {
		slog.Info("shutting down embedded NATS")
		ns.Shutdown()
		done := make(chan struct{})
		go func() {
			ns.WaitForShutdown()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(natsShutdownTimeout):
			slog.Warn("embedded NATS did not shut down within timeout; proceeding to exit", "timeout", natsShutdownTimeout)
		}
	}()

	// 2) Auto-migrate once, up front, under the advisory lock. Compact
	//    owns its schema — there is no separate migrate Job in the
	//    single-container shape. Children run with ZKDRIVE_AUTO_MIGRATE
	//    forced off (see childEnv) so they don't redundantly re-acquire
	//    the lock on every (re)start.
	if err := migrate(ctx, cfg); err != nil {
		return fmt.Errorf("auto-migrate: %w", err)
	}

	// 3) Resolve the child binaries shipped in the combined image.
	serverBin, err := resolveBinary("ZKDRIVE_SERVER_BIN", "server")
	if err != nil {
		return err
	}
	workerBin, err := resolveBinary("ZKDRIVE_WORKER_BIN", "worker")
	if err != nil {
		return err
	}

	// 4) Supervise both children. Each runs in its own supervise loop;
	//    when ctx is cancelled both loops terminate their child
	//    gracefully and return, and wg.Wait unblocks so the NATS defer
	//    can run.
	//
	//    Ordering matters: the worker creates the DRIVE_JOBS WorkQueue
	//    stream, and a WorkQueue stream silently DROPS messages
	//    published to a subject that has no stream yet. The server's job
	//    publishes are async (PublishMsgAsync), so starting the server
	//    before the stream exists could lose the first jobs in a cold
	//    start. So we start the worker, then block (bounded) until the
	//    stream actually exists before starting the server — a real
	//    barrier, not just goroutine-launch order. The worker remains
	//    the single creator of the stream; we only wait on it.
	env := childEnv()
	var wg sync.WaitGroup
	supervise := func(name, bin string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			superviseChild(ctx, name, bin, env)
		}()
	}

	supervise("worker", workerBin)
	if err := waitForJobsStream(ctx, cfg.NATSURL, jobsStreamReadyTimeout); err != nil {
		if ctx.Err() != nil {
			// Shutting down before the worker came up; skip the server.
			wg.Wait()
			return nil
		}
		// Bounded wait elapsed: start the server anyway (availability
		// over the narrow cold-start race — see jobsStreamReadyTimeout).
		slog.Warn("starting server before DRIVE_JOBS stream is ready", "err", err)
	}
	supervise("server", serverBin)

	<-ctx.Done()
	slog.Info("signal received; draining child processes")
	wg.Wait()
	slog.Info("all children stopped")
	return nil
}

// startEmbeddedNATS boots an in-process NATS server with JetStream
// enabled, bound to the host:port parsed from natsURL (the compact
// profile sets NATS_URL=nats://127.0.0.1:4222, which the children also
// dial). JetStream state is persisted under ZKDRIVE_NATS_STORE_DIR
// (default a temp dir) so durable consumers survive a supervisor
// restart when that path is mounted on a volume.
func startEmbeddedNATS(natsURL string) (*natsserver.Server, error) {
	host, port, err := hostPortFromNATSURL(natsURL)
	if err != nil {
		return nil, err
	}

	storeDir := os.Getenv("ZKDRIVE_NATS_STORE_DIR")
	if storeDir == "" {
		storeDir = filepath.Join(os.TempDir(), "zk-drive-nats-jetstream")
	}
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return nil, fmt.Errorf("create JetStream store dir %q: %w", storeDir, err)
	}
	// Fail fast on a non-writable store dir. MkdirAll is a no-op (and
	// returns nil) when the path already exists but is owned by another
	// user — the common footgun when ZKDRIVE_NATS_STORE_DIR points at a
	// root-owned mounted volume while we run as nonroot. Without this
	// probe JetStream cannot create its sub-store and the only symptom
	// is an opaque "did not become ready" timeout; surface the real
	// cause (permission denied) immediately and actionably instead.
	probe := filepath.Join(storeDir, ".write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return nil, fmt.Errorf("JetStream store dir %q is not writable by uid %d (set ZKDRIVE_NATS_STORE_DIR to a writable path or fix the volume ownership): %w", storeDir, os.Getuid(), err)
	}
	_ = os.Remove(probe)

	opts := &natsserver.Options{
		Host:      host,
		Port:      port,
		JetStream: true,
		StoreDir:  storeDir,
		// NoSigs: the supervisor owns signal handling and forwards
		// shutdown to NATS explicitly; we don't want the embedded
		// server installing its own SIGINT/SIGTERM handlers.
		NoSigs: true,
		// NoLog keeps the embedded broker quiet; zk-drive's own slog
		// output is the operator-facing log surface.
		NoLog: true,
	}

	ns, err := natsserver.NewServer(opts)
	if err != nil {
		return nil, err
	}
	go ns.Start()

	if !ns.ReadyForConnections(natsReadyTimeout) {
		ns.Shutdown()
		ns.WaitForShutdown()
		return nil, fmt.Errorf("embedded NATS did not become ready within %s", natsReadyTimeout)
	}
	slog.Info("embedded NATS JetStream ready", "addr", net.JoinHostPort(host, fmt.Sprintf("%d", port)), "store_dir", storeDir)
	return ns, nil
}

// migrate connects to Postgres and applies pending migrations under the
// advisory lock, then closes the pool. Run once at startup before any
// child process comes up.
func migrate(ctx context.Context, cfg *config.Config) error {
	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()

	start := time.Now()
	if err := database.Migrate(ctx, pool, cfg.MigrationsDir); err != nil {
		return err
	}
	slog.Info("migrations applied",
		"duration", time.Since(start).Round(time.Millisecond).String(),
		"migrations_dir", cfg.MigrationsDir,
	)
	return nil
}

// waitForJobsStream blocks until the worker has created the DRIVE_JOBS
// JetStream stream, or until timeout / ctx cancellation. The supervisor
// owns the embedded broker, so it connects as a plain client and polls
// StreamInfo rather than creating the stream itself — the worker stays
// the single source of truth for the stream's config. Returns nil as
// soon as the stream exists; a non-nil error means "not ready" (timeout
// or shutdown), which the caller treats as non-fatal.
func waitForJobsStream(ctx context.Context, natsURL string, timeout time.Duration) error {
	url := natsURL
	if url == "" {
		url = nats.DefaultURL
	}
	nc, err := nats.Connect(url,
		nats.Name("zk-drive-compact-supervisor"),
		nats.Timeout(5*time.Second),
	)
	if err != nil {
		return fmt.Errorf("connect NATS for stream readiness: %w", err)
	}
	defer nc.Close()

	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("jetstream context: %w", err)
	}

	deadline := time.Now().Add(timeout)
	for {
		if _, err := js.StreamInfo(jobs.StreamName); err == nil {
			slog.Info("DRIVE_JOBS stream ready", "stream", jobs.StreamName)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("stream %q not ready within %s", jobs.StreamName, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// childEnv returns the environment passed to the child processes: the
// supervisor's own environment (which already carries the resolved
// compact-profile defaults from applyProfileDefaults) with
// ZKDRIVE_AUTO_MIGRATE forced off, because the supervisor has already
// migrated and we don't want each child re-acquiring the advisory lock
// on every restart.
func childEnv() []string {
	env := os.Environ()
	out := env[:0:0]
	for _, kv := range env {
		if len(kv) >= len("ZKDRIVE_AUTO_MIGRATE=") && kv[:len("ZKDRIVE_AUTO_MIGRATE=")] == "ZKDRIVE_AUTO_MIGRATE=" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "ZKDRIVE_AUTO_MIGRATE=false")
}

// superviseChild runs one child binary, restarting it with capped
// exponential backoff if it exits while ctx is still live, and
// terminating it gracefully (SIGTERM, then SIGKILL after
// childShutdownGrace) once ctx is cancelled.
func superviseChild(ctx context.Context, name, bin string, env []string) {
	backoff := restartBackoffMin
	for {
		if ctx.Err() != nil {
			return
		}
		startedAt := time.Now()
		err := runChildOnce(ctx, name, bin, env)
		if ctx.Err() != nil {
			// Shutting down: a non-nil err here is just the child
			// reacting to SIGTERM, not a crash.
			return
		}
		if err != nil {
			slog.Error("child exited unexpectedly; will restart", "child", name, "err", err)
		} else {
			slog.Warn("child exited cleanly but unexpectedly; will restart", "child", name)
		}
		// Reset backoff if the child had been stable for a while; a
		// fast crash-loop keeps backing off up to the cap.
		if time.Since(startedAt) >= restartStableAfter {
			backoff = restartBackoffMin
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > restartBackoffMax {
			backoff = restartBackoffMax
		}
	}
}

// runChildOnce starts the child, streams its stdout/stderr to the
// supervisor's, and blocks until it exits or ctx is cancelled. On
// cancellation it sends SIGTERM and waits up to childShutdownGrace
// before SIGKILL.
func runChildOnce(ctx context.Context, name, bin string, env []string) error {
	cmd := exec.Command(bin)
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Own process group so a stray child of the child is reaped with
	// the group on SIGKILL escalation.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	slog.Info("child started", "child", name, "pid", cmd.Process.Pid, "bin", bin)

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	select {
	case err := <-waitErr:
		return err
	case <-ctx.Done():
		slog.Info("stopping child", "child", name, "pid", cmd.Process.Pid)
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-waitErr:
		case <-time.After(childShutdownGrace):
			slog.Warn("child did not exit within grace period; killing", "child", name, "grace", childShutdownGrace)
			// SIGKILL the whole process group (negative PID), not just
			// the child's PID. We set Setpgid above precisely so a hung
			// child's grandchildren — e.g. a stuck LibreOffice/FFmpeg the
			// worker shelled out to — are reaped with it instead of being
			// reparented and leaked. os.Process.Kill() targets only the
			// single PID, which is the bug this avoids.
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-waitErr
		}
		return nil
	}
}

// resolveBinary locates a child binary. Precedence:
//  1. The env override (ZKDRIVE_SERVER_BIN / ZKDRIVE_WORKER_BIN) if set.
//  2. /app/<name> (the combined image layout).
//  3. <dir of this executable>/<name> (local builds placed side by side).
//  4. <name> on $PATH.
//
// Returning a clear error rather than letting exec fail later means a
// misconfigured deployment surfaces "worker binary not found" at
// startup, not as a silent missing-async-pipeline at runtime.
func resolveBinary(envKey, name string) (string, error) {
	if v := os.Getenv(envKey); v != "" {
		if isExecutableFile(v) {
			return v, nil
		}
		return "", fmt.Errorf("%s=%q is not an executable file", envKey, v)
	}
	candidates := []string{filepath.Join("/app", name)}
	if self, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(self), name))
	}
	for _, c := range candidates {
		if isExecutableFile(c) {
			return c, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("could not locate %q binary (set %s to override; looked in %v and $PATH)", name, envKey, candidates)
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}

// hostPortFromNATSURL extracts the host and numeric port the embedded
// server should bind from a nats:// URL. Defaults to 127.0.0.1:4222
// when the URL is empty or omits a component.
func hostPortFromNATSURL(natsURL string) (string, int, error) {
	host, port := "127.0.0.1", natsserver.DEFAULT_PORT
	if natsURL == "" {
		return host, port, nil
	}
	u, err := url.Parse(natsURL)
	if err != nil {
		return "", 0, fmt.Errorf("parse NATS_URL %q: %w", natsURL, err)
	}
	if h := u.Hostname(); h != "" {
		host = h
	}
	if p := u.Port(); p != "" {
		n, err := net.LookupPort("tcp", p)
		if err != nil {
			return "", 0, fmt.Errorf("invalid NATS_URL port %q: %w", p, err)
		}
		port = n
	}
	return host, port, nil
}
