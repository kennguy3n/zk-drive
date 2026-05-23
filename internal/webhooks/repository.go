package webhooks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrSubscriptionNotFound is returned when a Get / Update / Delete
// finds no row matching the supplied (workspace, id) tuple. Tenant
// isolation is enforced both by the WHERE clause and by RLS so the
// "wrong workspace" case also produces this sentinel.
var ErrSubscriptionNotFound = errors.New("webhooks: subscription not found")

// ErrSubscriptionCapReached is returned when Create would push the
// workspace over MaxSubscriptionsPerWorkspace. The API layer maps
// this to HTTP 409 Conflict so the admin UI can render a clear "you
// have N webhooks; delete one before adding another" message.
var ErrSubscriptionCapReached = errors.New("webhooks: subscription cap reached for this workspace")

// Repository is the persistence interface the service depends on.
// Defined here (not in service.go) so unit tests can wire an
// in-memory fake without dragging pgx in.
type Repository interface {
	Create(ctx context.Context, s *Subscription) error
	GetByID(ctx context.Context, workspaceID, id uuid.UUID) (*Subscription, error)
	List(ctx context.Context, workspaceID uuid.UUID) ([]*Subscription, error)
	// ListActiveForEvent returns every active subscription in the
	// given workspace that subscribes to event_type. The worker
	// calls this on every event delivery to fan out the publish.
	ListActiveForEvent(ctx context.Context, workspaceID uuid.UUID, eventType EventType) ([]*Subscription, error)
	Delete(ctx context.Context, workspaceID, id uuid.UUID) error
	// UpdateAttempt records the outcome of one delivery attempt
	// against the subscription's counters (consecutive_failures,
	// last_succeeded_at, last_attempted_at) and auto-pauses when
	// the threshold is reached. Atomic — implemented as a single
	// UPDATE statement so concurrent deliveries against the same
	// subscription can't interleave halfway through.
	UpdateAttempt(ctx context.Context, workspaceID, subscriptionID uuid.UUID, outcome DeliveryOutcome, at time.Time) error
	// SetActive flips the active flag (pause / resume). Used by the
	// admin UI's resume button after an auto-pause.
	SetActive(ctx context.Context, workspaceID, id uuid.UUID, active bool) error
	// InsertDelivery records one delivery attempt row.
	InsertDelivery(ctx context.Context, d *Delivery) error
	// ListDeliveries returns the most recent attempts for the
	// subscription, paginated by attempted_at DESC.
	ListDeliveries(ctx context.Context, workspaceID, subscriptionID uuid.UUID, limit int) ([]*Delivery, error)
}

// PostgresRepository implements Repository against the Postgres
// pool. The pool is wired without setting app.workspace_id when used
// from the worker (so RLS falls through to the bypass branch); the
// API handlers run inside a per-request transaction that DOES set the
// GUC, so admin-console queries see only their own workspace.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository constructs a PostgresRepository.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

const subColumns = `
	id, workspace_id, created_by, url, event_type, description,
	secret, active, consecutive_failures, last_succeeded_at,
	last_attempted_at, auto_paused_at, created_at, updated_at
`

// Create inserts a new subscription. Enforces MaxSubscriptionsPerWorkspace
// in a single CTE that counts the existing rows AND inserts atomically,
// so two concurrent creates can't both squeeze in as the 21st row.
//
// The secret is generated server-side (NEVER accepted from the client)
// — this keeps secrets uniformly strong and avoids the "operator
// pasted their old shared secret" failure mode.
func (r *PostgresRepository) Create(ctx context.Context, s *Subscription) error {
	if s.Secret == "" {
		secret, err := GenerateSecret()
		if err != nil {
			return fmt.Errorf("generate secret: %w", err)
		}
		s.Secret = secret
	}
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	// The cap is on ACTIVE subscriptions, not on the row count, so
	// an admin who has accumulated 5 auto-paused rows can still
	// register their 20 active subscriptions. Mirrors the
	// documented contract in README ("20 active subscriptions") and
	// the handler-test fakeRepo which also filters on Active.
	const q = `
WITH cap_check AS (
	SELECT COUNT(*) AS n
	FROM webhook_subscriptions
	WHERE workspace_id = $2 AND active = TRUE
)
INSERT INTO webhook_subscriptions
	(id, workspace_id, created_by, url, event_type, description, secret, active)
SELECT $1, $2, $3, $4, $5, $6, $7, TRUE
FROM cap_check
WHERE cap_check.n < $8
RETURNING created_at, updated_at`
	var description any
	if s.Description != "" {
		description = s.Description
	}
	err := r.pool.QueryRow(ctx, q,
		s.ID, s.WorkspaceID, s.CreatedBy, s.URL, string(s.EventType),
		description, s.Secret, MaxSubscriptionsPerWorkspace,
	).Scan(&s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSubscriptionCapReached
		}
		return fmt.Errorf("insert webhook_subscriptions: %w", err)
	}
	s.Active = true
	return nil
}

func scanSubscription(row pgx.Row) (*Subscription, error) {
	var s Subscription
	var description *string
	var eventType string
	if err := row.Scan(
		&s.ID, &s.WorkspaceID, &s.CreatedBy, &s.URL, &eventType, &description,
		&s.Secret, &s.Active, &s.ConsecutiveFailures, &s.LastSucceededAt,
		&s.LastAttemptedAt, &s.AutoPausedAt, &s.CreatedAt, &s.UpdatedAt,
	); err != nil {
		return nil, err
	}
	s.EventType = EventType(eventType)
	if description != nil {
		s.Description = *description
	}
	return &s, nil
}

// GetByID returns one subscription, scoped to the workspace.
func (r *PostgresRepository) GetByID(ctx context.Context, workspaceID, id uuid.UUID) (*Subscription, error) {
	q := `SELECT ` + subColumns + ` FROM webhook_subscriptions WHERE id = $1 AND workspace_id = $2`
	row := r.pool.QueryRow(ctx, q, id, workspaceID)
	s, err := scanSubscription(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSubscriptionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("select webhook_subscriptions: %w", err)
	}
	return s, nil
}

// List returns every subscription in the workspace, newest first.
func (r *PostgresRepository) List(ctx context.Context, workspaceID uuid.UUID) ([]*Subscription, error) {
	q := `SELECT ` + subColumns + ` FROM webhook_subscriptions WHERE workspace_id = $1 ORDER BY created_at DESC`
	rows, err := r.pool.Query(ctx, q, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("select webhook_subscriptions: %w", err)
	}
	defer rows.Close()
	out := make([]*Subscription, 0)
	for rows.Next() {
		s, err := scanSubscription(rows)
		if err != nil {
			return nil, fmt.Errorf("scan webhook_subscriptions: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListActiveForEvent is the hot path called by the worker on every
// event delivery: "give me every active subscription in this workspace
// that subscribes to this event type". The partial index
// idx_webhook_subs_active_lookup covers this exactly.
func (r *PostgresRepository) ListActiveForEvent(ctx context.Context, workspaceID uuid.UUID, eventType EventType) ([]*Subscription, error) {
	q := `SELECT ` + subColumns + ` FROM webhook_subscriptions
		WHERE workspace_id = $1 AND event_type = $2 AND active = TRUE`
	rows, err := r.pool.Query(ctx, q, workspaceID, string(eventType))
	if err != nil {
		return nil, fmt.Errorf("select active webhook_subscriptions: %w", err)
	}
	defer rows.Close()
	out := make([]*Subscription, 0)
	for rows.Next() {
		s, err := scanSubscription(rows)
		if err != nil {
			return nil, fmt.Errorf("scan webhook_subscriptions: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Delete hard-removes the subscription. Delivery history rows cascade
// via the FK ON DELETE CASCADE. Returns ErrSubscriptionNotFound when
// the workspace doesn't own the id.
func (r *PostgresRepository) Delete(ctx context.Context, workspaceID, id uuid.UUID) error {
	q := `DELETE FROM webhook_subscriptions WHERE id = $1 AND workspace_id = $2`
	tag, err := r.pool.Exec(ctx, q, id, workspaceID)
	if err != nil {
		return fmt.Errorf("delete webhook_subscriptions: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSubscriptionNotFound
	}
	return nil
}

// SetActive is the manual pause/resume endpoint. Clears auto_paused_at
// when activating so the admin UI's "paused N hours ago" badge
// disappears.
func (r *PostgresRepository) SetActive(ctx context.Context, workspaceID, id uuid.UUID, active bool) error {
	q := `
UPDATE webhook_subscriptions
SET active = $3,
    auto_paused_at = CASE WHEN $3 THEN NULL ELSE auto_paused_at END,
    consecutive_failures = CASE WHEN $3 THEN 0 ELSE consecutive_failures END,
    updated_at = NOW()
WHERE id = $1 AND workspace_id = $2`
	tag, err := r.pool.Exec(ctx, q, id, workspaceID, active)
	if err != nil {
		return fmt.Errorf("update webhook_subscriptions.active: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSubscriptionNotFound
	}
	return nil
}

// UpdateAttempt records one delivery's effect on the subscription's
// running counters. Done as a single UPDATE so a race between two
// in-flight deliveries against the same subscription can't double-
// count failures (the row-level lock Postgres takes during UPDATE
// serialises the read-modify-write).
//
// Auto-pause is implemented inline: when consecutive_failures + 1
// reaches AutoPauseThreshold and the outcome is non-success, the
// statement also flips active=false and sets auto_paused_at=NOW().
func (r *PostgresRepository) UpdateAttempt(ctx context.Context, workspaceID, subscriptionID uuid.UUID, outcome DeliveryOutcome, at time.Time) error {
	q := `
UPDATE webhook_subscriptions
SET last_attempted_at = $4,
    last_succeeded_at = CASE WHEN $3 = 'success' THEN $4 ELSE last_succeeded_at END,
    consecutive_failures = CASE WHEN $3 = 'success' THEN 0 ELSE consecutive_failures + 1 END,
    active = CASE
        WHEN $3 != 'success' AND consecutive_failures + 1 >= $5 THEN FALSE
        ELSE active
    END,
    auto_paused_at = CASE
        WHEN $3 != 'success' AND consecutive_failures + 1 >= $5 THEN $4
        ELSE auto_paused_at
    END,
    updated_at = NOW()
WHERE id = $1 AND workspace_id = $2`
	tag, err := r.pool.Exec(ctx, q, subscriptionID, workspaceID, string(outcome), at.UTC(), AutoPauseThreshold)
	if err != nil {
		return fmt.Errorf("update webhook_subscriptions attempt counters: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSubscriptionNotFound
	}
	return nil
}

// InsertDelivery records one webhook_deliveries row.
func (r *PostgresRepository) InsertDelivery(ctx context.Context, d *Delivery) error {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	q := `
INSERT INTO webhook_deliveries
	(id, subscription_id, workspace_id, event_id, event_type,
	 attempt_number, outcome, status_code, response_body,
	 error_message, duration_ms, attempted_at, next_retry_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`
	var responseBody, errorMessage any
	if d.ResponseBody != "" {
		responseBody = d.ResponseBody
	}
	if d.ErrorMessage != "" {
		errorMessage = d.ErrorMessage
	}
	var nextRetry any
	if d.NextRetryAt != nil {
		nextRetry = d.NextRetryAt.UTC()
	}
	_, err := r.pool.Exec(ctx, q,
		d.ID, d.SubscriptionID, d.WorkspaceID, d.EventID, string(d.EventType),
		d.AttemptNumber, string(d.Outcome), d.StatusCode, responseBody,
		errorMessage, d.DurationMs, d.AttemptedAt.UTC(), nextRetry,
	)
	if err != nil {
		return fmt.Errorf("insert webhook_deliveries: %w", err)
	}
	return nil
}

// ListDeliveries returns the most recent N attempts for the given
// subscription, newest first. Used by the admin UI's "delivery
// history" view.
func (r *PostgresRepository) ListDeliveries(ctx context.Context, workspaceID, subscriptionID uuid.UUID, limit int) ([]*Delivery, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `
SELECT id, subscription_id, workspace_id, event_id, event_type,
       attempt_number, outcome, status_code, response_body,
       error_message, duration_ms, attempted_at, next_retry_at
FROM webhook_deliveries
WHERE subscription_id = $1 AND workspace_id = $2
ORDER BY attempted_at DESC
LIMIT $3`
	rows, err := r.pool.Query(ctx, q, subscriptionID, workspaceID, limit)
	if err != nil {
		return nil, fmt.Errorf("select webhook_deliveries: %w", err)
	}
	defer rows.Close()
	out := make([]*Delivery, 0)
	for rows.Next() {
		var d Delivery
		var eventType, outcome string
		var responseBody, errorMessage *string
		if err := rows.Scan(
			&d.ID, &d.SubscriptionID, &d.WorkspaceID, &d.EventID, &eventType,
			&d.AttemptNumber, &outcome, &d.StatusCode, &responseBody,
			&errorMessage, &d.DurationMs, &d.AttemptedAt, &d.NextRetryAt,
		); err != nil {
			return nil, fmt.Errorf("scan webhook_deliveries: %w", err)
		}
		d.EventType = EventType(eventType)
		d.Outcome = DeliveryOutcome(outcome)
		if responseBody != nil {
			d.ResponseBody = *responseBody
		}
		if errorMessage != nil {
			d.ErrorMessage = *errorMessage
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

// GenerateSecret returns a hex-encoded SecretByteLength-byte random
// string suitable for webhook_subscriptions.secret. Hex (rather than
// base64) so the value is URL-safe and doesn't tempt subscribers to
// trim trailing '=' chars before comparison.
func GenerateSecret() (string, error) {
	buf := make([]byte, SecretByteLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

