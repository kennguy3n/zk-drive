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
