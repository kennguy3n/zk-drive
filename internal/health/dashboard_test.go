package health

import (
	"context"
	"testing"
	"time"
)

// stubProbe is a DashboardProbe whose result is fixed, with an optional
// delay and panic to exercise the runner's timeout / recover paths.
type stubProbe struct {
	name   string
	result Subsystem
	delay  time.Duration
	panics bool
}

func (p stubProbe) SubsystemName() string { return p.name }
func (p stubProbe) Probe(ctx context.Context) Subsystem {
	if p.panics {
		panic("boom")
	}
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return Subsystem{Name: p.name, Status: ColorRed, Error: "timeout"}
		}
	}
	return p.result
}

func TestColorSeverityOrder(t *testing.T) {
	// Unknown must sit below green so an unconfigured optional
	// dependency never drags the overall roll-up down.
	if !(ColorUnknown.severity() < ColorGreen.severity() &&
		ColorGreen.severity() < ColorYellow.severity() &&
		ColorYellow.severity() < ColorRed.severity()) {
		t.Fatal("severity ordering must be unknown < green < yellow < red")
	}
}

// TestNewDashboardZeroTimeoutFallsBackToDashboardDefault pins the
// fallback: a zero/negative timeout must adopt DefaultDashboardTimeout
// (5s), not the 900ms /readyz budget, so slow-but-healthy probes
// (ClamAV, ONLYOFFICE) are not spuriously timed out to red.
func TestNewDashboardZeroTimeoutFallsBackToDashboardDefault(t *testing.T) {
	for _, tc := range []time.Duration{0, -1} {
		d := NewDashboard(nil, tc)
		if d.timeout != DefaultDashboardTimeout {
			t.Fatalf("NewDashboard(_, %v).timeout = %v, want %v", tc, d.timeout, DefaultDashboardTimeout)
		}
	}
	// An explicit positive timeout is honoured verbatim.
	if d := NewDashboard(nil, 2*time.Second); d.timeout != 2*time.Second {
		t.Fatalf("explicit timeout overridden: got %v", d.timeout)
	}
}

func TestReportRollupWorstSeverity(t *testing.T) {
	d := NewDashboard([]DashboardProbe{
		stubProbe{name: "postgres", result: Subsystem{Name: "postgres", Status: ColorGreen}},
		stubProbe{name: "redis", result: Subsystem{Name: "redis", Status: ColorYellow}},
		stubProbe{name: "nats", result: Subsystem{Name: "nats", Status: ColorGreen}},
	}, time.Second)

	rep := d.Report(context.Background())
	if rep.Status != ColorYellow {
		t.Fatalf("overall = %q, want yellow (worst of green/yellow/green)", rep.Status)
	}
	// Subsystems must be sorted by name for stable rendering.
	if rep.Subsystems[0].Name != "nats" || rep.Subsystems[1].Name != "postgres" || rep.Subsystems[2].Name != "redis" {
		t.Fatalf("subsystems not sorted by name: %+v", rep.Subsystems)
	}
}

func TestReportUnknownDoesNotDragDown(t *testing.T) {
	d := NewDashboard([]DashboardProbe{
		stubProbe{name: "postgres", result: Subsystem{Name: "postgres", Status: ColorGreen}},
		stubProbe{name: "redis", result: Subsystem{Name: "redis", Status: ColorUnknown}},
	}, time.Second)
	rep := d.Report(context.Background())
	if rep.Status != ColorGreen {
		t.Fatalf("overall = %q, want green (unknown excluded)", rep.Status)
	}
}

func TestReportAllUnknown(t *testing.T) {
	d := NewDashboard([]DashboardProbe{
		stubProbe{name: "redis", result: Subsystem{Name: "redis", Status: ColorUnknown}},
		stubProbe{name: "nats", result: Subsystem{Name: "nats", Status: ColorUnknown}},
	}, time.Second)
	rep := d.Report(context.Background())
	if rep.Status != ColorUnknown {
		t.Fatalf("overall = %q, want unknown for an all-unconfigured dashboard", rep.Status)
	}
}

func TestReportRecoversFromPanic(t *testing.T) {
	d := NewDashboard([]DashboardProbe{
		stubProbe{name: "postgres", result: Subsystem{Name: "postgres", Status: ColorGreen}},
		stubProbe{name: "boom", panics: true},
	}, time.Second)
	rep := d.Report(context.Background())
	if rep.Status != ColorRed {
		t.Fatalf("a panicking probe must be reported red, overall=%q", rep.Status)
	}
	var found bool
	for _, s := range rep.Subsystems {
		if s.Name == "boom" {
			found = true
			if s.Status != ColorRed || s.Error == "" {
				t.Fatalf("panicking probe should be red with an error, got %+v", s)
			}
		}
	}
	if !found {
		t.Fatal("panicking probe missing from report")
	}
}

func TestReportProbeTimeoutEnforced(t *testing.T) {
	// A probe slower than the dashboard timeout must surface its
	// ctx-cancelled fallback rather than blocking the whole report.
	d := NewDashboard([]DashboardProbe{
		stubProbe{name: "slow", delay: 2 * time.Second, result: Subsystem{Name: "slow", Status: ColorGreen}},
	}, 50*time.Millisecond)
	start := time.Now()
	rep := d.Report(context.Background())
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("report blocked %v; per-probe timeout not enforced", elapsed)
	}
	if rep.Subsystems[0].Status != ColorRed {
		t.Fatalf("slow probe should report its timeout fallback, got %+v", rep.Subsystems[0])
	}
}
