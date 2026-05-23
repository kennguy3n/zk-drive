// zk-drive audit-log restore CLI — Phase 5 / WS-23.
//
// Reads archived audit_log rows from the S3 cold tier for a single
// workspace and emits them as newline-delimited JSON to stdout (or
// a file). The intended use case is incident investigation and
// compliance "produce all admin actions in workspace X between two
// dates" requests.
//
// Usage:
//
//	audit-restore --workspace <uuid> --from <ts> --to <ts> [--output FILE]
//
// Where:
//
//	--workspace   UUID of the workspace whose archive should be read.
//	--from / --to RFC3339 timestamps bounding the inclusive range.
//	--output      Destination file path. Defaults to stdout.
//
// The tool:
//
//  1. Lists every cold-tier object under
//     {archive_prefix}{workspace}/ via the S3 ListObjects API.
//  2. Filters objects to those whose key matches a year-month that
//     could overlap [from, to].
//  3. GET each matching object, gunzip, parse JSONL.
//  4. Filter entries to those whose created_at falls in [from, to].
//  5. Deduplicate by entry.ID (cold tier may carry duplicate
//     objects from idempotency-driven re-uploads; the restore
//     output must contain each audit row exactly once).
//  6. Emit the surviving entries as JSONL.
//
// The tool does NOT modify the S3 archive or the database — it is
// strictly read-only. Operators can run it safely against any
// historical period without risk of mutating retention state.
//
// Exit code is non-zero on configuration error, S3 fetch error,
// or empty result when the operator explicitly requested rows but
// none were found (--require-non-empty flag — TODO for v2).
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/audit"
	"github.com/kennguy3n/zk-drive/internal/config"
	"github.com/kennguy3n/zk-drive/internal/logging"
	"github.com/kennguy3n/zk-drive/internal/storage"
	"github.com/kennguy3n/zk-drive/internal/version"
)

func main() {
	if err := run(); err != nil {
		slog.Error("audit-restore failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	logging.Init("audit-restore")
	slog.Info("zk-drive audit-restore", "version", version.Version)

	var (
		workspaceFlag string
		fromFlag      string
		toFlag        string
		outputFlag    string
	)
	flag.StringVar(&workspaceFlag, "workspace", "", "Workspace UUID to restore audit history for (required)")
	flag.StringVar(&fromFlag, "from", "", "Inclusive lower bound on created_at as RFC3339 (required)")
	flag.StringVar(&toFlag, "to", "", "Inclusive upper bound on created_at as RFC3339 (required)")
	flag.StringVar(&outputFlag, "output", "", "Output file path (default: stdout)")
	flag.Parse()

	workspaceID, err := uuid.Parse(strings.TrimSpace(workspaceFlag))
	if err != nil {
		return fmt.Errorf("--workspace required and must be a UUID: %w", err)
	}
	from, err := time.Parse(time.RFC3339, strings.TrimSpace(fromFlag))
	if err != nil {
		return fmt.Errorf("--from required and must be RFC3339: %w", err)
	}
	to, err := time.Parse(time.RFC3339, strings.TrimSpace(toFlag))
	if err != nil {
		return fmt.Errorf("--to required and must be RFC3339: %w", err)
	}
	if !to.After(from) {
		return errors.New("--to must be strictly after --from")
	}

	// LoadStorageOnly skips the DATABASE_URL / JWT_SECRET checks
	// that the server's Load enforces — audit-restore is strictly
	// read-only against S3 and an on-call engineer responding to a
	// compliance request shouldn't need Postgres credentials just
	// to stream archived JSONL out. The S3 group is still validated
	// (S3_ENDPOINT requires bucket + access key + secret key).
	// See WS-23 PR #68 Devin Review finding
	// ANALYSIS_pr-review-job-ad89da4c3a1449c5b914d6045dc4ffb8_0001.
	cfg, err := config.LoadStorageOnly()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	archiveBucket := cfg.S3Bucket
	if cfg.AuditArchiveBucket != "" {
		archiveBucket = cfg.AuditArchiveBucket
	}
	storageClient, err := storage.NewClient(storage.Config{
		Endpoint:  cfg.S3Endpoint,
		Bucket:    archiveBucket,
		AccessKey: cfg.S3AccessKey,
		SecretKey: cfg.S3SecretKey,
	})
	if err != nil {
		return fmt.Errorf("storage client: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Prefix scope: every archive object for this workspace is
	// directly underneath {archive_prefix}{workspace_id}/. Keep
	// the LIST narrow so the operator's S3 cost is bounded by
	// the workspace's own archive history rather than the full
	// bucket.
	prefix := fmt.Sprintf("%s%s/", cfg.AuditArchivePrefix, workspaceID.String())
	keys, err := listOverlappingObjects(ctx, storageClient, prefix, from, to)
	if err != nil {
		return fmt.Errorf("list archive objects: %w", err)
	}
	slog.Info("audit-restore discovered archive objects",
		"workspace_id", workspaceID,
		"prefix", prefix,
		"matching_objects", len(keys),
	)

	entries, err := fetchAndDedupe(ctx, storageClient, keys, from, to)
	if err != nil {
		return err
	}
	slog.Info("audit-restore fetched entries", "count", len(entries))

	out := os.Stdout
	if strings.TrimSpace(outputFlag) != "" {
		f, err := os.Create(outputFlag)
		if err != nil {
			return fmt.Errorf("open output: %w", err)
		}
		defer func() {
			if cerr := f.Close(); cerr != nil {
				slog.Warn("close output file", "err", cerr)
			}
		}()
		out = f
	}

	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return fmt.Errorf("encode entry %s: %w", e.ID, err)
		}
	}
	return nil
}

// listOverlappingObjects enumerates archive objects under prefix
// and returns those whose key embeds a year-month in [from, to].
// The S3 key shape is
//
//	{prefix}{workspace_id}/{YYYY-MM}/{run_id}.jsonl.gz
//
// so the second path segment (relative to prefix) is the year-month
// we filter on.
func listOverlappingObjects(ctx context.Context, st *storage.Client, prefix string, from, to time.Time) ([]string, error) {
	fromMonth := from.UTC().Format("2006-01")
	toMonth := to.UTC().Format("2006-01")
	var keys []string
	err := st.ListObjects(ctx, prefix, func(key string, _ int64) error {
		// key looks like prefix + "YYYY-MM/{runid}.jsonl.gz".
		rest := strings.TrimPrefix(key, prefix)
		idx := strings.IndexByte(rest, '/')
		if idx <= 0 {
			return nil
		}
		ym := rest[:idx]
		if len(ym) != 7 {
			return nil
		}
		if ym < fromMonth || ym > toMonth {
			return nil
		}
		keys = append(keys, key)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(keys)
	return keys, nil
}

// fetchAndDedupe reads each archive object, gunzips, JSON-decodes
// entries, filters to [from, to], and deduplicates by id. The
// dedupe is critical: the cold tier may contain multiple objects
// covering the same (workspace, month) range when a previous
// archiver run crashed between S3 upload and DELETE — the rows
// stayed in the hot tier and got re-uploaded on the next run.
// Each row's id is stable across re-uploads so dedupe by id
// produces the canonical set.
//
// Entries are returned sorted by created_at ASC (chronological)
// so a JSONL dump reads top-to-bottom as the events happened.
func fetchAndDedupe(ctx context.Context, st *storage.Client, keys []string, from, to time.Time) ([]*audit.Entry, error) {
	seen := make(map[uuid.UUID]bool)
	var out []*audit.Entry
	for _, key := range keys {
		body, err := st.GetObject(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("get %s: %w", key, err)
		}
		gz, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("gunzip %s: %w", key, err)
		}
		raw, err := io.ReadAll(gz)
		_ = gz.Close()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", key, err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		for {
			e := &audit.Entry{}
			if err := dec.Decode(e); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return nil, fmt.Errorf("decode %s: %w", key, err)
			}
			if e.CreatedAt.Before(from) || e.CreatedAt.After(to) {
				continue
			}
			if seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}
