package webhooks

import (
	"time"

	"github.com/google/uuid"
)

// MaxSubscriptionsPerWorkspace caps the number of active subscriptions
// per workspace. 20 mirrors GitHub's per-repo webhook cap; high enough
// for legitimate use cases (one per integration consumer) but low
// enough that a misconfigured automation can't enqueue thousands of
// deliveries per event.
const MaxSubscriptionsPerWorkspace = 20

// AutoPauseThreshold is the number of consecutive non-success
// outcomes after which the worker auto-pauses a subscription. 50
// chosen empirically: a flaky-but-recoverable subscriber typically
// produces fewer than 10 consecutive failures over a 30-minute
// window; a permanently broken one produces dozens. The threshold
// puts a clear ceiling on the cumulative delivery cost we pay for
// a broken endpoint.
const AutoPauseThreshold = 50

// SecretByteLength is the number of random bytes used as the HMAC
// secret. 32 bytes = 256 bits = matches SHA-256's output width and
// gives a worst-case key-search cost equivalent to brute-forcing the
// hash itself.
const SecretByteLength = 32

// Subscription mirrors one webhook_subscriptions row. The Secret
// field is populated only on the response to a successful Create
// (the admin needs it to configure their consumer); list/get
// responses return the row with Secret zeroed out. The Repository
// layer maintains this contract.
type Subscription struct {
	ID                  uuid.UUID
	WorkspaceID         uuid.UUID
	CreatedBy           uuid.UUID
	URL                 string
	EventType           EventType
	Description         string
	Secret              string
	Active              bool
	ConsecutiveFailures int
	LastSucceededAt     *time.Time
	LastAttemptedAt     *time.Time
	AutoPausedAt        *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// PublicView returns a copy of the subscription safe to serialise to
// admin UI / API clients. It zeroes the Secret field so the hex
// random bytes never leave the create-time response. Operators who
// lose the secret rotate the subscription rather than recovering it.
func (s Subscription) PublicView() Subscription {
	s.Secret = ""
	return s
}

// Delivery mirrors one webhook_deliveries row.
type Delivery struct {
	ID             uuid.UUID
	SubscriptionID uuid.UUID
	WorkspaceID    uuid.UUID
	EventID        uuid.UUID
	EventType      EventType
	AttemptNumber  int
	Outcome        DeliveryOutcome
	StatusCode     int
	ResponseBody   string
	ErrorMessage   string
	DurationMs     int
	AttemptedAt    time.Time
	NextRetryAt    *time.Time
}
