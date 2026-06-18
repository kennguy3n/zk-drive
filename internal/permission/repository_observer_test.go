package permission

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// recordingDBObserver captures every RecordDBQuery invocation so
// tests can assert which code paths fired the observer (and
// importantly, which did NOT).
type recordingDBObserver struct {
	mu      sync.Mutex
	records []dbObservation
}

type dbObservation struct {
	op       string
	duration time.Duration
	result   string
}

func (o *recordingDBObserver) RecordDBQuery(op string, duration time.Duration, result string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.records = append(o.records, dbObservation{op: op, duration: duration, result: result})
}

func (o *recordingDBObserver) snapshot() []dbObservation {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]dbObservation, len(o.records))
	copy(out, o.records)
	return out
}

// TestPostgresRepository_ValidationDoesNotPolluteDBMetrics pins
// the contract that input-validation failures (invalid role,
// invalid resource type) do NOT fire the DB observer — they did
// not consume a Postgres round-trip and would otherwise pollute
// the result="error" counter and the duration histogram.
//
// An earlier implementation had `defer r.observeQuery(...)` at the
// top of the method, BEFORE validation; the defer now sits below
// validation. This test ensures the contract holds going
// forward.
//
// Implementation note: we don't need a real Postgres pool to
// exercise this path because validation returns BEFORE any pool
// access. A nil pool is fine — if validation accidentally falls
// through, the next line (`r.pool.Query`) would panic, which
// would also fail the test loudly.
func TestPostgresRepository_ValidationDoesNotPolluteDBMetrics(t *testing.T) {
	t.Parallel()
	obs := &recordingDBObserver{}
	repo := &PostgresRepository{obs: obs}
	ctx := context.Background()

	t.Run("CheckAccess rejects invalid role without DB observer fire", func(t *testing.T) {
		_, err := repo.CheckAccess(ctx, uuid.New(), ResourceFile, uuid.New(), GranteeUser, uuid.New(), "not-a-real-role")
		if err == nil {
			t.Fatal("expected validation error for invalid role; got nil")
		}
		if got := len(obs.snapshot()); got != 0 {
			t.Errorf("expected 0 DB observations on validation failure; got %d (%+v)", got, obs.snapshot())
		}
	})

	t.Run("CheckAccessWithInheritance rejects invalid role without DB observer fire", func(t *testing.T) {
		obs2 := &recordingDBObserver{}
		repo2 := &PostgresRepository{obs: obs2}
		_, err := repo2.CheckAccessWithInheritance(ctx, uuid.New(), ResourceFile, uuid.New(), GranteeUser, uuid.New(), "garbage")
		if err == nil {
			t.Fatal("expected validation error for invalid role; got nil")
		}
		if got := len(obs2.snapshot()); got != 0 {
			t.Errorf("expected 0 DB observations on validation failure; got %d (%+v)", got, obs2.snapshot())
		}
	})

	t.Run("CheckAccessWithInheritance rejects invalid resource type without DB observer fire", func(t *testing.T) {
		obs3 := &recordingDBObserver{}
		repo3 := &PostgresRepository{obs: obs3}
		_, err := repo3.CheckAccessWithInheritance(ctx, uuid.New(), "not-a-resource", uuid.New(), GranteeUser, uuid.New(), RoleViewer)
		if err == nil {
			t.Fatal("expected validation error for invalid resource type; got nil")
		}
		if got := len(obs3.snapshot()); got != 0 {
			t.Errorf("expected 0 DB observations on validation failure; got %d (%+v)", got, obs3.snapshot())
		}
	})
}
