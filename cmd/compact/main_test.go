package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/kennguy3n/zk-drive/internal/jobs"
)

// TestHostPortFromNATSURL covers the parse paths the embedded broker
// relies on to decide where to bind.
func TestHostPortFromNATSURL(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{name: "empty defaults to loopback", in: "", wantHost: "127.0.0.1", wantPort: natsserver.DEFAULT_PORT},
		{name: "full url", in: "nats://10.0.0.5:6222", wantHost: "10.0.0.5", wantPort: 6222},
		{name: "host only keeps default port", in: "nats://broker", wantHost: "broker", wantPort: natsserver.DEFAULT_PORT},
		{name: "loopback explicit", in: "nats://127.0.0.1:4222", wantHost: "127.0.0.1", wantPort: 4222},
		{name: "bad port", in: "nats://h:notaport", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host, port, err := hostPortFromNATSURL(tc.in)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if host != tc.wantHost || port != tc.wantPort {
				t.Fatalf("got %s:%d, want %s:%d", host, port, tc.wantHost, tc.wantPort)
			}
		})
	}
}

// TestChildEnvForcesAutoMigrateOff: children must never run migrations —
// the supervisor already migrated once under the advisory lock, so each
// child inherits ZKDRIVE_AUTO_MIGRATE=false exactly once regardless of
// what the parent env held.
func TestChildEnvForcesAutoMigrateOff(t *testing.T) {
	t.Setenv("ZKDRIVE_AUTO_MIGRATE", "true")
	t.Setenv("SOME_OTHER_VAR", "kept")

	env := childEnv()

	var autoMigrate []string
	var sawOther bool
	for _, kv := range env {
		if strings.HasPrefix(kv, "ZKDRIVE_AUTO_MIGRATE=") {
			autoMigrate = append(autoMigrate, kv)
		}
		if kv == "SOME_OTHER_VAR=kept" {
			sawOther = true
		}
	}
	if len(autoMigrate) != 1 || autoMigrate[0] != "ZKDRIVE_AUTO_MIGRATE=false" {
		t.Fatalf("ZKDRIVE_AUTO_MIGRATE entries = %v, want exactly [ZKDRIVE_AUTO_MIGRATE=false]", autoMigrate)
	}
	if !sawOther {
		t.Fatalf("childEnv dropped unrelated env var")
	}
}

// TestResolveBinaryEnvOverride: an explicit ZKDRIVE_*_BIN pointing at an
// executable wins; pointing at a non-executable fails fast.
func TestResolveBinaryEnvOverride(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission model")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "server")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	notExe := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(notExe, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ZKDRIVE_SERVER_BIN", exe)
	got, err := resolveBinary("ZKDRIVE_SERVER_BIN", "server")
	if err != nil {
		t.Fatalf("resolveBinary: %v", err)
	}
	if got != exe {
		t.Fatalf("got %q, want %q", got, exe)
	}

	t.Setenv("ZKDRIVE_SERVER_BIN", notExe)
	if _, err := resolveBinary("ZKDRIVE_SERVER_BIN", "server"); err == nil {
		t.Fatalf("expected error for non-executable override")
	}
}

// TestWaitForJobsStream verifies the cold-start barrier the supervisor
// uses before launching the server: it must block while the DRIVE_JOBS
// stream is absent (returning a bounded-timeout error), return promptly
// on context cancellation, and succeed once the worker has created the
// stream.
func TestWaitForJobsStream(t *testing.T) {
	ns, err := natsserver.NewServer(&natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1, // ephemeral free port
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoSigs:    true,
		NoLog:     true,
	})
	if err != nil {
		t.Fatalf("new embedded nats: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded nats did not become ready")
	}
	defer func() { ns.Shutdown(); ns.WaitForShutdown() }()
	url := ns.ClientURL()

	// Stream absent → bounded wait elapses with a "not ready" error.
	if err := waitForJobsStream(context.Background(), url, 300*time.Millisecond); err == nil {
		t.Fatal("expected timeout error while DRIVE_JOBS stream is absent")
	}

	// Stream absent + already-cancelled ctx → returns promptly, not after
	// the (here, long) timeout.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := waitForJobsStream(cancelled, url, time.Minute); err == nil {
		t.Fatal("expected error on cancelled context")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancelled wait took %s, expected prompt return", elapsed)
	}

	// Create the stream the worker would create, then the barrier passes.
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := js.AddStream(&nats.StreamConfig{
		Name:     jobs.StreamName,
		Subjects: []string{"drive.>"},
	}); err != nil {
		t.Fatalf("add stream: %v", err)
	}
	if err := waitForJobsStream(context.Background(), url, 5*time.Second); err != nil {
		t.Fatalf("expected stream ready, got %v", err)
	}
}

// TestResolveBinaryNotFound: with no override and a name that does not
// exist in /app, alongside the test binary, or on $PATH, resolveBinary
// returns a descriptive error rather than a silent miss.
func TestResolveBinaryNotFound(t *testing.T) {
	t.Setenv("ZKDRIVE_WORKER_BIN", "")
	t.Setenv("PATH", t.TempDir())
	name := "zk-drive-nonexistent-binary-xyz"
	_, err := resolveBinary("ZKDRIVE_WORKER_BIN", name)
	if err == nil {
		t.Fatalf("expected not-found error")
	}
	if !strings.Contains(err.Error(), name) {
		t.Fatalf("error %q should name the missing binary", err)
	}
}
