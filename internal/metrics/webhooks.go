package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// WebhookOutcome* are the bounded label values for the
// webhook delivery counter. Kept in sync with the literals in
// internal/webhooks/delivery.go's DeliveryOutcome enum — a typo here
// would silently mint a new series that nobody alerts on.
const (
	WebhookOutcomeSuccess   = "success"
	WebhookOutcomeHTTPError = "http_error"
	WebhookOutcomeNetError  = "net_error"
	WebhookOutcomeBlocked   = "blocked"
)

// statusCodeBucket reduces an HTTP status code to one of the four
// summary buckets ("2xx" / "3xx" / "4xx" / "5xx" / "none"). Used as a
// metric label so cardinality stays small — the full integer range
// would expand the timeseries dimensions by ~500 per subscription
// type. Subscribers needing exact codes can query the
// webhook_deliveries table directly.
func statusCodeBucket(code int) string {
	switch {
	case code == 0:
		return "none"
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500 && code < 600:
		return "5xx"
	default:
		return "other"
	}
}

// RecordWebhookDelivery emits the per-attempt counter labelled by
// outcome + status code bucket, AND observes the duration histogram
// labelled by outcome. Called once per HTTP attempt by the worker —
// regardless of success / failure / SSRF-block — so the dashboards
// always see attempt cadence + outcome distribution.
//
// The "status_bucket" label collapses the integer status into 2xx /
// 3xx / 4xx / 5xx / none. "none" covers net_error + blocked outcomes
// where no HTTP response was received.
func (m *Metrics) RecordWebhookDelivery(outcome string, statusCode int, duration time.Duration) {
	m.webhookDeliveriesTotal.WithLabelValues(outcome, statusCodeBucket(statusCode)).Inc()
	m.webhookDeliveryDuration.WithLabelValues(outcome).Observe(duration.Seconds())
}

// registerWebhookMetrics mounts the webhook metric family on the
// supplied registry. Same promauto.With(reg) shape as the other
// metric families in metrics.New() so a contributor adding the next
// family by copy-paste lands on the same pattern.
func (m *Metrics) registerWebhookMetrics(reg prometheus.Registerer) {
	auto := promauto.With(reg)

	m.webhookDeliveriesTotal = auto.NewCounterVec(prometheus.CounterOpts{
		Name: "zkdrive_webhook_deliveries_total",
		Help: "Total webhook delivery attempts, partitioned by outcome ('success' = 2xx response; 'http_error' = non-2xx response; 'net_error' = DNS/dial/TLS/timeout failure with no HTTP response; 'blocked' = URL re-validated into a forbidden range at delivery time, no request sent) and HTTP status bucket ('2xx' / '3xx' / '4xx' / '5xx' / 'none').",
	}, []string{"outcome", "status_bucket"})

	m.webhookDeliveryDuration = auto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "zkdrive_webhook_delivery_duration_seconds",
		Help:    "Wall time per webhook delivery attempt (request-start to response-end, or to failure). Labelled by outcome so operators can plot p99 success latency separately from net_error timeouts.",
		Buckets: webhookDeliveryBuckets,
	}, []string{"outcome"})
}

// webhookDeliveryBuckets cover the per-attempt duration range. Lowest
// bucket (10 ms) catches local-network subscribers; highest bucket
// (60 s) caps at the DefaultDeliveryTimeout. Slightly finer than the
// audit-archive buckets because per-delivery latency is the dominant
// signal operators tune against.
var webhookDeliveryBuckets = []float64{
	0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60,
}
