package retention

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestArchiveBatchCancellationExcludesSuccessfulItems(t *testing.T) {
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New()}

	// First two items succeed; cancel the context before the third
	// archive call so the loop returns mid-batch. The previous (buggy)
	// implementation used len(failed) as a proxy for the loop index and
	// would have returned ids[0:] (all four), incorrectly including the
	// two successes.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	archive := func(_ context.Context, _ uuid.UUID) error {
		calls++
		if calls == 2 {
			cancel()
		}
		return nil
	}

	got, err := archiveBatch(ctx, ids, archive)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("want archive called 2 times, got %d", calls)
	}

	want := ids[2:]
	if len(got) != len(want) {
		t.Fatalf("want %d remaining ids, got %d (%v)", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want got[%d]=%s, got %s", i, want[i], got[i])
		}
	}
}

func TestArchiveBatchCancellationKeepsFailedItems(t *testing.T) {
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}

	// Mix of one failure and one success before cancellation. The
	// returned slice should contain the failed id plus every id not
	// yet processed (in this case just ids[2]).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	sentinel := errors.New("boom")
	archive := func(_ context.Context, _ uuid.UUID) error {
		calls++
		if calls == 1 {
			return sentinel
		}
		if calls == 2 {
			cancel()
			return nil
		}
		return nil
	}

	got, err := archiveBatch(ctx, ids, archive)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want errors.Is(context.Canceled), got %v", err)
	}
	// errors.Join in the cancel path must preserve the originating
	// per-item error so callers can still introspect the failure.
	if !errors.Is(err, sentinel) {
		t.Fatalf("want errors.Is(sentinel) to be true, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("want archive called 2 times, got %d", calls)
	}

	want := []uuid.UUID{ids[0], ids[2]}
	if len(got) != len(want) {
		t.Fatalf("want %d ids, got %d (%v)", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want got[%d]=%s, got %s", i, want[i], got[i])
		}
	}
}

func TestArchiveBatchAllSucceedReturnsNilNil(t *testing.T) {
	ids := []uuid.UUID{uuid.New(), uuid.New()}

	archive := func(_ context.Context, _ uuid.UUID) error { return nil }

	got, err := archiveBatch(context.Background(), ids, archive)
	if err != nil {
		t.Fatalf("want nil error, got %v", err)
	}
	if got != nil {
		t.Fatalf("want nil failed slice, got %v", got)
	}
}

func TestArchiveBatchCollectsFailuresAndReturnsFirstError(t *testing.T) {
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}

	first := errors.New("first")
	second := errors.New("second")
	calls := 0
	archive := func(_ context.Context, _ uuid.UUID) error {
		calls++
		switch calls {
		case 1:
			return nil
		case 2:
			return first
		case 3:
			return second
		}
		return nil
	}

	got, err := archiveBatch(context.Background(), ids, archive)
	if !errors.Is(err, first) {
		t.Fatalf("want first error, got %v", err)
	}
	want := []uuid.UUID{ids[1], ids[2]}
	if len(got) != len(want) {
		t.Fatalf("want %d failed ids, got %d (%v)", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want got[%d]=%s, got %s", i, want[i], got[i])
		}
	}
}
