package drive

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/preview"
	"github.com/kennguy3n/zk-drive/internal/webhooks"
)

// fakeTagSuggester is a non-nil concrete TagSuggester used as the
// "happy path" anchor in the typed-nil-guard test. Returning an
// empty slice keeps the test focused on the guard semantics rather
// than the suggestion plumbing.
type fakeTagSuggester struct{}

func (fakeTagSuggester) Suggest(ctx context.Context, workspaceID, fileID uuid.UUID) ([]string, error) {
	return nil, nil
}

// fakeQueryExpander is the QueryExpander analogue of fakeTagSuggester.
type fakeQueryExpander struct{}

func (fakeQueryExpander) Expand(ctx context.Context, workspaceID uuid.UUID, query string) ([]string, bool, string, error) {
	return nil, false, "", nil
}

// nilSuggester is a typed-nil concrete pointer used to exercise the
// reflect-based guard inside WithTagSuggester. A regular `s == nil`
// comparison against the interface returns false here because the
// type slot is set even though the value slot is nil — exactly the
// failure mode the bot's edited finding flagged on PR #85.
type nilSuggester struct{}

func (*nilSuggester) Suggest(ctx context.Context, workspaceID, fileID uuid.UUID) ([]string, error) {
	// Intentionally dereferences the receiver so a NPE would
	// surface immediately if the guard didn't catch the typed-nil.
	return nil, nil
}

type nilExpander struct{}

func (*nilExpander) Expand(ctx context.Context, workspaceID uuid.UUID, query string) ([]string, bool, string, error) {
	return nil, false, "", nil
}

// nilPreviewRepo is a typed-nil concrete pointer satisfying
// preview.Repository used to exercise the WithPreviews guard.
// Defined as part of Devin Review ANALYSIS_0002 on commit 10bd9b9
// which flagged WithPreviews as the last interface-taking With*
// setter without the isTypedNil guard.
type nilPreviewRepo struct{}

func (*nilPreviewRepo) Upsert(ctx context.Context, p *preview.Preview) error { return nil }
func (*nilPreviewRepo) GetByVersion(ctx context.Context, fileID, versionID uuid.UUID) (*preview.Preview, error) {
	return nil, nil
}
func (*nilPreviewRepo) GetLatestByFile(ctx context.Context, fileID uuid.UUID) (*preview.Preview, error) {
	return nil, nil
}

// nilWebhookPublisher is the WithWebhooks analogue — pinning the
// refactor from the old `p.(*webhooks.Publisher)` type-assertion
// guard to the isTypedNil-based one (same Devin Review finding).
type nilWebhookPublisher struct{}

func (*nilWebhookPublisher) PublishFileEvent(ctx context.Context, t webhooks.EventType, workspaceID uuid.UUID, actorID *uuid.UUID, data webhooks.FileEventData) error {
	return nil
}
func (*nilWebhookPublisher) PublishPermissionEvent(ctx context.Context, t webhooks.EventType, workspaceID uuid.UUID, actorID *uuid.UUID, data webhooks.PermissionEventData) error {
	return nil
}

// nilSuspensionChecker is a typed-nil concrete pointer satisfying
// middleware.WorkspaceSuspensionChecker used to exercise the
// WithSuspensionChecker guard. The receiver method dereferences a
// field so a NPE surfaces immediately if the guard fails to collapse
// the typed-nil to a real nil.
type nilSuspensionChecker struct{ marker int }

func (c *nilSuspensionChecker) WorkspaceSuspension(ctx context.Context, workspaceID uuid.UUID) (bool, string, error) {
	_ = c.marker
	return false, "", nil
}

// TestIsTypedNil pins the reflect-based detection for nil concrete
// pointers wrapped in non-nil interface values. Added as part of
// Devin Review's elevated finding on api/drive/handler.go:203 that
// flagged WithTagSuggester/WithQueryExpander as missing the typed-
// nil guard WithWebhooks already has.
func TestIsTypedNil(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want bool
	}{
		{"nil interface", nil, true},
		{"typed-nil pointer", (*nilSuggester)(nil), true},
		{"non-nil pointer", &nilSuggester{}, false},
		{"non-nil concrete value", fakeTagSuggester{}, false},
		// nil slice / map should also normalise to "typed nil"
		// because the With* setters could in principle accept
		// these someday and the helper's contract should be
		// uniform across nil-checkable kinds.
		{"nil slice", []string(nil), true},
		{"nil map", map[string]int(nil), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTypedNil(tc.in); got != tc.want {
				t.Errorf("isTypedNil(%v) = %v; want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestWithTagSuggesterNormalisesTypedNil pins the WithTagSuggester
// guard's behaviour: a typed-nil concrete pointer must land as a
// nil interface in h.tagSuggest so the runtime nil-check in
// SuggestFileTags (api/drive/ai.go:33) still skips it. Without the
// guard, h.tagSuggest != nil (the interface holds a non-nil type
// slot) and the request handler would NPE on the first method call.
func TestWithTagSuggesterNormalisesTypedNil(t *testing.T) {
	h := &Handler{}
	h.WithTagSuggester((*nilSuggester)(nil))
	if h.tagSuggest != nil {
		t.Errorf("WithTagSuggester(typed-nil) should leave h.tagSuggest as nil interface, got non-nil")
	}

	h.WithTagSuggester(fakeTagSuggester{})
	if h.tagSuggest == nil {
		t.Errorf("WithTagSuggester(non-nil) should set h.tagSuggest, got nil")
	}

	// Round-trip: passing a nil interface explicitly must also
	// clear the field (regression on a previously-wired handler).
	h.WithTagSuggester(nil)
	if h.tagSuggest != nil {
		t.Errorf("WithTagSuggester(nil) should clear h.tagSuggest, got non-nil")
	}
}

// TestWithQueryExpanderNormalisesTypedNil is the QueryExpander
// analogue of the WithTagSuggester guard test.
func TestWithQueryExpanderNormalisesTypedNil(t *testing.T) {
	h := &Handler{}
	h.WithQueryExpander((*nilExpander)(nil))
	if h.queryExpand != nil {
		t.Errorf("WithQueryExpander(typed-nil) should leave h.queryExpand as nil interface, got non-nil")
	}

	h.WithQueryExpander(fakeQueryExpander{})
	if h.queryExpand == nil {
		t.Errorf("WithQueryExpander(non-nil) should set h.queryExpand, got nil")
	}

	h.WithQueryExpander(nil)
	if h.queryExpand != nil {
		t.Errorf("WithQueryExpander(nil) should clear h.queryExpand, got non-nil")
	}
}

// TestWithPreviewsNormalisesTypedNil pins the WithPreviews guard
// added per Devin Review ANALYSIS_0002 on commit 10bd9b9. Without
// the guard, a (*preview.PostgresRepository)(nil) wrapped in
// preview.Repository would slip past h.previews == nil and NPE
// inside handlePreviewURL on first method call.
func TestWithPreviewsNormalisesTypedNil(t *testing.T) {
	h := &Handler{}
	h.WithPreviews((*nilPreviewRepo)(nil))
	if h.previews != nil {
		t.Errorf("WithPreviews(typed-nil) should leave h.previews as nil interface, got non-nil")
	}

	h.WithPreviews(&nilPreviewRepo{})
	if h.previews == nil {
		t.Errorf("WithPreviews(non-nil) should set h.previews, got nil")
	}

	h.WithPreviews(nil)
	if h.previews != nil {
		t.Errorf("WithPreviews(nil) should clear h.previews, got non-nil")
	}
}

// TestWithWebhooksNormalisesTypedNil pins the WithWebhooks refactor
// from the old `p.(*webhooks.Publisher)` type-assertion guard to the
// isTypedNil-based one. Same Devin Review finding (ANALYSIS_0002 on
// commit 10bd9b9). The new guard works for any concrete pointer
// satisfying WebhookEventPublisher, not just the *webhooks.Publisher
// the old assertion was hard-coded to recognise.
func TestWithWebhooksNormalisesTypedNil(t *testing.T) {
	h := &Handler{}
	h.WithWebhooks((*nilWebhookPublisher)(nil))
	if h.webhooks != nil {
		t.Errorf("WithWebhooks(typed-nil) should leave h.webhooks as nil interface, got non-nil")
	}

	h.WithWebhooks(&nilWebhookPublisher{})
	if h.webhooks == nil {
		t.Errorf("WithWebhooks(non-nil) should set h.webhooks, got nil")
	}

	h.WithWebhooks(nil)
	if h.webhooks != nil {
		t.Errorf("WithWebhooks(nil) should clear h.webhooks, got non-nil")
	}
}

// TestWithSuspensionCheckerNormalisesTypedNil pins the
// WithSuspensionChecker guard. Without it, a typed-nil
// (*platform.Service)(nil) wrapped in WorkspaceSuspensionChecker
// would leave h.suspension != nil, so ensureNotSuspended's
// h.suspension == nil short-circuit would be skipped and the
// WorkspaceSuspension call would NPE on the ONLYOFFICE save path.
func TestWithSuspensionCheckerNormalisesTypedNil(t *testing.T) {
	h := &Handler{}
	h.WithSuspensionChecker((*nilSuspensionChecker)(nil))
	if h.suspension != nil {
		t.Errorf("WithSuspensionChecker(typed-nil) should leave h.suspension as nil interface, got non-nil")
	}
	// ensureNotSuspended must treat the normalised nil as "no checker
	// wired" and return nil without dereferencing.
	if err := h.ensureNotSuspended(context.Background(), uuid.New()); err != nil {
		t.Errorf("ensureNotSuspended with typed-nil checker should be a no-op, got %v", err)
	}

	h.WithSuspensionChecker(&nilSuspensionChecker{})
	if h.suspension == nil {
		t.Errorf("WithSuspensionChecker(non-nil) should set h.suspension, got nil")
	}

	h.WithSuspensionChecker(nil)
	if h.suspension != nil {
		t.Errorf("WithSuspensionChecker(nil) should clear h.suspension, got non-nil")
	}
}
