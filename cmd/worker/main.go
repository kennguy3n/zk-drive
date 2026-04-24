// zk-drive worker binary — Phase 2 skeleton.
//
// The worker hosts JetStream consumers for the drive.* subjects the
// API server publishes to after a successful upload:
//
//   drive.preview.generate — LibreOffice / ImageMagick preview build
//   drive.scan.virus       — ClamAV virus scan
//   drive.search.index     — Postgres FTS index refresh
//
// For now the consumer handlers are placeholders that log the job and
// ack the message so the full pipeline (server → NATS → worker ack)
// can be exercised end to end. The actual preview / scan / index logic
// lands in a later Phase-2 sprint and is tracked in docs/PROGRESS.md.
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

	"github.com/kennguy3n/zk-drive/internal/jobs"
)

const (
	streamName  = "DRIVE_JOBS"
	defaultNATS = "nats://localhost:4222"
	ackWait     = 30 * time.Second
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("worker exited: %v", err)
	}
}

func run() error {
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	subs, err := subscribeAll(ctx, js)
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
// Handlers log the job, ack, and return — actual pipeline work lands
// later in Phase 2.
func subscribeAll(_ context.Context, js nats.JetStreamContext) ([]*nats.Subscription, error) {
	subjects := []struct {
		subject string
		durable string
		label   string
	}{
		{jobs.SubjectPreview, "drive-preview", "preview"},
		{jobs.SubjectScan, "drive-scan", "scan"},
		{jobs.SubjectIndex, "drive-index", "index"},
	}
	var subs []*nats.Subscription
	for _, s := range subjects {
		sub, err := js.Subscribe(s.subject, handleJob(s.label),
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

// handleJob returns a nats.MsgHandler that decodes the FileJob
// envelope, logs it, and acks. Malformed payloads are terminated
// rather than redelivered so a single poison pill can't wedge the
// consumer.
func handleJob(label string) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var job jobs.FileJob
		if err := json.Unmarshal(msg.Data, &job); err != nil {
			log.Printf("worker: malformed %s payload: %v", label, err)
			_ = msg.Term()
			return
		}
		log.Printf("worker: %s job received file_id=%s version_id=%s", label, job.FileID, job.VersionID)
		if err := msg.Ack(); err != nil {
			log.Printf("worker: ack %s job failed: %v", label, err)
		}
	}
}
