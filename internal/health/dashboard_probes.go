package health

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/zk-drive/internal/ai"
	"github.com/kennguy3n/zk-drive/internal/heartbeat"
	"github.com/kennguy3n/zk-drive/internal/storage"
)

// ---------------------------------------------------------------------------
// Postgres
// ---------------------------------------------------------------------------

// PostgresDashboardProbe reports pool utilisation and, when read
// replicas are streaming from this primary, the worst replica replay
// lag. Replica detection is config-free: it reads pg_stat_replication,
// which lists exactly the standbys currently connected to this node,
// so a deployment that adds a read replica starts reporting lag with
// no env-var change.
type PostgresDashboardProbe struct {
	pool *pgxpool.Pool
}

// NewPostgresDashboardProbe wraps a pgx pool.
func NewPostgresDashboardProbe(pool *pgxpool.Pool) *PostgresDashboardProbe {
	return &PostgresDashboardProbe{pool: pool}
}

// SubsystemName implements DashboardProbe.
func (p *PostgresDashboardProbe) SubsystemName() string { return "postgres" }

// Probe implements DashboardProbe.
func (p *PostgresDashboardProbe) Probe(ctx context.Context) Subsystem {
	s := Subsystem{Name: p.SubsystemName()}
	if p == nil || p.pool == nil {
		s.Status = ColorRed
		s.Error = "postgres pool not initialised"
		return s
	}
	if err := p.pool.Ping(ctx); err != nil {
		s.Status = ColorRed
		s.Error = "ping failed"
		return s
	}

	stat := p.pool.Stat()
	detail := map[string]any{
		"total_conns":    stat.TotalConns(),
		"acquired_conns": stat.AcquiredConns(),
		"idle_conns":     stat.IdleConns(),
		"max_conns":      stat.MaxConns(),
	}
	s.Status = ColorGreen

	// Pool-saturation early warning: when nearly every connection is
	// checked out, new queries start queueing on AcquireConn. Surface
	// that as yellow before it becomes a latency incident.
	if stat.MaxConns() > 0 && float64(stat.AcquiredConns())/float64(stat.MaxConns()) >= poolSaturationThreshold {
		s.Status = ColorYellow
		s.Error = "connection pool near saturation"
	}

	// Replication lag, only when standbys are connected.
	if replicas, maxLag, err := p.replicationLag(ctx); err == nil && replicas > 0 {
		detail["read_replicas"] = replicas
		detail["max_replica_lag_seconds"] = maxLag.Seconds()
		if maxLag >= replicaLagRedThreshold {
			s.Status = worsen(s.Status, ColorRed)
			s.Error = "read replica replication lag critical"
		} else if maxLag >= replicaLagYellowThreshold {
			s.Status = worsen(s.Status, ColorYellow)
			if s.Error == "" {
				s.Error = "read replica replication lag elevated"
			}
		}
	}

	s.Detail = detail
	return s
}

// poolSaturationThreshold is the acquired/max ratio at or above which
// the pool is flagged yellow.
const poolSaturationThreshold = 0.9

// replicaLagYellowThreshold / replicaLagRedThreshold bound acceptable
// read-replica replay lag. A few seconds is normal under write
// bursts; tens of seconds means readers are serving materially stale
// data.
const (
	replicaLagYellowThreshold = 10 * time.Second
	replicaLagRedThreshold    = 60 * time.Second
)

// replicationLag returns the number of connected standbys and the
// worst replay lag among them. lag is computed from the primary's
// perspective as now() - reply-side replay timestamp via the
// replay_lag interval column, falling back to 0 when the standby has
// not yet reported one.
func (p *PostgresDashboardProbe) replicationLag(ctx context.Context) (int, time.Duration, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT COALESCE(EXTRACT(EPOCH FROM replay_lag), 0)::float8
		FROM pg_stat_replication
	`)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()

	count := 0
	var maxLag time.Duration
	for rows.Next() {
		var lagSeconds float64
		if err := rows.Scan(&lagSeconds); err != nil {
			return 0, 0, err
		}
		count++
		if d := time.Duration(lagSeconds * float64(time.Second)); d > maxLag {
			maxLag = d
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	return count, maxLag, nil
}

// ---------------------------------------------------------------------------
// Redis
// ---------------------------------------------------------------------------

// RedisDashboardProbe reports connectivity and memory usage. A nil
// client means Redis is intentionally unconfigured (single-node
// in-memory posture), reported as ColorUnknown so it never drags the
// overall status down.
type RedisDashboardProbe struct {
	client *redis.Client
}

// NewRedisDashboardProbe wraps a *redis.Client; pass nil for "not
// configured".
func NewRedisDashboardProbe(client *redis.Client) *RedisDashboardProbe {
	return &RedisDashboardProbe{client: client}
}

// SubsystemName implements DashboardProbe.
func (r *RedisDashboardProbe) SubsystemName() string { return "redis" }

// Probe implements DashboardProbe.
func (r *RedisDashboardProbe) Probe(ctx context.Context) Subsystem {
	s := Subsystem{Name: r.SubsystemName()}
	if r == nil || r.client == nil {
		s.Status = ColorUnknown
		s.Detail = map[string]any{"configured": false}
		return s
	}
	if err := r.client.Ping(ctx).Err(); err != nil {
		s.Status = ColorRed
		s.Error = "ping failed"
		return s
	}
	s.Status = ColorGreen
	s.Detail = map[string]any{"configured": true}

	info, err := r.client.Info(ctx, "memory").Result()
	if err == nil {
		mem := parseRedisMemory(info)
		for k, v := range mem {
			s.Detail[k] = v
		}
	}
	return s
}

// parseRedisMemory extracts the fields the dashboard surfaces from a
// Redis `INFO memory` payload. Unknown / missing fields are simply
// omitted. Exposed values:
//   - used_memory_bytes (int)
//   - used_memory_human (string)
//   - maxmemory_bytes (int, 0 = unbounded)
func parseRedisMemory(info string) map[string]any {
	out := map[string]any{}
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch key {
		case "used_memory":
			if n, err := strconv.ParseInt(val, 10, 64); err == nil {
				out["used_memory_bytes"] = n
			}
		case "used_memory_human":
			out["used_memory_human"] = val
		case "maxmemory":
			if n, err := strconv.ParseInt(val, 10, 64); err == nil {
				out["maxmemory_bytes"] = n
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// NATS / JetStream
// ---------------------------------------------------------------------------

// NATSDashboardProbe reports connection state and the JetStream
// message depth per subject for the jobs stream. A nil connection
// means NATS is unconfigured (ColorUnknown).
type NATSDashboardProbe struct {
	conn       *nats.Conn
	js         nats.JetStreamContext
	streamName string
}

// NewNATSDashboardProbe wraps the server's NATS connection, its
// JetStream context, and the jobs stream name. Pass a nil conn for
// "not configured".
func NewNATSDashboardProbe(conn *nats.Conn, js nats.JetStreamContext, streamName string) *NATSDashboardProbe {
	return &NATSDashboardProbe{conn: conn, js: js, streamName: streamName}
}

// SubsystemName implements DashboardProbe.
func (n *NATSDashboardProbe) SubsystemName() string { return "nats" }

// Probe implements DashboardProbe.
func (n *NATSDashboardProbe) Probe(ctx context.Context) Subsystem {
	s := Subsystem{Name: n.SubsystemName()}
	if n == nil || n.conn == nil {
		s.Status = ColorUnknown
		s.Detail = map[string]any{"configured": false}
		return s
	}
	switch n.conn.Status() {
	case nats.CONNECTED:
		s.Status = ColorGreen
	case nats.RECONNECTING:
		// Reachable-ish but actively reconnecting: surface as degraded
		// rather than down so a brief blip doesn't flash red.
		s.Status = ColorYellow
		s.Error = "reconnecting"
	default:
		s.Status = ColorRed
		s.Error = "disconnected"
		return s
	}

	detail := map[string]any{"configured": true, "status": n.conn.Status().String()}
	if n.js != nil && n.streamName != "" {
		// Passing a *StreamInfoRequest with a SubjectsFilter asks the
		// server to populate State.Subjects (the per-subject message
		// counts); StreamInfoRequest satisfies nats.JSOpt directly.
		// nats.Context binds the request to the dashboard's per-probe
		// deadline so a wedged JetStream server can't hang the probe
		// past the dashboard timeout — without it StreamInfo would only
		// honour the client's own (longer) request timeout.
		if info, err := n.js.StreamInfo(n.streamName, &nats.StreamInfoRequest{SubjectsFilter: ">"}, nats.Context(ctx)); err == nil && info != nil {
			detail["stream"] = n.streamName
			detail["messages"] = info.State.Msgs
			if len(info.State.Subjects) > 0 {
				// Convert to a stable, JSON-friendly shape.
				subjects := make(map[string]uint64, len(info.State.Subjects))
				for subj, cnt := range info.State.Subjects {
					subjects[subj] = cnt
				}
				detail["subject_depth"] = subjects
			}
		}
	}
	s.Detail = detail
	return s
}

// ---------------------------------------------------------------------------
// ClamAV
// ---------------------------------------------------------------------------

// ClamAVDashboardProbe reports virus-scanner connectivity and its
// signature-database date by issuing the clamd VERSION command. An
// empty address means ClamAV is unconfigured (ColorUnknown) — virus
// scanning is an optional service.
type ClamAVDashboardProbe struct {
	address string
}

// NewClamAVDashboardProbe wraps the CLAMAV_ADDRESS. Empty == not
// configured.
func NewClamAVDashboardProbe(address string) *ClamAVDashboardProbe {
	return &ClamAVDashboardProbe{address: strings.TrimSpace(address)}
}

// SubsystemName implements DashboardProbe.
func (c *ClamAVDashboardProbe) SubsystemName() string { return "clamav" }

// Probe implements DashboardProbe.
func (c *ClamAVDashboardProbe) Probe(ctx context.Context) Subsystem {
	s := Subsystem{Name: c.SubsystemName()}
	if c == nil || c.address == "" {
		s.Status = ColorUnknown
		s.Detail = map[string]any{"configured": false}
		return s
	}
	version, err := clamavVersion(ctx, c.address)
	if err != nil {
		s.Status = ColorRed
		s.Error = "unreachable"
		return s
	}
	s.Status = ColorGreen
	detail := map[string]any{"configured": true, "version": version.Engine}
	if version.DBVersion != "" {
		detail["definitions_version"] = version.DBVersion
	}
	if !version.DBDate.IsZero() {
		detail["definitions_date"] = version.DBDate.Format(time.RFC3339)
		// Stale-definitions warning: ClamAV freshclam normally updates
		// several times a day, so signatures older than a week mean
		// freshclam is broken and the scanner is missing recent threats.
		if time.Since(version.DBDate) >= clamavStaleDefsThreshold {
			s.Status = ColorYellow
			s.Error = "virus definitions stale"
		}
	}
	s.Detail = detail
	return s
}

// clamavStaleDefsThreshold is how old the signature database may be
// before the dashboard flags it yellow.
const clamavStaleDefsThreshold = 7 * 24 * time.Hour

// clamavVersionInfo is the parsed clamd VERSION response.
type clamavVersionInfo struct {
	Engine    string    // e.g. "ClamAV 0.103.11"
	DBVersion string    // e.g. "27000"
	DBDate    time.Time // signature DB build date
}

// clamavVersion dials clamd and issues the newline-style VERSION
// command, returning the parsed response. The whole exchange is bound
// by ctx's deadline.
func clamavVersion(ctx context.Context, address string) (clamavVersionInfo, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return clamavVersionInfo{}, fmt.Errorf("dial clamd: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := conn.Write([]byte("nVERSION\n")); err != nil {
		return clamavVersionInfo{}, fmt.Errorf("write VERSION: %w", err)
	}
	buf := make([]byte, 256)
	nRead, err := conn.Read(buf)
	if err != nil {
		return clamavVersionInfo{}, fmt.Errorf("read VERSION: %w", err)
	}
	return parseClamAVVersion(string(buf[:nRead])), nil
}

// parseClamAVVersion parses a clamd VERSION line of the form
// "ClamAV 0.103.11/27000/Thu Apr 18 09:00:00 2024". The engine string
// is always returned (the whole line when no "/" is present); the DB
// version and date are best-effort.
func parseClamAVVersion(raw string) clamavVersionInfo {
	raw = strings.TrimSpace(raw)
	parts := strings.Split(raw, "/")
	info := clamavVersionInfo{Engine: strings.TrimSpace(parts[0])}
	if len(parts) >= 2 {
		info.DBVersion = strings.TrimSpace(parts[1])
	}
	if len(parts) >= 3 {
		// clamd emits the C-locale ctime format.
		if t, err := time.Parse("Mon Jan _2 15:04:05 2006", strings.TrimSpace(parts[2])); err == nil {
			info.DBDate = t
		}
	}
	return info
}

// ---------------------------------------------------------------------------
// ONLYOFFICE
// ---------------------------------------------------------------------------

// OnlyOfficeDashboardProbe reports Document Server reachability via
// its /healthcheck endpoint (which returns the literal "true" when
// healthy). An empty URL means collaborative editing is unconfigured
// (ColorUnknown).
type OnlyOfficeDashboardProbe struct {
	baseURL string
	client  *http.Client
}

// NewOnlyOfficeDashboardProbe wraps the ONLYOFFICE_URL and an HTTP
// client. A nil client falls back to a default with a conservative
// timeout. Empty URL == not configured.
func NewOnlyOfficeDashboardProbe(baseURL string, client *http.Client) *OnlyOfficeDashboardProbe {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &OnlyOfficeDashboardProbe{baseURL: strings.TrimSpace(baseURL), client: client}
}

// SubsystemName implements DashboardProbe.
func (o *OnlyOfficeDashboardProbe) SubsystemName() string { return "onlyoffice" }

// Probe implements DashboardProbe.
func (o *OnlyOfficeDashboardProbe) Probe(ctx context.Context) Subsystem {
	s := Subsystem{Name: o.SubsystemName()}
	if o == nil || o.baseURL == "" {
		s.Status = ColorUnknown
		s.Detail = map[string]any{"configured": false}
		return s
	}
	url := strings.TrimRight(o.baseURL, "/") + "/healthcheck"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		s.Status = ColorRed
		s.Error = "invalid url"
		return s
	}
	resp, err := o.client.Do(req)
	if err != nil {
		s.Status = ColorRed
		s.Error = "unreachable"
		return s
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		s.Status = ColorRed
		s.Error = fmt.Sprintf("healthcheck status %d", resp.StatusCode)
		return s
	}
	s.Status = ColorGreen
	s.Detail = map[string]any{"configured": true}
	return s
}

// ---------------------------------------------------------------------------
// Storage / Fabric
// ---------------------------------------------------------------------------

// storageDashProbe is the contract the storage dashboard probe needs:
// a reachability check plus the rolling error-rate summary. Declared
// as an interface so tests can supply a fake without the AWS SDK.
type storageDashProbe interface {
	HealthCheck(ctx context.Context) error
	RecentErrorStats() storage.OpStats
}

// StorageDashboardProbe reports gateway reachability (HeadBucket) and
// the recent server-side operation error rate.
type StorageDashboardProbe struct {
	probe storageDashProbe
}

// NewStorageDashboardProbe wraps the storage client. It deliberately
// takes the concrete *storage.Client (not the interface) so a nil
// client — the common path when S3 is unconfigured — normalises to a
// nil probe field and the "not configured" branch fires, avoiding the
// typed-nil-interface trap (same rationale as NewStorageChecker).
func NewStorageDashboardProbe(client *storage.Client) *StorageDashboardProbe {
	if client == nil {
		return &StorageDashboardProbe{probe: nil}
	}
	return &StorageDashboardProbe{probe: client}
}

// newStorageDashboardProbeWithProbe is a test seam that accepts the
// interface directly (so a fake can stand in for *storage.Client).
func newStorageDashboardProbeWithProbe(probe storageDashProbe) *StorageDashboardProbe {
	return &StorageDashboardProbe{probe: probe}
}

// SubsystemName implements DashboardProbe.
func (s *StorageDashboardProbe) SubsystemName() string { return "storage" }

// Probe implements DashboardProbe.
func (s *StorageDashboardProbe) Probe(ctx context.Context) Subsystem {
	out := Subsystem{Name: s.SubsystemName()}
	if s == nil || s.probe == nil {
		out.Status = ColorUnknown
		out.Detail = map[string]any{"configured": false}
		return out
	}
	stats := s.probe.RecentErrorStats()
	detail := map[string]any{
		"configured":        true,
		"recent_operations": stats.Total,
		"recent_errors":     stats.Errors,
		"error_rate":        stats.ErrorRate(),
		"error_window_secs": stats.Window.Seconds(),
	}
	if err := s.probe.HealthCheck(ctx); err != nil {
		out.Status = ColorRed
		out.Error = "gateway unreachable"
		out.Detail = detail
		return out
	}
	out.Status = ColorGreen
	// Elevated-but-non-zero error rate over the window is a degraded
	// signal even when the live HeadBucket succeeds (intermittent
	// failures).
	if stats.Total > 0 && stats.ErrorRate() >= storageErrorRateThreshold {
		out.Status = ColorYellow
		out.Error = "elevated storage error rate"
	}
	out.Detail = detail
	return out
}

// storageErrorRateThreshold is the windowed error fraction at or above
// which storage is flagged yellow despite a healthy live probe.
const storageErrorRateThreshold = 0.1

// ---------------------------------------------------------------------------
// Worker heartbeats
// ---------------------------------------------------------------------------

// heartbeatReader is the contract the worker probe needs from the
// heartbeat store; an interface so tests can supply canned data.
type heartbeatReader interface {
	List(ctx context.Context) ([]heartbeat.WorkerHealth, error)
}

// WorkerDashboardProbe reports the liveness of each worker type from
// the worker_heartbeats table, applying the staleness policy from the
// heartbeat package.
type WorkerDashboardProbe struct {
	reader heartbeatReader
	now    func() time.Time // injectable for tests
}

// NewWorkerDashboardProbe wraps a heartbeat reader.
func NewWorkerDashboardProbe(reader heartbeatReader) *WorkerDashboardProbe {
	return &WorkerDashboardProbe{reader: reader, now: time.Now}
}

// SubsystemName implements DashboardProbe.
func (w *WorkerDashboardProbe) SubsystemName() string { return "worker" }

// Probe implements DashboardProbe.
func (w *WorkerDashboardProbe) Probe(ctx context.Context) Subsystem {
	s := Subsystem{Name: w.SubsystemName()}
	if w == nil || w.reader == nil {
		s.Status = ColorUnknown
		s.Detail = map[string]any{"configured": false}
		return s
	}
	healths, err := w.reader.List(ctx)
	if err != nil {
		s.Status = ColorRed
		s.Error = "heartbeat read failed"
		return s
	}
	now := w.now()
	workers := make([]map[string]any, 0, len(healths))
	overall := ColorUnknown
	for _, h := range healths {
		color := workerColor(h, now)
		overall = worsen(overall, color)
		workers = append(workers, map[string]any{
			"worker_type":  h.WorkerType,
			"instances":    h.Instances,
			"last_seen_at": h.LastSeenAt.UTC().Format(time.RFC3339),
			"age_seconds":  now.Sub(h.LastSeenAt).Seconds(),
			"status":       color,
		})
	}
	sort.Slice(workers, func(i, j int) bool {
		return workers[i]["worker_type"].(string) < workers[j]["worker_type"].(string)
	})

	if len(healths) == 0 {
		// No worker has ever reported. On an SME single-node box this
		// is expected only before the worker first boots; surface it
		// as yellow ("no workers reporting") rather than red so a
		// momentary race at first boot isn't alarming, but the
		// operator still sees that nothing is processing jobs.
		s.Status = ColorYellow
		s.Error = "no workers reporting"
		s.Detail = map[string]any{"workers": []any{}}
		return s
	}
	s.Status = overall
	s.Detail = map[string]any{"workers": workers}
	return s
}

// workerColor maps one worker type's aggregated health + age onto a
// traffic-light colour using the heartbeat staleness thresholds.
func workerColor(h heartbeat.WorkerHealth, now time.Time) Color {
	age := now.Sub(h.LastSeenAt)
	switch {
	case age >= heartbeat.DeadAfter:
		return ColorRed
	case age >= heartbeat.StaleAfter:
		return ColorYellow
	case h.Status == heartbeat.StatusDegraded:
		return ColorYellow
	default:
		return ColorGreen
	}
}

// ---------------------------------------------------------------------------
// AI local LLM (Ollama)
// ---------------------------------------------------------------------------

// aiLLMProbe is the contract the AI-LLM dashboard probe needs from the
// Ollama client; an interface so tests can supply a fake without a live
// daemon.
type aiLLMProbe interface {
	Model() string
	Health(ctx context.Context) error
}

// AILLMDashboardProbe reports whether the optional local LLM (Ollama)
// is wired and reachable. The LLM is the one genuinely config-gated AI
// capability (OLLAMA_URL): when it is unset the AI features fall back
// to the deterministic rule-based path, so a disabled LLM is reported
// as ColorUnknown ("not configured") rather than an error. When it is
// wired, the probe issues a cheap liveness check (GET /api/tags, no
// inference) so a misconfigured or down daemon surfaces as red instead
// of silently degrading every summary / tag-suggestion / query-
// expansion request to the fallback.
type AILLMDashboardProbe struct {
	llm aiLLMProbe
}

// NewAILLMDashboardProbe wraps the Ollama client. It deliberately takes
// the concrete *ai.OllamaClient (not the interface) so a nil client —
// the common path when OLLAMA_URL is unset — normalises to a nil probe
// field and the "not configured" branch fires, avoiding the typed-nil-
// interface trap (same rationale as NewStorageDashboardProbe).
func NewAILLMDashboardProbe(client *ai.OllamaClient) *AILLMDashboardProbe {
	if client == nil {
		return &AILLMDashboardProbe{llm: nil}
	}
	return &AILLMDashboardProbe{llm: client}
}

// newAILLMDashboardProbeWithProbe is a test seam that accepts the
// interface directly (so a fake can stand in for *ai.OllamaClient).
func newAILLMDashboardProbeWithProbe(p aiLLMProbe) *AILLMDashboardProbe {
	return &AILLMDashboardProbe{llm: p}
}

// SubsystemName implements DashboardProbe.
func (a *AILLMDashboardProbe) SubsystemName() string { return "ai_llm" }

// Probe implements DashboardProbe.
func (a *AILLMDashboardProbe) Probe(ctx context.Context) Subsystem {
	out := Subsystem{Name: a.SubsystemName()}
	if a == nil || a.llm == nil {
		out.Status = ColorUnknown
		out.Detail = map[string]any{"configured": false, "mode": "rule-based fallback"}
		return out
	}
	detail := map[string]any{"configured": true, "model": a.llm.Model()}
	if err := a.llm.Health(ctx); err != nil {
		out.Status = ColorRed
		out.Error = "llm daemon unreachable"
		out.Detail = detail
		return out
	}
	out.Status = ColorGreen
	out.Detail = detail
	return out
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// worsen returns the worse (higher-severity) of two colours.
func worsen(a, b Color) Color {
	if b.severity() > a.severity() {
		return b
	}
	return a
}
