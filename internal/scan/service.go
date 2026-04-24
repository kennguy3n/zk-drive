package scan

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when the requested file / version is missing.
var ErrNotFound = errors.New("scan: not found")

// PresignClient is the minimal storage surface the scanner needs to
// fetch the uploaded bytes. Mirrors preview.PresignClient but kept
// separate to avoid cross-package coupling.
type PresignClient interface {
	GenerateDownloadURL(ctx context.Context, objectKey string, expiry time.Duration) (string, error)
}

// QuarantineNotifier is invoked when clamd reports a threat. It is
// implemented by the notification service in production and by test
// fakes in unit tests. Passing nil disables notifications.
type QuarantineNotifier interface {
	NotifyQuarantine(ctx context.Context, workspaceID, fileID, versionID uuid.UUID, signature string) error
}

// Service scans a single (file_id, version_id) tuple:
//  1. look up the version's object_key + workspace in Postgres
//  2. mark the version scanning
//  3. download the bytes via presigned GET URL
//  4. stream them to clamd over the INSTREAM protocol
//  5. persist the verdict in file_versions and fire notifications on
//     quarantine
type Service struct {
	pool      *pgxpool.Pool
	storage   PresignClient
	notifier  QuarantineNotifier
	httpc     *http.Client
	dialer    func(ctx context.Context, address string) (net.Conn, error)
	address   string
	permissive bool
	now       func() time.Time
}

// NewService wires a ScanService. address may be empty, in which case
// the service runs in permissive mode and never calls clamd (every
// version is marked clean). This keeps local dev / CI green without
// requiring a ClamAV instance.
func NewService(pool *pgxpool.Pool, storage PresignClient, address string) *Service {
	d := &net.Dialer{Timeout: 10 * time.Second}
	return &Service{
		pool:    pool,
		storage: storage,
		httpc:   &http.Client{Timeout: 5 * time.Minute},
		dialer: func(ctx context.Context, address string) (net.Conn, error) {
			return d.DialContext(ctx, "tcp", address)
		},
		address:    strings.TrimSpace(address),
		permissive: strings.TrimSpace(address) == "",
		now:        time.Now,
	}
}

// SetNotifier wires (or rewires) the notifier used for quarantine
// events. Kept separate from NewService so the notification service
// can be constructed after the scan service without circular init.
func (s *Service) SetNotifier(n QuarantineNotifier) { s.notifier = n }

// Scan runs the full scan pipeline for a version. The returned verdict
// always corresponds to the final scan_status persisted on the row.
func (s *Service) Scan(ctx context.Context, fileID, versionID uuid.UUID) (Verdict, error) {
	meta, err := s.loadVersionMeta(ctx, fileID, versionID)
	if err != nil {
		return Verdict{}, err
	}

	if err := s.markStatus(ctx, versionID, StatusScanning, "", nil); err != nil {
		return Verdict{}, err
	}

	body, err := s.downloadObject(ctx, meta.objectKey)
	if err != nil {
		// Leave status as 'scanning' — a transient download error is a
		// retriable failure, not a scan verdict.
		return Verdict{Status: StatusScanning, Detail: err.Error()}, err
	}

	verdict, scanErr := s.scanBytes(ctx, body)
	at := s.now()
	verdict.ScannedAt = at

	if err := s.markStatus(ctx, versionID, verdict.Status, verdict.Detail, &at); err != nil {
		return verdict, err
	}

	// Transient clamd failures must propagate so the JetStream worker
	// Naks the message and NATS redelivers. The row is left in
	// 'pending' with a detail string so operators can audit retries.
	if scanErr != nil {
		return verdict, scanErr
	}

	if verdict.Status == StatusQuarantined && s.notifier != nil {
		// Notification failure is logged by the notifier implementation;
		// we do not fail the scan because of it.
		_ = s.notifier.NotifyQuarantine(ctx, meta.workspaceID, fileID, versionID, verdict.Detail)
	}
	return verdict, nil
}

// scanBytes is split out so tests can inject bodies without a real
// presigned-URL round trip. Returns clean / quarantined based on the
// clamd verdict, or clean when permissive mode is active. A non-nil
// error signals a transient clamd connectivity failure — callers must
// propagate it so the worker retries the job rather than acking it.
func (s *Service) scanBytes(ctx context.Context, body []byte) (Verdict, error) {
	if s.permissive {
		return Verdict{Status: StatusClean, Detail: "permissive mode: scan skipped"}, nil
	}
	sig, err := s.instream(ctx, body)
	if err != nil {
		return Verdict{Status: StatusPending, Detail: fmt.Sprintf("clamd error: %v", err)}, err
	}
	if sig == "" {
		return Verdict{Status: StatusClean}, nil
	}
	return Verdict{Status: StatusQuarantined, Detail: sig}, nil
}

type versionMeta struct {
	workspaceID uuid.UUID
	objectKey   string
}

func (s *Service) loadVersionMeta(ctx context.Context, fileID, versionID uuid.UUID) (versionMeta, error) {
	var m versionMeta
	const q = `
SELECT f.workspace_id, fv.object_key
FROM file_versions fv
JOIN files f ON f.id = fv.file_id
WHERE fv.id = $1 AND fv.file_id = $2`
	if err := s.pool.QueryRow(ctx, q, versionID, fileID).Scan(&m.workspaceID, &m.objectKey); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return m, fmt.Errorf("%w: version %s", ErrNotFound, versionID)
		}
		return m, fmt.Errorf("load version meta: %w", err)
	}
	return m, nil
}

func (s *Service) markStatus(ctx context.Context, versionID uuid.UUID, status, detail string, at *time.Time) error {
	const q = `
UPDATE file_versions
SET scan_status = $2,
    scan_detail = NULLIF($3, ''),
    scanned_at  = COALESCE($4, scanned_at)
WHERE id = $1`
	if _, err := s.pool.Exec(ctx, q, versionID, status, detail, at); err != nil {
		return fmt.Errorf("update scan_status: %w", err)
	}
	return nil
}

func (s *Service) downloadObject(ctx context.Context, key string) ([]byte, error) {
	url, err := s.storage.GenerateDownloadURL(ctx, key, 10*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("presign get: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("get %s: status %d", key, resp.StatusCode)
	}
	// Read MaxScanBytes+1 so we can distinguish an overflow from a
	// file that is exactly MaxScanBytes long. Silently truncating
	// would let malware past the cap slip through as 'clean'; we
	// surface an error instead so the version is left in 'scanning'
	// and the worker Naks for redelivery / alerting.
	buf, err := io.ReadAll(io.LimitReader(resp.Body, MaxScanBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > MaxScanBytes {
		return nil, fmt.Errorf("source %s exceeds %d bytes", key, MaxScanBytes)
	}
	return buf, nil
}

// instream implements the clamd INSTREAM protocol. Frames look like:
//
//	zINSTREAM\0
//	<4-byte big-endian length><chunk bytes>...
//	<4-byte zero>     (EOF sentinel)
//
// clamd then writes a single null-terminated line of the form
// "stream: <SIG> FOUND" or "stream: OK".
func (s *Service) instream(ctx context.Context, body []byte) (string, error) {
	addr := s.address
	if addr == "" {
		addr = DefaultAddress
	}
	conn, err := s.dialer(ctx, addr)
	if err != nil {
		return "", fmt.Errorf("dial clamd: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if _, err := conn.Write([]byte("zINSTREAM\x00")); err != nil {
		return "", err
	}
	// Chunk the body so arbitrarily large uploads don't require us to
	// hold more than one frame in a write buffer. 64 KiB chunks match
	// clamd's recommended StreamMaxLength step.
	const chunkSize = 64 * 1024
	for i := 0; i < len(body); i += chunkSize {
		end := i + chunkSize
		if end > len(body) {
			end = len(body)
		}
		if err := writeChunk(conn, body[i:end]); err != nil {
			return "", err
		}
	}
	if err := writeChunk(conn, nil); err != nil {
		return "", err
	}

	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\x00')
	if err != nil {
		return "", fmt.Errorf("read clamd response: %w", err)
	}
	line = strings.TrimRight(line, "\x00\n ")
	// Expected shapes:
	//   "stream: OK"
	//   "stream: Eicar-Test-Signature FOUND"
	//   "... ERROR"
	if strings.HasSuffix(line, " OK") {
		return "", nil
	}
	if strings.HasSuffix(line, " FOUND") {
		// Strip prefix "stream: " and suffix " FOUND".
		inner := strings.TrimPrefix(line, "stream: ")
		inner = strings.TrimSuffix(inner, " FOUND")
		return strings.TrimSpace(inner), nil
	}
	return "", fmt.Errorf("clamd unexpected response: %q", line)
}

func writeChunk(w io.Writer, data []byte) error {
	var sizeBuf bytes.Buffer
	if err := binary.Write(&sizeBuf, binary.BigEndian, uint32(len(data))); err != nil {
		return err
	}
	if _, err := w.Write(sizeBuf.Bytes()); err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	_, err := w.Write(data)
	return err
}
