// zk-drive worker binary — Phase 2.
//
// The worker hosts JetStream consumers for the drive.* subjects the
// API server publishes to after a successful upload:
//
//   drive.preview.generate — image thumbnail (Go stdlib + x/image)
//   drive.scan.virus       — ClamAV virus scan over INSTREAM
//   drive.search.index     — Postgres FTS index refresh (placeholder)
//
// Each handler resolves its dependencies at startup (Postgres pool,
// zk-object-fabric storage client, optional ClamAV address) and runs
// inline against the enqueued file_id / version_id tuple. Job results
// (preview rows, scan verdicts) are persisted back to Postgres so the
// server can surface them without talking to NATS.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/kennguy3n/zk-drive/internal/config"
	"github.com/kennguy3n/zk-drive/internal/database"
	"github.com/kennguy3n/zk-drive/internal/jobs"
	"github.com/kennguy3n/zk-drive/internal/notification"
	"github.com/kennguy3n/zk-drive/internal/preview"
	"github.com/kennguy3n/zk-drive/internal/scan"
	"github.com/kennguy3n/zk-drive/internal/storage"
)

const (
	streamName  = "DRIVE_JOBS"
	defaultNATS = "nats://localhost:4222"
	ackWait     = 5 * time.Minute
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("worker exited: %v", err)
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

	// Storage client is optional: if the worker is started without
	// S3_ENDPOINT it can only log incoming jobs (same placeholder
	// behaviour as before). In production the server and worker share
	// the same S3 configuration.
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
		log.Printf("worker: storage client wired to %s (bucket=%s)", cfg.S3Endpoint, cfg.S3Bucket)
	} else {
		log.Printf("worker: S3_ENDPOINT unset; preview/scan jobs will be logged only")
	}

	notifSvc := notification.NewService(notification.NewPostgresRepository(pool))

	var previewSvc *preview.Service
	var scanSvc *scan.Service
	if storageClient != nil {
		previewSvc = preview.NewService(pool, storageClient, preview.NewPostgresRepository(pool))
		scanSvc = scan.NewService(pool, storageClient, os.Getenv("CLAMAV_ADDRESS"))
		scanSvc.SetNotifier(notifSvc)
	}

	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = defaultNATS
	}

	nc, err := nats.Connect(natsURL,
		nats.Name("zk-drive-worker"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return fmt.Errorf("connect nats %s: %w", natsURL, err)
	}
	defer nc.Drain() //nolint:errcheck // best-effort drain during shutdown

	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}

	if err := ensureStream(js); err != nil {
		return fmt.Errorf("ensure stream: %w", err)
	}

	subs, err := subscribeAll(ctx, js, previewSvc, scanSvc)
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	defer unsubscribeAll(subs)

	log.Printf("zk-drive worker listening on %s (stream=%s)", natsURL, streamName)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("received signal %s, shutting down", sig)
	return nil
}

// ensureStream creates or updates the DRIVE_JOBS stream that backs
// every drive.* subject. Running AddStream with an existing name
// returns ErrStreamNameAlreadyInUse; we fall through to UpdateStream
// so stream config stays current across deploys without manual
// migration.
func ensureStream(js nats.JetStreamContext) error {
	cfg := &nats.StreamConfig{
		Name:      streamName,
		Subjects:  []string{jobs.SubjectPreview, jobs.SubjectScan, jobs.SubjectIndex},
		Storage:   nats.FileStorage,
		Retention: nats.WorkQueuePolicy,
		MaxAge:    7 * 24 * time.Hour,
	}
	if _, err := js.AddStream(cfg); err != nil {
		if errors.Is(err, nats.ErrStreamNameAlreadyInUse) {
			if _, uerr := js.UpdateStream(cfg); uerr != nil {
				return uerr
			}
			return nil
		}
		return err
	}
	return nil
}

// subscribeAll wires a durable consumer for each subject. Durable
// names let the worker restart without losing checkpoint state.
func subscribeAll(ctx context.Context, js nats.JetStreamContext, previewSvc *preview.Service, scanSvc *scan.Service) ([]*nats.Subscription, error) {
	subjects := []struct {
		subject string
		durable string
		handler nats.MsgHandler
	}{
		{jobs.SubjectPreview, "drive-preview", previewHandler(ctx, previewSvc)},
		{jobs.SubjectScan, "drive-scan", scanHandler(ctx, scanSvc)},
		{jobs.SubjectIndex, "drive-index", indexHandler()},
	}
	var subs []*nats.Subscription
	for _, s := range subjects {
		sub, err := js.Subscribe(s.subject, s.handler,
			nats.Durable(s.durable),
			nats.AckWait(ackWait),
			nats.DeliverAll(),
			nats.ManualAck(),
		)
		if err != nil {
			unsubscribeAll(subs)
			return nil, fmt.Errorf("subscribe %s: %w", s.subject, err)
		}
		subs = append(subs, sub)
	}
	return subs, nil
}

func unsubscribeAll(subs []*nats.Subscription) {
	for _, s := range subs {
		_ = s.Unsubscribe()
	}
}

// previewHandler decodes the FileJob envelope and runs the preview
// service. Unsupported mime types (ErrUnsupportedMime) ack without
// error because the file is simply not previewable; every other
// failure Nak's so NATS redelivers on the next AckWait cycle.
func previewHandler(ctx context.Context, svc *preview.Service) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var job jobs.FileJob
		if err := json.Unmarshal(msg.Data, &job); err != nil {
			log.Printf("worker: malformed preview payload: %v", err)
			_ = msg.Term()
			return
		}
		if svc == nil {
			log.Printf("worker: preview skipped (no storage client): file=%s version=%s", job.FileID, job.VersionID)
			_ = msg.Ack()
			return
		}
		jobCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		p, err := svc.Generate(jobCtx, job.FileID, job.VersionID)
		if err != nil {
			if errors.Is(err, preview.ErrUnsupportedMime) {
				log.Printf("worker: preview unsupported mime: file=%s version=%s", job.FileID, job.VersionID)
				_ = msg.Ack()
				return
			}
			log.Printf("worker: preview failed file=%s version=%s: %v", job.FileID, job.VersionID, err)
			_ = msg.Nak()
			return
		}
		log.Printf("worker: preview ok file=%s version=%s key=%s", job.FileID, job.VersionID, p.ObjectKey)
		_ = msg.Ack()
	}
}

// scanHandler decodes the FileJob envelope and runs the scan service.
// All verdicts (clean / quarantined / pending) are acked so the
// message doesn't loop; the final status is persisted to file_versions
// so operators can audit results via SQL.
func scanHandler(ctx context.Context, svc *scan.Service) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var job jobs.FileJob
		if err := json.Unmarshal(msg.Data, &job); err != nil {
			log.Printf("worker: malformed scan payload: %v", err)
			_ = msg.Term()
			return
		}
		if svc == nil {
			log.Printf("worker: scan skipped (no storage client): file=%s version=%s", job.FileID, job.VersionID)
			_ = msg.Ack()
			return
		}
		jobCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		v, err := svc.Scan(jobCtx, job.FileID, job.VersionID)
		if err != nil {
			log.Printf("worker: scan error file=%s version=%s: %v", job.FileID, job.VersionID, err)
			_ = msg.Nak()
			return
		}
		log.Printf("worker: scan %s file=%s version=%s detail=%q", v.Status, job.FileID, job.VersionID, v.Detail)
		_ = msg.Ack()
	}
}

// indexHandler remains a logging placeholder; search indexing lives
// inline in the confirm-upload handler today via the Postgres FTS
// trigger migration, so this consumer only drains the subject so
// queues don't back up.
func indexHandler() nats.MsgHandler {
	return func(msg *nats.Msg) {
		var job jobs.FileJob
		if err := json.Unmarshal(msg.Data, &job); err != nil {
			log.Printf("worker: malformed index payload: %v", err)
			_ = msg.Term()
			return
		}
		log.Printf("worker: index job acked file=%s version=%s", job.FileID, job.VersionID)
		_ = msg.Ack()
	}
}
