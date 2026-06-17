package health

import (
	"context"
	"errors"
	"testing"
)

// fakeAILLMProbe stands in for the *ai.OllamaClient behind an
// AILLMDashboardProbe so the probe's traffic-light logic can be
// exercised without a live Ollama daemon. It satisfies aiLLMProbe
// (Model + Health).
type fakeAILLMProbe struct {
	model     string
	healthErr error
}

func (f *fakeAILLMProbe) Model() string                { return f.model }
func (f *fakeAILLMProbe) Health(context.Context) error { return f.healthErr }

func TestAILLMDashboardProbe(t *testing.T) {
	tests := []struct {
		name  string
		probe *fakeAILLMProbe
		want  Color
	}{
		{
			name:  "wired and reachable is green",
			probe: &fakeAILLMProbe{model: "qwen2.5:1.5b"},
			want:  ColorGreen,
		},
		{
			name:  "wired but unreachable is red",
			probe: &fakeAILLMProbe{model: "qwen2.5:1.5b", healthErr: errors.New("dial tcp 127.0.0.1:11434: connection refused")},
			want:  ColorRed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newAILLMDashboardProbeWithProbe(tc.probe)
			got := p.Probe(context.Background())
			if got.Status != tc.want {
				t.Fatalf("status = %q, want %q", got.Status, tc.want)
			}
			if got.Detail["configured"] != true {
				t.Fatalf("configured = %v, want true", got.Detail["configured"])
			}
			if got.Detail["model"] != tc.probe.model {
				t.Fatalf("model = %v, want %q", got.Detail["model"], tc.probe.model)
			}
		})
	}
}

func TestAILLMDashboardProbeUnconfigured(t *testing.T) {
	// A nil client (OLLAMA_URL unset) normalises to a nil probe field,
	// which the dashboard reports as unconfigured/unknown — the AI
	// features simply use the deterministic rule-based fallback, so
	// this is not a fault.
	p := NewAILLMDashboardProbe(nil)
	got := p.Probe(context.Background())
	if got.Status != ColorUnknown {
		t.Fatalf("status = %q, want %q", got.Status, ColorUnknown)
	}
	if got.Detail["configured"] != false {
		t.Fatalf("configured = %v, want false", got.Detail["configured"])
	}
}
