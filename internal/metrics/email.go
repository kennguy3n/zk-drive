package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// RecordEmailSent emits the per-send observability counter used by
// internal/email.Service. template is the bounded set of template
// identifiers wired in internal/email (currently just
// "guest_invite"); outcome is the SendOutcome string ("ok",
// "smtp_error", "template_error", "address_invalid", "disabled").
//
// Implements the email.MetricsRecorder interface — the email
// package depends on the abstract surface so it does not import
// this package directly (avoids a cycle when cmd/server wires
// both).
//
// Cardinality is bounded: 1 template × 5 outcomes = 5 series
// per template, per binary. Adding new templates is free; adding
// new outcomes requires a corresponding constant in
// internal/email/service.go to keep the surface documented.
func (m *Metrics) RecordEmailSent(template string, outcome string) {
	m.emailSentTotal.WithLabelValues(template, outcome).Inc()
}

// registerEmailMetrics is called from metrics.New() to mount the
// email counter on the registry. Uses the same promauto.With(reg)
// pattern as every other metric family in New() so the
// construct-and-register step stays atomic and a future
// contributor can copy this shape verbatim without accidentally
// double-registering on the wrong registry.
func (m *Metrics) registerEmailMetrics(reg prometheus.Registerer) {
	auto := promauto.With(reg)
	m.emailSentTotal = auto.NewCounterVec(prometheus.CounterOpts{
		Name: "zkdrive_email_sent_total",
		Help: "Total transactional emails the server attempted to send, partitioned by template and outcome ('ok' = relay accepted DATA; 'smtp_error' = transient transport failure; 'template_error' = render failure; 'address_invalid' = recipient parse failure; 'disabled' = no SMTP relay configured).",
	}, []string{"template", "outcome"})
}
