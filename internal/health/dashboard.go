package health

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Color is a traffic-light health signal for one subsystem (or the
// dashboard as a whole). The four values map directly onto the admin
// UI's coloured pills.
type Color string

const (
	// ColorGreen — the subsystem is configured and healthy.
	ColorGreen Color = "green"
	// ColorYellow — configured and reachable but degraded (e.g. a
	// dependency auto-disabled, a worker type briefly stale, an
	// elevated-but-non-zero error rate).
	ColorYellow Color = "yellow"
	// ColorRed — configured but unhealthy (unreachable / erroring).
	ColorRed Color = "red"
	// ColorUnknown — the subsystem is not configured in this
	// deployment (e.g. Redis unset on a single-node box). Rendered
	// grey and explicitly EXCLUDED from the overall roll-up: a NoOps
	// SME deployment that runs without Redis must still show an
	// overall-green dashboard.
	ColorUnknown Color = "unknown"
)

// severity orders colours for the overall roll-up. Higher is worse.
// ColorUnknown sits below green so it never drags the overall status
// down — an unconfigured optional dependency is not a fault.
func (c Color) severity() int {
	switch c {
	case ColorRed:
		return 3
	case ColorYellow:
		return 2
	case ColorGreen:
		return 1
	default: // ColorUnknown / ""
		return 0
	}
}

// Subsystem is the health of a single dependency in the dashboard
// report. Detail carries subsystem-specific structured context
// (pool stats, memory usage, stream depths, …) for the operator UI;
// Error carries a short, non-sensitive failure summary when Status is
// red/yellow. Unlike /readyz (which deliberately hides error detail
// because it may be exposed to untrusted clients), the dashboard is
// an admin-only surface, so a bounded error string is acceptable and
// useful — but probes still avoid echoing secrets/credentials.
type Subsystem struct {
	Name   string         `json:"name"`
	Status Color          `json:"status"`
	Detail map[string]any `json:"detail,omitempty"`
	Error  string         `json:"error,omitempty"`
}

// Report is the full health-dashboard payload returned by
// GET /api/admin/health-dashboard.
type Report struct {
	// Status is the worst severity across all subsystems, ignoring
	// ColorUnknown (unconfigured) subsystems.
	Status      Color       `json:"status"`
	GeneratedAt time.Time   `json:"generated_at"`
	Subsystems  []Subsystem `json:"subsystems"`
}

// DashboardProbe gathers the detailed health of one subsystem. Unlike
// the readiness Checker (which returns only an error), a probe owns
// its own status mapping and structured detail so each subsystem can
// express domain-specific nuance (e.g. "reachable but degraded").
//
// Implementations MUST respect ctx (per-probe timeout) and MUST NOT
// panic: a probe panic must not take down the admin dashboard. The
// Dashboard runner additionally recovers from panics defensively.
type DashboardProbe interface {
	// SubsystemName is the stable key under which this probe's result
	// appears in the report.
	SubsystemName() string
	// Probe runs the health check and returns the subsystem result.
	Probe(ctx context.Context) Subsystem
}

// Dashboard runs a set of probes concurrently under a per-probe
// timeout and assembles the Report. Construct via NewDashboard.
type Dashboard struct {
	probes  []DashboardProbe
	timeout time.Duration
}

// DefaultDashboardTimeout is the per-probe timeout the admin health
// dashboard uses when no explicit timeout is configured. It is
// deliberately larger than the /readyz DefaultCheckTimeout (900ms):
// the dashboard is an operator-triggered request, not a k8s probe, so
// a ClamAV VERSION round-trip or an ONLYOFFICE /healthcheck that takes
// a couple of seconds is acceptable and should be reported as the real
// (slow-but-up) state rather than timed out.
const DefaultDashboardTimeout = 5 * time.Second

// NewDashboard builds a Dashboard. A zero or negative timeout falls
// back to DefaultDashboardTimeout (5s), NOT the 900ms /readyz budget:
// the dashboard is admin-triggered (not a k8s probe), so a ClamAV
// version round-trip or a NATS stream info call that takes a couple of
// seconds is a slow-but-healthy state, not a fault. Falling back to the
// readiness budget here would make those probes spuriously report red.
func NewDashboard(probes []DashboardProbe, timeout time.Duration) *Dashboard {
	if timeout <= 0 {
		timeout = DefaultDashboardTimeout
	}
	return &Dashboard{probes: probes, timeout: timeout}
}

// Report runs every probe concurrently, each under its own timeout
// derived from ctx, and returns the assembled report. Total wall time
// is bounded by the slowest probe, not the sum. Subsystems are sorted
// by name for stable rendering. The overall Status is the worst
// severity across configured subsystems (ColorUnknown excluded); an
// all-unknown dashboard reports ColorUnknown.
func (d *Dashboard) Report(ctx context.Context) Report {
	results := make([]Subsystem, len(d.probes))
	var wg sync.WaitGroup
	for i, p := range d.probes {
		wg.Add(1)
		go func(i int, p DashboardProbe) {
			defer wg.Done()
			results[i] = d.runProbe(ctx, p)
		}(i, p)
	}
	wg.Wait()

	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })

	overall := ColorUnknown
	for _, s := range results {
		if s.Status.severity() > overall.severity() {
			overall = s.Status
		}
	}
	return Report{
		Status:      overall,
		GeneratedAt: time.Now().UTC(),
		Subsystems:  results,
	}
}

// runProbe invokes a single probe under the per-probe timeout and
// recovers from any panic so one misbehaving probe cannot crash the
// admin request. A panicking or timed-out probe is reported red.
func (d *Dashboard) runProbe(ctx context.Context, p DashboardProbe) (s Subsystem) {
	probeCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			s = Subsystem{
				Name:   p.SubsystemName(),
				Status: ColorRed,
				Error:  "probe panicked",
			}
		}
	}()
	return p.Probe(probeCtx)
}
