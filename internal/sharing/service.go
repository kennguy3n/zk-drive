package sharing

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// Errors returned by Service methods. Handlers translate these into HTTP
// status codes; see api/drive/handler.go for the mapping.
var (
	ErrInvalidResourceType = errors.New("sharing: invalid resource type")
	ErrInvalidRole         = errors.New("sharing: invalid role")
	ErrInvalidEmail        = errors.New("sharing: invalid email")
	ErrLinkExpired         = errors.New("sharing: share link has expired")
	ErrLinkExhausted       = errors.New("sharing: share link download cap reached")
	ErrPasswordRequired    = errors.New("sharing: share link requires a password")
	ErrPasswordIncorrect   = errors.New("sharing: incorrect password for share link")
	ErrInviteExpired       = errors.New("sharing: guest invite has expired")
	ErrInviteAlreadyUsed   = errors.New("sharing: guest invite already accepted")
)

// PermissionGranter is the minimal surface the sharing service needs
// from the permission service to wire a guest invite into the permission
// system. We take an interface rather than the concrete *permission.Service
// to avoid a package-level import cycle and to keep tests able to stub
// the permission layer.
type PermissionGranter interface {
	Grant(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, granteeType string, granteeID uuid.UUID, role string, expiresAt *time.Time) (PermissionRef, error)
	Revoke(ctx context.Context, workspaceID, permID uuid.UUID) error
}

// PermissionRef is the subset of a permission.Permission the sharing
// service cares about: just the id, which it stores on the invite so it
// can revoke the grant when the invite is deleted.
type PermissionRef struct {
	ID uuid.UUID
}

// Service wraps the sharing repository with validation, token / password
// handling, and coordination with the permission service for guest
// invites.
type Service struct {
	repo        Repository
	permissions PermissionGranter
	now         func() time.Time
}

// NewService returns a Service backed by the given repository and
// permission granter. The permission granter may be nil in tests that
// only exercise share-link flows.
func NewService(repo Repository, perms PermissionGranter) *Service {
	return &Service{repo: repo, permissions: perms, now: time.Now}
}

// SetClock overrides the wall-clock source. Intended for tests.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// tokenBytes determines the entropy of a share-link token; 32 bytes of
// base64url (~43 chars) keeps tokens guess-resistant while remaining
// URL-safe and easy to type / share.
const tokenBytes = 32

func generateToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// CreateShareLink creates a share link for a resource. password is
// optional: when non-empty it is hashed with bcrypt and resolution
// callers must supply the correct plaintext password. expiresAt and
// maxDownloads are optional caps.
func (s *Service) CreateShareLink(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, password string, expiresAt *time.Time, maxDownloads *int, createdBy uuid.UUID) (*ShareLink, error) {
	if !IsValidResourceType(resourceType) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidResourceType, resourceType)
	}
	if maxDownloads != nil && *maxDownloads <= 0 {
		return nil, fmt.Errorf("sharing: max_downloads must be > 0")
	}
	token, err := generateToken()
	if err != nil {
		return nil, err
	}
	link := &ShareLink{
		WorkspaceID:  workspaceID,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Token:        token,
		ExpiresAt:    expiresAt,
		MaxDownloads: maxDownloads,
		CreatedBy:    createdBy,
	}
	if password != "" {
		hash, herr := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if herr != nil {
			return nil, fmt.Errorf("hash password: %w", herr)
		}
		hs := string(hash)
		link.PasswordHash = &hs
	}
	if err := s.repo.CreateShareLink(ctx, link); err != nil {
		return nil, err
	}
	return link, nil
}

// ResolveShareLink fetches the link by token, validates expiry /
// password / download cap, bumps the download count, and returns the
// validated link so the caller can build a download URL. Returning the
// full link keeps the HTTP layer simple and lets tests inspect the
// metadata.
func (s *Service) ResolveShareLink(ctx context.Context, token string, password string) (*ShareLink, error) {
	link, err := s.repo.GetShareLinkByToken(ctx, token)
	if err != nil {
		return nil, err
	}
	if link.IsExpired(s.now()) {
		return nil, ErrLinkExpired
	}
	// Max-downloads enforcement lives inside IncrementDownloadCount so
	// the check and the increment happen atomically. Checking
	// link.IsExhausted() here on a cached snapshot would reintroduce a
	// TOCTOU race where two concurrent callers on a link with
	// max_downloads=1 could both pass the check and both succeed.
	if link.RequiresPassword() {
		if password == "" {
			return nil, ErrPasswordRequired
		}
		if err := bcrypt.CompareHashAndPassword([]byte(*link.PasswordHash), []byte(password)); err != nil {
			return nil, ErrPasswordIncorrect
		}
	}
	if err := s.repo.IncrementDownloadCount(ctx, link.ID); err != nil {
		return nil, err
	}
	link.DownloadCount++
	return link, nil
}

// GetShareLinkByID fetches a share link by id scoped to workspace.
// Callers use this to display or manage existing links in the UI.
func (s *Service) GetShareLinkByID(ctx context.Context, workspaceID, id uuid.UUID) (*ShareLink, error) {
	return s.repo.GetShareLinkByID(ctx, workspaceID, id)
}

// RevokeShareLink deletes a share link. Idempotent beyond ErrNotFound.
func (s *Service) RevokeShareLink(ctx context.Context, workspaceID, id uuid.UUID) error {
	return s.repo.DeleteShareLink(ctx, workspaceID, id)
}

// CreateGuestInvite creates a guest invite and the matching permission
// grant on the folder (grantee_type=guest, expires_at carried through).
// The invite and the grant share the same expires_at so the retention
// worker (Phase 2 §7.4) revokes both on the same schedule.
//
// role must be one of "viewer" | "editor" | "admin"; email must be a
// valid address per net/mail.ParseAddress.
func (s *Service) CreateGuestInvite(ctx context.Context, workspaceID uuid.UUID, email string, folderID uuid.UUID, role string, expiresAt *time.Time, createdBy uuid.UUID) (*GuestInvite, error) {
	email = strings.TrimSpace(email)
	if _, err := mail.ParseAddress(email); err != nil {
		return nil, fmt.Errorf("%w: %q", ErrInvalidEmail, email)
	}
	if !isValidRole(role) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidRole, role)
	}
	if s.permissions == nil {
		return nil, fmt.Errorf("sharing: permission granter not configured")
	}
	// Each guest invite is identified on the permission side by a
	// synthetic grantee_id — a namespaced UUID v5 over the email so the
	// same guest email is deterministic across multiple invites in a
	// workspace. grantee_type is fixed to 'guest'.
	granteeID := guestGranteeID(workspaceID, email)
	perm, err := s.permissions.Grant(ctx, workspaceID, ResourceFolder, folderID, "guest", granteeID, role, expiresAt)
	if err != nil {
		return nil, err
	}
	invite := &GuestInvite{
		WorkspaceID:  workspaceID,
		Email:        email,
		FolderID:     folderID,
		Role:         role,
		ExpiresAt:    expiresAt,
		PermissionID: perm.ID,
		CreatedBy:    createdBy,
	}
	if err := s.repo.CreateGuestInvite(ctx, invite); err != nil {
		// Best-effort compensation: roll back the grant we just created.
		// A failed revoke here is logged at the caller layer; we prefer
		// surfacing the original insert error.
		_ = s.permissions.Revoke(ctx, workspaceID, perm.ID)
		return nil, err
	}
	return invite, nil
}

// AcceptGuestInvite marks an invite accepted. Expired or already-
// accepted invites are rejected so the frontend can surface the right
// error to the invitee.
func (s *Service) AcceptGuestInvite(ctx context.Context, workspaceID, id uuid.UUID) (*GuestInvite, error) {
	inv, err := s.repo.GetGuestInviteByID(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if inv.ExpiresAt != nil && !inv.ExpiresAt.After(now) {
		return nil, ErrInviteExpired
	}
	if inv.AcceptedAt != nil {
		return nil, ErrInviteAlreadyUsed
	}
	if err := s.repo.AcceptGuestInvite(ctx, workspaceID, id, now); err != nil {
		return nil, err
	}
	inv.AcceptedAt = &now
	return inv, nil
}

// RevokeGuestInvite deletes the invite and the matching permission
// grant. Missing rows are swallowed so the endpoint is idempotent
// except for the initial lookup.
func (s *Service) RevokeGuestInvite(ctx context.Context, workspaceID, id uuid.UUID) error {
	inv, err := s.repo.GetGuestInviteByID(ctx, workspaceID, id)
	if err != nil {
		return err
	}
	if s.permissions != nil && inv.PermissionID != uuid.Nil {
		if rerr := s.permissions.Revoke(ctx, workspaceID, inv.PermissionID); rerr != nil && !errors.Is(rerr, ErrNotFound) {
			// The permission service returns its own not-found error; we
			// can't import the permission package here so we swallow any
			// error when revoking and rely on the caller's logs.
			_ = rerr
		}
	}
	return s.repo.DeleteGuestInvite(ctx, workspaceID, id)
}

// GetGuestInviteByID is a thin pass-through used by handlers that need
// to look up an invite before confirming / revoking it.
func (s *Service) GetGuestInviteByID(ctx context.Context, workspaceID, id uuid.UUID) (*GuestInvite, error) {
	return s.repo.GetGuestInviteByID(ctx, workspaceID, id)
}

// ListGuestInvitesByFolder returns all invites for a folder, newest
// first.
func (s *Service) ListGuestInvitesByFolder(ctx context.Context, workspaceID, folderID uuid.UUID) ([]*GuestInvite, error) {
	return s.repo.ListGuestInvitesByFolder(ctx, workspaceID, folderID)
}

// ExpireGuestAccess deletes expired guest permission rows. Called by
// the periodic sweep in the worker binary. Returns the count of
// permissions revoked so the caller can log useful telemetry.
func (s *Service) ExpireGuestAccess(ctx context.Context, now time.Time) (int, error) {
	return s.repo.ExpireGuestPermissions(ctx, now)
}

// guestGranteeID derives a deterministic UUID v5 for a guest email
// within a workspace. Using v5 (name-based) means two invites to the
// same email in the same workspace always reuse the same grantee_id, so
// multiple invites can be enforced by the permission layer's unique
// constraint if we add one later.
func guestGranteeID(workspaceID uuid.UUID, email string) uuid.UUID {
	ns := uuid.NewSHA1(uuid.Nil, []byte("zk-drive:guest"))
	return uuid.NewSHA1(ns, []byte(workspaceID.String()+"|"+strings.ToLower(email)))
}

func isValidRole(role string) bool {
	switch role {
	case "viewer", "editor", "admin":
		return true
	default:
		return false
	}
}
