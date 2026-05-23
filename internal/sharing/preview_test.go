package sharing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeRepo implements just enough of Repository for the preview
// path. Methods we don't exercise return ErrNotImplemented so a
// future code change that accidentally takes a different code
// path through the service crashes loudly in tests instead of
// silently passing.
type fakeRepo struct {
	invite *GuestInvite
	err    error
}

var errNotImplFake = errors.New("fakeRepo: not implemented")

func (f *fakeRepo) GetGuestInviteByIDAnyWorkspace(_ context.Context, _ uuid.UUID) (*GuestInvite, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.invite, nil
}

func (f *fakeRepo) CreateShareLink(context.Context, *ShareLink) error { return errNotImplFake }
func (f *fakeRepo) GetShareLinkByToken(context.Context, string) (*ShareLink, error) {
	return nil, errNotImplFake
}
func (f *fakeRepo) GetShareLinkByID(context.Context, uuid.UUID, uuid.UUID) (*ShareLink, error) {
	return nil, errNotImplFake
}
func (f *fakeRepo) DeleteShareLink(context.Context, uuid.UUID, uuid.UUID) error {
	return errNotImplFake
}
func (f *fakeRepo) IncrementDownloadCount(context.Context, uuid.UUID) error { return errNotImplFake }
func (f *fakeRepo) CreateGuestInvite(context.Context, *GuestInvite) error   { return errNotImplFake }
func (f *fakeRepo) GetGuestInviteByID(context.Context, uuid.UUID, uuid.UUID) (*GuestInvite, error) {
	return nil, errNotImplFake
}
func (f *fakeRepo) ListGuestInvitesByFolder(context.Context, uuid.UUID, uuid.UUID) ([]*GuestInvite, error) {
	return nil, errNotImplFake
}
func (f *fakeRepo) AcceptGuestInvite(context.Context, uuid.UUID, uuid.UUID, time.Time) error {
	return errNotImplFake
}
func (f *fakeRepo) DeleteGuestInvite(context.Context, uuid.UUID, uuid.UUID) error {
	return errNotImplFake
}
func (f *fakeRepo) ExpireGuestPermissions(context.Context, time.Time) (int, error) {
	return 0, errNotImplFake
}

// nopGranter satisfies PermissionGranter without exercising the
// permission package; the preview path never invokes it.
type nopGranter struct{}

func (nopGranter) Grant(context.Context, uuid.UUID, string, uuid.UUID, string, uuid.UUID, string, *time.Time) (PermissionRef, error) {
	return PermissionRef{}, errNotImplFake
}
func (nopGranter) Revoke(context.Context, uuid.UUID, uuid.UUID) error { return errNotImplFake }

func newPreviewService(t *testing.T, repo Repository, now time.Time) *Service {
	t.Helper()
	svc := NewService(repo, nopGranter{})
	svc.SetClock(func() time.Time { return now })
	return svc
}

// TestGetGuestInvitePreview_StatusPending pins the happy path:
// an invite with no AcceptedAt and a future ExpiresAt is "pending".
func TestGetGuestInvitePreview_StatusPending(t *testing.T) {
	now := time.Date(2025, 11, 1, 12, 0, 0, 0, time.UTC)
	future := now.Add(7 * 24 * time.Hour)
	repo := &fakeRepo{invite: &GuestInvite{
		ID:          uuid.New(),
		WorkspaceID: uuid.New(),
		FolderID:    uuid.New(),
		Email:       "alice@external.com",
		Role:        "editor",
		ExpiresAt:   &future,
	}}
	svc := newPreviewService(t, repo, now)
	got, err := svc.GetGuestInvitePreview(context.Background(), repo.invite.ID)
	if err != nil {
		t.Fatalf("GetGuestInvitePreview: %v", err)
	}
	if got.Status != GuestInviteStatusPending {
		t.Errorf("status = %q, want %q", got.Status, GuestInviteStatusPending)
	}
}

// TestGetGuestInvitePreview_StatusExpired pins that an invite past
// its ExpiresAt with no AcceptedAt surfaces as "expired" so the
// frontend can render "this invite expired" instead of offering
// the accept flow (which would 410 anyway).
func TestGetGuestInvitePreview_StatusExpired(t *testing.T) {
	now := time.Date(2025, 11, 1, 12, 0, 0, 0, time.UTC)
	past := now.Add(-1 * time.Hour)
	repo := &fakeRepo{invite: &GuestInvite{
		ID:          uuid.New(),
		WorkspaceID: uuid.New(),
		FolderID:    uuid.New(),
		Email:       "alice@external.com",
		Role:        "viewer",
		ExpiresAt:   &past,
	}}
	svc := newPreviewService(t, repo, now)
	got, err := svc.GetGuestInvitePreview(context.Background(), repo.invite.ID)
	if err != nil {
		t.Fatalf("GetGuestInvitePreview: %v", err)
	}
	if got.Status != GuestInviteStatusExpired {
		t.Errorf("status = %q, want %q", got.Status, GuestInviteStatusExpired)
	}
}

// TestGetGuestInvitePreview_StatusAcceptedTrumpsExpiry pins the
// precedence rule: an invite that was redeemed BEFORE expiry must
// surface as "accepted" even if the wall-clock now is past
// ExpiresAt. The audit log already records the accept; the
// preview must match.
func TestGetGuestInvitePreview_StatusAcceptedTrumpsExpiry(t *testing.T) {
	now := time.Date(2025, 11, 1, 12, 0, 0, 0, time.UTC)
	past := now.Add(-1 * time.Hour)
	accepted := now.Add(-2 * time.Hour)
	repo := &fakeRepo{invite: &GuestInvite{
		ID:          uuid.New(),
		WorkspaceID: uuid.New(),
		FolderID:    uuid.New(),
		Email:       "alice@external.com",
		Role:        "editor",
		ExpiresAt:   &past,
		AcceptedAt:  &accepted,
	}}
	svc := newPreviewService(t, repo, now)
	got, err := svc.GetGuestInvitePreview(context.Background(), repo.invite.ID)
	if err != nil {
		t.Fatalf("GetGuestInvitePreview: %v", err)
	}
	if got.Status != GuestInviteStatusAccepted {
		t.Errorf("status = %q, want %q", got.Status, GuestInviteStatusAccepted)
	}
}

// TestResolveWorkspaceName_FallbackWhenResolverNil pins the
// nil-safe contract: when WithDisplayResolvers is never called,
// the accessor returns the fallback string and does NOT panic.
// Same posture as the email-dispatch path needs, since dev /
// metadata-only modes don't wire the resolver.
func TestResolveWorkspaceName_FallbackWhenResolverNil(t *testing.T) {
	svc := NewService(&fakeRepo{}, nopGranter{})
	got := svc.ResolveWorkspaceName(context.Background(), uuid.New())
	if got != "your workspace" {
		t.Errorf("ResolveWorkspaceName fallback = %q, want %q", got, "your workspace")
	}
	gotF := svc.ResolveFolderName(context.Background(), uuid.New(), uuid.New())
	if gotF != "a shared folder" {
		t.Errorf("ResolveFolderName fallback = %q, want %q", gotF, "a shared folder")
	}
}

// TestResolveWorkspaceName_ResolverErrorFallsBack pins the error
// contract: a resolver that returns (name, err) with err != nil
// falls back to the placeholder string rather than propagating
// the error. The email-dispatch path is best-effort and must
// never block on a transient workspace-service hiccup.
func TestResolveWorkspaceName_ResolverErrorFallsBack(t *testing.T) {
	wantErr := errors.New("workspace service unreachable")
	svc := NewService(&fakeRepo{}, nopGranter{}).WithDisplayResolvers(
		func(context.Context, uuid.UUID) (string, error) { return "", wantErr },
		func(context.Context, uuid.UUID, uuid.UUID) (string, error) { return "", wantErr },
	)
	if got := svc.ResolveWorkspaceName(context.Background(), uuid.New()); got != "your workspace" {
		t.Errorf("ResolveWorkspaceName on err = %q, want fallback", got)
	}
	if got := svc.ResolveFolderName(context.Background(), uuid.New(), uuid.New()); got != "a shared folder" {
		t.Errorf("ResolveFolderName on err = %q, want fallback", got)
	}
}

// TestResolveWorkspaceName_HappyPath pins that a successful
// resolver result is returned verbatim.
func TestResolveWorkspaceName_HappyPath(t *testing.T) {
	svc := NewService(&fakeRepo{}, nopGranter{}).WithDisplayResolvers(
		func(context.Context, uuid.UUID) (string, error) { return "Acme Co", nil },
		func(context.Context, uuid.UUID, uuid.UUID) (string, error) { return "Q4 Roadmap", nil },
	)
	if got := svc.ResolveWorkspaceName(context.Background(), uuid.New()); got != "Acme Co" {
		t.Errorf("ResolveWorkspaceName = %q, want %q", got, "Acme Co")
	}
	if got := svc.ResolveFolderName(context.Background(), uuid.New(), uuid.New()); got != "Q4 Roadmap" {
		t.Errorf("ResolveFolderName = %q, want %q", got, "Q4 Roadmap")
	}
}
