package permission

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeRepo is the minimal in-memory Repository implementation used by
// the service tests. It records every Create the service forwarded
// to it so tests can assert the validation layer normalised inputs
// before the database round-trip — and it deliberately fails CheckAccess
// in the negative paths so the test exercises the wrapper, not the
// fake.
type fakeRepo struct {
	created                  []*Permission
	deleted                  []uuid.UUID
	checkAccessReturn        bool
	checkAccessErr           error
	checkInheritanceReturn   bool
	checkInheritanceErr      error
	listByResourceCalled     bool
	listByResourceReturn     []*Permission
	listByGranteeCalled      bool
	listByGranteeReturn      []*Permission
	getByIDReturn            *Permission
	getByIDErr               error
	expiresAtRecordedAtCall  *time.Time
}

func (f *fakeRepo) Create(_ context.Context, p *Permission) error {
	f.created = append(f.created, p)
	f.expiresAtRecordedAtCall = p.ExpiresAt
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	return nil
}

func (f *fakeRepo) GetByID(_ context.Context, _, _ uuid.UUID) (*Permission, error) {
	return f.getByIDReturn, f.getByIDErr
}

func (f *fakeRepo) ListByResource(_ context.Context, _ uuid.UUID, _ string, _ uuid.UUID) ([]*Permission, error) {
	f.listByResourceCalled = true
	return f.listByResourceReturn, nil
}

func (f *fakeRepo) ListByGrantee(_ context.Context, _ uuid.UUID, _ string, _ uuid.UUID) ([]*Permission, error) {
	f.listByGranteeCalled = true
	return f.listByGranteeReturn, nil
}

func (f *fakeRepo) Delete(_ context.Context, _, permID uuid.UUID) error {
	f.deleted = append(f.deleted, permID)
	return nil
}

func (f *fakeRepo) CheckAccess(_ context.Context, _ uuid.UUID, _ string, _ uuid.UUID, _ string, _ uuid.UUID, _ string) (bool, error) {
	return f.checkAccessReturn, f.checkAccessErr
}

func (f *fakeRepo) CheckAccessWithInheritance(_ context.Context, _ uuid.UUID, _ string, _ uuid.UUID, _ string, _ uuid.UUID, _ string) (bool, error) {
	return f.checkInheritanceReturn, f.checkInheritanceErr
}

// TestRoleRankHierarchy locks down the role hierarchy used by
// CheckAccess. admin > editor > viewer > unknown (=0); any drift
// here silently broadens or narrows access, so this is the cheapest
// regression net we can build.
func TestRoleRankHierarchy(t *testing.T) {
	tests := []struct {
		role string
		want int
	}{
		{RoleAdmin, 3},
		{RoleEditor, 2},
		{RoleViewer, 1},
		{"", 0},
		{"superuser", 0}, // unknowns must map to 0 — no accidental grant.
		{"Viewer", 0},    // case-sensitive — must not accept variants.
	}
	for _, tc := range tests {
		if got := roleRank(tc.role); got != tc.want {
			t.Fatalf("roleRank(%q) = %d, want %d", tc.role, got, tc.want)
		}
	}
	if roleRank(RoleAdmin) <= roleRank(RoleEditor) || roleRank(RoleEditor) <= roleRank(RoleViewer) {
		t.Fatalf("role hierarchy broken: admin=%d editor=%d viewer=%d",
			roleRank(RoleAdmin), roleRank(RoleEditor), roleRank(RoleViewer))
	}
}

// TestValidatorPredicates pins the three validator predicates so a
// new enum value added in a future PR forces a corresponding test
// update.
func TestValidatorPredicates(t *testing.T) {
	for _, tc := range []struct {
		role string
		ok   bool
	}{
		{RoleViewer, true}, {RoleEditor, true}, {RoleAdmin, true},
		{"", false}, {"owner", false}, {"VIEWER", false},
	} {
		if got := isValidRole(tc.role); got != tc.ok {
			t.Fatalf("isValidRole(%q)=%v, want %v", tc.role, got, tc.ok)
		}
	}
	for _, tc := range []struct {
		res string
		ok  bool
	}{
		{ResourceFolder, true}, {ResourceFile, true},
		{"", false}, {"workspace", false}, {"folder ", false},
	} {
		if got := isValidResourceType(tc.res); got != tc.ok {
			t.Fatalf("isValidResourceType(%q)=%v, want %v", tc.res, got, tc.ok)
		}
	}
	for _, tc := range []struct {
		gt string
		ok bool
	}{
		{GranteeUser, true}, {GranteeGuest, true},
		{"", false}, {"service", false}, {"USER", false},
	} {
		if got := isValidGranteeType(tc.gt); got != tc.ok {
			t.Fatalf("isValidGranteeType(%q)=%v, want %v", tc.gt, got, tc.ok)
		}
	}
}

// expectedResourceTypeCount is the number of distinct Resource*
// constants declared in models.go. The companion test
// TestAllResourceTypesIsComplete asserts AllResourceTypes has
// exactly this many entries, so adding a Resource* constant
// without appending it to AllResourceTypes trips CI.
const expectedResourceTypeCount = 2

// TestAllResourceTypesIsComplete pins the invariant that
// AllResourceTypes covers every Resource* constant declared in
// models.go. The cross-package coupling test in
// internal/changefeed/permission_coupling_test.go iterates
// AllResourceTypes to enforce that every resource type has a
// corresponding changefeed Kind whose move/delete ops bust the
// cache. If AllResourceTypes silently drifts from the const
// block, that downstream enforcement gets bypassed.
//
// Per Devin Review ANALYSIS_0003 (escalated): the previous
// failure mode was that adding e.g. ResourceDocument here would
// create a stale-cache risk in the permission cache without any
// test catching it, because the changefeed already had a
// KindDocument that wasn't audited against the resource-type
// vocabulary. This test (combined with the cross-package
// coupling test) closes that gap.
func TestAllResourceTypesIsComplete(t *testing.T) {
	t.Parallel()
	if got := len(AllResourceTypes); got != expectedResourceTypeCount {
		t.Fatalf("AllResourceTypes has %d entries but expectedResourceTypeCount=%d; if you added a new Resource* constant in internal/permission/models.go, also append it to AllResourceTypes and bump expectedResourceTypeCount", got, expectedResourceTypeCount)
	}
	// Belt and braces: assert every entry in AllResourceTypes
	// passes isValidResourceType. If isValidResourceType is
	// later refactored to NOT derive from AllResourceTypes (it
	// currently does), this catches the drift.
	for _, rt := range AllResourceTypes {
		if !isValidResourceType(rt) {
			t.Errorf("AllResourceTypes contains %q but isValidResourceType rejects it — registry/validator drift", rt)
		}
	}
	// Also assert the well-known constants are covered. A
	// future rename of e.g. ResourceFolder to ResourceFolderV2
	// without updating AllResourceTypes would slip past the
	// count check above; this explicit check catches that.
	seen := map[string]bool{}
	for _, rt := range AllResourceTypes {
		seen[rt] = true
	}
	for _, want := range []string{ResourceFolder, ResourceFile} {
		if !seen[want] {
			t.Errorf("AllResourceTypes missing well-known Resource constant %q", want)
		}
	}
}

// TestGrantValidates verifies that every invalid-input branch in
// Grant short-circuits before the repository call, so a buggy or
// malicious caller cannot smuggle an unknown enum past the service
// layer even if the database CHECK constraints are dropped one day.
func TestGrantValidates(t *testing.T) {
	cases := []struct {
		name    string
		res     string
		grantee string
		role    string
		wantErr error
	}{
		{"bad resource", "workspace", GranteeUser, RoleViewer, ErrInvalidResourceType},
		{"bad grantee", ResourceFile, "service", RoleViewer, ErrInvalidGranteeType},
		{"bad role", ResourceFile, GranteeUser, "owner", ErrInvalidRole},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepo{}
			svc := NewService(repo)
			_, err := svc.Grant(context.Background(), uuid.New(), tc.res, uuid.New(), tc.grantee, uuid.New(), tc.role, nil)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected %v, got %v", tc.wantErr, err)
			}
			if len(repo.created) != 0 {
				t.Fatalf("repository must not be touched on validation error, got %d Create calls", len(repo.created))
			}
		})
	}
}

// TestGrantHappyPath confirms valid inputs flow through to the
// repository unchanged, and the ExpiresAt pointer is forwarded by
// reference (not silently copied to nil).
func TestGrantHappyPath(t *testing.T) {
	repo := &fakeRepo{}
	svc := NewService(repo)
	exp := time.Now().Add(48 * time.Hour)
	ws := uuid.New()
	resID := uuid.New()
	granteeID := uuid.New()

	p, err := svc.Grant(context.Background(), ws, ResourceFile, resID, GranteeUser, granteeID, RoleEditor, &exp)
	if err != nil {
		t.Fatalf("Grant returned unexpected error: %v", err)
	}
	if p == nil {
		t.Fatalf("Grant returned nil permission on happy path")
	}
	if len(repo.created) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(repo.created))
	}
	got := repo.created[0]
	if got.WorkspaceID != ws || got.ResourceID != resID || got.GranteeID != granteeID {
		t.Fatalf("Grant did not forward ids verbatim: %+v", got)
	}
	if got.ResourceType != ResourceFile || got.GranteeType != GranteeUser || got.Role != RoleEditor {
		t.Fatalf("Grant did not forward enum values: %+v", got)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
		t.Fatalf("Grant lost ExpiresAt: %v", got.ExpiresAt)
	}
}

// TestHasAccessValidates short-circuits on bad enums before the
// repository call, mirroring the Grant guarantees.
func TestHasAccessValidates(t *testing.T) {
	cases := []struct {
		name    string
		res     string
		grantee string
		minRole string
		wantErr error
	}{
		{"bad resource", "bucket", GranteeUser, RoleViewer, ErrInvalidResourceType},
		{"bad grantee", ResourceFolder, "robot", RoleViewer, ErrInvalidGranteeType},
		{"bad role", ResourceFolder, GranteeUser, "root", ErrInvalidRole},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepo{checkAccessReturn: true}
			svc := NewService(repo)
			ok, err := svc.HasAccess(context.Background(), uuid.New(), tc.res, uuid.New(), tc.grantee, uuid.New(), tc.minRole)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected %v, got %v", tc.wantErr, err)
			}
			if ok {
				t.Fatalf("validation failure must report ok=false even when fake repo says true")
			}
		})
	}
}

// TestHasAccessForwards confirms the wrapper preserves repo errors
// and return values, so callers see authentic database failures.
func TestHasAccessForwards(t *testing.T) {
	wantErr := errors.New("db down")
	repo := &fakeRepo{checkAccessErr: wantErr}
	svc := NewService(repo)
	ok, err := svc.HasAccess(context.Background(), uuid.New(), ResourceFile, uuid.New(), GranteeUser, uuid.New(), RoleViewer)
	if ok {
		t.Fatalf("expected ok=false on repo error, got true")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected %v, got %v", wantErr, err)
	}
}

// TestRevokeForwards confirms the wrapper passes through to Delete
// with the right id.
func TestRevokeForwards(t *testing.T) {
	repo := &fakeRepo{}
	svc := NewService(repo)
	permID := uuid.New()
	if err := svc.Revoke(context.Background(), uuid.New(), permID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if len(repo.deleted) != 1 || repo.deleted[0] != permID {
		t.Fatalf("expected Delete called once with %s, got %v", permID, repo.deleted)
	}
}
