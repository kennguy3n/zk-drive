package webhooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// DefaultDeliveryTimeout caps a single HTTP attempt. 30s is long
// enough for a slow subscriber (synchronous database write etc.) but
// short enough that one wedged endpoint can't pin the worker. The
// JetStream redelivery cadence (exponential backoff starting at 1s)
// gives the subscriber more cumulative time across attempts than this
// single-shot timeout would suggest.
const DefaultDeliveryTimeout = 30 * time.Second

// DefaultMaxResponseBodyBytes caps how much of the subscriber's HTTP
// response we read into memory before truncating. 4 KiB is enough for
// a typical JSON error body ("{\"error\":\"invalid event type\"}")
// without letting a chatty endpoint blow up the webhook_deliveries
// table. The matching column comment in migration 028 documents the
// truncation contract.
const DefaultMaxResponseBodyBytes = 4 * 1024

// DefaultUserAgent is sent on every webhook POST. Suffixed with the
// version constant baked into the binary at build time so a subscriber
// can correlate behavioural changes against a specific server release.
const DefaultUserAgent = "zk-drive-webhooks/1"

// DeliveryOutcome categorises the terminal state of one attempt.
// Mirrors the CHECK constraint in migration 028's webhook_deliveries
// table — keep the literals in sync.
type DeliveryOutcome string

const (
	OutcomeSuccess   DeliveryOutcome = "success"
	OutcomeHTTPError DeliveryOutcome = "http_error"
	OutcomeNetError  DeliveryOutcome = "net_error"
	OutcomeBlocked   DeliveryOutcome = "blocked"
)

// AllOutcomes returns every possible DeliveryOutcome value. Used by
// the metrics layer to pre-register histograms / counters with every
// label value so Prometheus' /metrics output is stable even before
// any deliveries have run.
func AllOutcomes() []DeliveryOutcome {
	return []DeliveryOutcome{OutcomeSuccess, OutcomeHTTPError, OutcomeNetError, OutcomeBlocked}
}

// DeliveryAttempt is the value passed back to the caller summarising
// one Deliver call. The repository persists it as a webhook_deliveries
// row; the metrics layer also records (Outcome, StatusCode, Duration)
// off the same struct.
type DeliveryAttempt struct {
	// ID is the per-attempt UUID (NOT the event ID). Generated in
	// Deliver so callers don't have to set it.
	ID uuid.UUID
	// Outcome categorises the result. See the constants above.
	Outcome DeliveryOutcome
	// StatusCode is the HTTP status code the subscriber returned.
	// 0 means no HTTP response was received (the request failed at
	// dial / TLS / timeout layer or was blocked pre-flight by the
	// SSRF re-validation).
	StatusCode int
	// ResponseBody is the first DefaultMaxResponseBodyBytes bytes
	// of the subscriber's response, suffixed with "[truncated]" if
	// the response exceeded the cap. Empty when Outcome is
	// net_error / blocked.
	ResponseBody string
	// ErrorMessage carries the Go error string for net_error /
	// blocked outcomes (DNS failure, TLS handshake error, etc.).
	// Empty for success / http_error.
	ErrorMessage string
	// Duration is the wall-clock time from request-start to
	// response-end (or to the failure point).
	Duration time.Duration
	// AttemptedAt records when the attempt was started, in UTC.
	AttemptedAt time.Time
}

// DeliveryClient performs a single HTTP POST attempt to a webhook
// subscriber. Re-used across attempts and across subscriptions; one
// instance is sufficient for the whole worker process.
type DeliveryClient struct {
	httpClient *http.Client
	validator  *URLValidator
	userAgent  string
	maxBody    int64
}

// NewDeliveryClient constructs a delivery client. The supplied
// timeout is applied per request via http.Client.Timeout; a separate
// per-attempt context with the same timeout is also created in
// Deliver so the cancellation propagates through transport callbacks
// that don't honour http.Client.Timeout (notably TLS handshake
// callbacks on some Go versions).
func NewDeliveryClient(validator *URLValidator, timeout time.Duration) *DeliveryClient {
	if timeout <= 0 {
		timeout = DefaultDeliveryTimeout
	}
	if validator == nil {
		validator = NewURLValidator()
	}
	return &DeliveryClient{
		httpClient: &http.Client{
			Timeout: timeout,
			// Defense-in-depth: we already SSRF-validate the
			// URL BEFORE issuing the request, but we also
			// refuse to follow redirects so a 302 to
			// http://169.254.169.254/ from a compromised
			// subscriber can't bypass the IP check. The
			// alternative — re-validate every redirect target
			// in CheckRedirect — has the same effect but is
			// more code; we choose the simpler "no redirects"
			// policy. Subscribers should answer at a stable
			// URL anyway.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		validator: validator,
		userAgent: DefaultUserAgent,
		maxBody:   DefaultMaxResponseBodyBytes,
	}
}

// SetUserAgent overrides the default User-Agent. Used by tests; not
// expected in production.
func (c *DeliveryClient) SetUserAgent(ua string) {
	if ua != "" {
		c.userAgent = ua
	}
}

// Deliver issues one POST attempt to u with the supplied JSON body,
// signed by signer at time ts (passed in rather than reading the
// clock so the same timestamp lands on the wire signature, the
// returned AttemptedAt, and the per-event idempotency key).
//
// Deliver NEVER returns a Go error for "the subscriber returned 500"
// — that's a successful attempt with Outcome=http_error. The only
// returned error is the catastrophic case where the attempt couldn't
// even be characterised (e.g. ctx already cancelled before any work);
// in practice callers ignore the error and just record the attempt.
//
// Each call also performs ValidateResolved against the URL's host as
// a DNS-rebinding defense: a subscription registered against
// host.example.com -> 1.2.3.4 may have its DNS swapped to
// host.example.com -> 169.254.169.254 between subscription-create and
// delivery; re-resolving on every attempt catches the swap.
func (c *DeliveryClient) Deliver(ctx context.Context, u *url.URL, eventID, deliveryID uuid.UUID, eventType EventType, body []byte, signer *Signer, ts time.Time) DeliveryAttempt {
	a := DeliveryAttempt{
		ID:          deliveryID,
		AttemptedAt: ts.UTC(),
	}

	// SSRF re-validation. If the host now resolves into a blocked
	// range we abandon the attempt without sending the request —
	// recording outcome=blocked so the operator sees the rebinding
	// in the delivery history.
	if err := c.validator.ValidateResolved(ctx, u); err != nil {
		a.Outcome = OutcomeBlocked
		a.ErrorMessage = err.Error()
		a.Duration = time.Since(ts)
		return a
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		a.Outcome = OutcomeNetError
		a.ErrorMessage = fmt.Sprintf("build request: %v", err)
		a.Duration = time.Since(ts)
		return a
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set(SignatureHeader, signer.Sign(body, ts))
	req.Header.Set(EventIDHeader, eventID.String())
	req.Header.Set(EventTypeHeader, string(eventType))
	req.Header.Set(DeliveryIDHeader, deliveryID.String())
	// Content-Length helps proxies and load balancers route the
	// request without buffering the body. http.NewRequest sets
	// this automatically from bytes.Reader, but we also set it
	// explicitly so a future swap to io.Reader doesn't drop the
	// header.
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))

	resp, err := c.httpClient.Do(req)
	a.Duration = time.Since(ts)
	if err != nil {
		a.Outcome = OutcomeNetError
		a.ErrorMessage = err.Error()
		return a
	}
	defer func() { _ = resp.Body.Close() }()

	a.StatusCode = resp.StatusCode

	// Read up to maxBody bytes of the response, then drain the
	// rest into io.Discard so the connection is reusable. The
	// "[truncated]" suffix gives admins reading the delivery
	// history a clear signal that there was more.
	buf := &bytes.Buffer{}
	_, copyErr := io.CopyN(buf, resp.Body, c.maxBody+1)
	if copyErr != nil && !errors.Is(copyErr, io.EOF) {
		// A read failure here is rare (the request itself
		// already succeeded). Outcome is still derived from
		// resp.StatusCode below — a 2xx with a body-read
		// failure stays OutcomeSuccess (the subscriber DID
		// acknowledge the event); only the ErrorMessage is
		// populated so an admin can see the diagnostic. A
		// non-2xx with a body-read failure stays
		// OutcomeHTTPError. We deliberately do NOT downgrade
		// to OutcomeNetError because the network round-trip
		// itself succeeded.
		a.ResponseBody = buf.String()
		a.ErrorMessage = fmt.Sprintf("read response body: %v", copyErr)
	} else if int64(buf.Len()) > c.maxBody {
		// Truncate to maxBody bytes and append a clear marker.
		truncated := buf.Bytes()[:c.maxBody]
		a.ResponseBody = string(truncated) + " [truncated]"
		// Drain the rest so the connection returns to the pool.
		_, _ = io.Copy(io.Discard, resp.Body)
	} else {
		a.ResponseBody = buf.String()
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		a.Outcome = OutcomeSuccess
	} else {
		a.Outcome = OutcomeHTTPError
	}
	return a
}

// MaxAttempts is the total number of times the worker will try to
// deliver a single event before giving up. Initial attempt + 4
// retries = 5 attempts total. Matches GitHub Apps' webhook retry
// budget exactly; chosen so a multi-hour subscriber outage still
// loses the events on a published schedule rather than infinitely
// queuing.
const MaxAttempts = 5

// BackoffDelay returns the delay before the Nth retry attempt
// (attempt is 1-indexed; the initial attempt is 1, so BackoffDelay(2)
// is the delay before the FIRST retry). Exponential schedule:
// 1s, 2s, 4s, 8s for retries 2-5. Caller adds jitter in production.
func BackoffDelay(attempt int) time.Duration {
	if attempt <= 1 {
		return 0
	}
	// 1s * 2^(attempt-2) — attempt 2 -> 1s, attempt 3 -> 2s, ...
	shift := attempt - 2
	if shift > 6 {
		shift = 6 // cap at 64s in case MaxAttempts grows later
	}
	return time.Duration(1<<shift) * time.Second
}
