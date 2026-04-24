package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/sharing"
)

// createShareLinkPayload is a convenience struct mirroring
// api/drive.createShareLinkRequest so tests can build request bodies
// without depending on the unexported API-package type.
type createShareLinkPayload struct {
	ResourceType string  `json:"resource_type"`
	ResourceID   string  `json:"resource_id"`
	Password     string  `json:"password,omitempty"`
	ExpiresAt    *string `json:"expires_at,omitempty"`
	MaxDownloads *int    `json:"max_downloads,omitempty"`
}

// createGuestInvitePayload mirrors the inner request body of
// POST /api/guest-invites.
type createGuestInvitePayload struct {
	Email     string  `json:"email"`
	FolderID  string  `json:"folder_id"`
	Role      string  `json:"role"`
	ExpiresAt *string `json:"expires_at,omitempty"`
}

// expireShareLinkRow reaches into Postgres to move a link's expires_at
// into the past. Triggering the normal service with a past-dated
// expires_at is rejected by business logic, so tests that need an
// already-expired row mutate it directly after creation.
func (e *testEnv) expireShareLinkRow(id uuid.UUID, at time.Time) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := e.pool.Exec(ctx,
		`UPDATE share_links SET expires_at = $2 WHERE id = $1`,
		id, at,
	); err != nil {
		e.t.Fatalf("expire share_links row: %v", err)
	}
}

func (e *testEnv) expireGuestInviteRow(id uuid.UUID, at time.Time) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := e.pool.Exec(ctx,
		`UPDATE guest_invites SET expires_at = $2 WHERE id = $1`,
		id, at,
	); err != nil {
		e.t.Fatalf("expire guest_invites row: %v", err)
	}
}

func TestCreateAndResolveShareLink(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	f := createFolder(t, env, tok.Token, nil, "Shared")

	status, body := env.httpRequest(http.MethodPost, "/api/share-links", tok.Token, createShareLinkPayload{
		ResourceType: sharing.ResourceFolder,
		ResourceID:   f.ID.String(),
	})
	if status != http.StatusCreated {
		t.Fatalf("create share link: status=%d body=%s", status, string(body))
	}
	var link sharing.ShareLink
	env.decodeJSON(body, &link)
	if link.Token == "" {
		t.Fatalf("expected non-empty token, got %q", link.Token)
	}
	if link.ResourceID != f.ID || link.ResourceType != sharing.ResourceFolder {
		t.Errorf("link metadata mismatch: %+v", link)
	}
	if link.DownloadCount != 0 {
		t.Errorf("expected 0 downloads, got %d", link.DownloadCount)
	}

	// Anonymous resolve (no auth header) must succeed and bump download_count.
	status, body = env.httpRequest(http.MethodGet, "/api/share-links/"+link.Token, "", nil)
	if status != http.StatusOK {
		t.Fatalf("resolve: status=%d body=%s", status, string(body))
	}
	var resolved sharing.ShareLink
	env.decodeJSON(body, &resolved)
	if resolved.DownloadCount != 1 {
		t.Errorf("expected download_count=1 after resolve, got %d", resolved.DownloadCount)
	}
	if resolved.ID != link.ID {
		t.Errorf("resolve returned different link id: %v vs %v", resolved.ID, link.ID)
	}
}

func TestResolveUnknownShareLinkReturns404(t *testing.T) {
	env := setupEnv(t)
	status, _ := env.httpRequest(http.MethodGet, "/api/share-links/doesnotexist", "", nil)
	if status != http.StatusNotFound {
		t.Errorf("expected 404 for unknown token, got %d", status)
	}
}

func TestPasswordProtectedShareLink(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	f := createFolder(t, env, tok.Token, nil, "Secret")

	status, body := env.httpRequest(http.MethodPost, "/api/share-links", tok.Token, createShareLinkPayload{
		ResourceType: sharing.ResourceFolder,
		ResourceID:   f.ID.String(),
		Password:     "hunter2",
	})
	if status != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", status, string(body))
	}
	var link sharing.ShareLink
	env.decodeJSON(body, &link)

	// GET without password => 401 (password required).
	status, _ = env.httpRequest(http.MethodGet, "/api/share-links/"+link.Token, "", nil)
	if status != http.StatusUnauthorized {
		t.Errorf("expected 401 for password-required GET, got %d", status)
	}

	// POST with wrong password => 403 (password incorrect).
	status, _ = env.httpRequest(http.MethodPost, "/api/share-links/"+link.Token, "", map[string]string{
		"password": "wrong",
	})
	if status != http.StatusForbidden {
		t.Errorf("expected 403 for wrong password, got %d", status)
	}

	// POST with correct password => 200.
	status, body = env.httpRequest(http.MethodPost, "/api/share-links/"+link.Token, "", map[string]string{
		"password": "hunter2",
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200 for correct password, got %d body=%s", status, string(body))
	}
}

func TestExpiredShareLinkRejected(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	f := createFolder(t, env, tok.Token, nil, "Ephemeral")

	// Create a link with a future expiry so the service accepts it; then
	// rewrite expires_at to a past timestamp to simulate elapsed time.
	future := time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)
	status, body := env.httpRequest(http.MethodPost, "/api/share-links", tok.Token, createShareLinkPayload{
		ResourceType: sharing.ResourceFolder,
		ResourceID:   f.ID.String(),
		ExpiresAt:    &future,
	})
	if status != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", status, string(body))
	}
	var link sharing.ShareLink
	env.decodeJSON(body, &link)

	env.expireShareLinkRow(link.ID, time.Now().UTC().Add(-1*time.Hour))

	status, _ = env.httpRequest(http.MethodGet, "/api/share-links/"+link.Token, "", nil)
	if status != http.StatusGone {
		t.Errorf("expected 410 for expired link, got %d", status)
	}
}

func TestMaxDownloadsEnforced(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	f := createFolder(t, env, tok.Token, nil, "Limited")

	max := 2
	status, body := env.httpRequest(http.MethodPost, "/api/share-links", tok.Token, createShareLinkPayload{
		ResourceType: sharing.ResourceFolder,
		ResourceID:   f.ID.String(),
		MaxDownloads: &max,
	})
	if status != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", status, string(body))
	}
	var link sharing.ShareLink
	env.decodeJSON(body, &link)

	for i := 0; i < max; i++ {
		status, _ := env.httpRequest(http.MethodGet, "/api/share-links/"+link.Token, "", nil)
		if status != http.StatusOK {
			t.Fatalf("resolve #%d: expected 200, got %d", i+1, status)
		}
	}
	// The (max+1)th resolve must be rejected with 429 (too many requests
	// stands in for "cap reached" here — callers parse the error name to
	// distinguish).
	status, _ = env.httpRequest(http.MethodGet, "/api/share-links/"+link.Token, "", nil)
	if status != http.StatusTooManyRequests {
		t.Errorf("expected 429 after max downloads, got %d", status)
	}
}

// TestMaxDownloadsSingleUseAtomic pins the TOCTOU fix: a link with
// max_downloads=1 must allow exactly one resolve and reject every
// subsequent call with 429 Too Many Requests, even when the handler
// gets there before the client observes the updated count. Before the
// atomic UPDATE guard, the service checked link.IsExhausted() on a
// cached snapshot and a race between two concurrent resolutions could
// both pass the check. We can't easily drive true concurrency from the
// integration harness, but we can assert the single-use invariant: the
// first resolve succeeds, the second fails with 429, and
// download_count never advances past 1.
func TestMaxDownloadsSingleUseAtomic(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	f := createFolder(t, env, tok.Token, nil, "OneShot")

	one := 1
	status, body := env.httpRequest(http.MethodPost, "/api/share-links", tok.Token, createShareLinkPayload{
		ResourceType: sharing.ResourceFolder,
		ResourceID:   f.ID.String(),
		MaxDownloads: &one,
	})
	if status != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", status, string(body))
	}
	var link sharing.ShareLink
	env.decodeJSON(body, &link)

	status, body = env.httpRequest(http.MethodGet, "/api/share-links/"+link.Token, "", nil)
	if status != http.StatusOK {
		t.Fatalf("first resolve: expected 200, got %d body=%s", status, string(body))
	}
	var resolved sharing.ShareLink
	env.decodeJSON(body, &resolved)
	if resolved.DownloadCount != 1 {
		t.Errorf("expected download_count=1, got %d", resolved.DownloadCount)
	}

	status, _ = env.httpRequest(http.MethodGet, "/api/share-links/"+link.Token, "", nil)
	if status != http.StatusTooManyRequests {
		t.Errorf("expected 429 on second resolve, got %d", status)
	}

	// download_count must stay pinned at 1 — the atomic UPDATE guard
	// rejects increments once the cap is hit, so even repeated failed
	// resolves cannot nudge the counter upward.
	var finalCount int
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := env.pool.QueryRow(ctx, `SELECT download_count FROM share_links WHERE id = $1`, link.ID).Scan(&finalCount); err != nil {
		t.Fatalf("read final download_count: %v", err)
	}
	if finalCount != 1 {
		t.Errorf("expected download_count to stay at 1, got %d", finalCount)
	}
}

func TestRevokeShareLink(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	f := createFolder(t, env, tok.Token, nil, "Shared")

	status, body := env.httpRequest(http.MethodPost, "/api/share-links", tok.Token, createShareLinkPayload{
		ResourceType: sharing.ResourceFolder,
		ResourceID:   f.ID.String(),
	})
	if status != http.StatusCreated {
		t.Fatalf("create: %d body=%s", status, string(body))
	}
	var link sharing.ShareLink
	env.decodeJSON(body, &link)

	status, _ = env.httpRequest(http.MethodDelete, "/api/share-links/"+link.ID.String(), tok.Token, nil)
	if status != http.StatusNoContent {
		t.Fatalf("revoke: %d", status)
	}

	// Second revoke returns 404.
	status, _ = env.httpRequest(http.MethodDelete, "/api/share-links/"+link.ID.String(), tok.Token, nil)
	if status != http.StatusNotFound {
		t.Errorf("expected 404 on second revoke, got %d", status)
	}

	// Resolving the revoked token must also 404.
	status, _ = env.httpRequest(http.MethodGet, "/api/share-links/"+link.Token, "", nil)
	if status != http.StatusNotFound {
		t.Errorf("expected 404 resolving revoked token, got %d", status)
	}
}

func TestShareLinkCrossTenantRevokeRejected(t *testing.T) {
	env := setupEnv(t)
	alice := env.signupAndLogin("Acme", "alice@acme.test", "Alice", "pw1")
	bob := env.signupAndLogin("Globex", "bob@globex.test", "Bob", "pw2")

	f := createFolder(t, env, alice.Token, nil, "AliceFolder")
	status, body := env.httpRequest(http.MethodPost, "/api/share-links", alice.Token, createShareLinkPayload{
		ResourceType: sharing.ResourceFolder,
		ResourceID:   f.ID.String(),
	})
	if status != http.StatusCreated {
		t.Fatalf("create: %d body=%s", status, string(body))
	}
	var link sharing.ShareLink
	env.decodeJSON(body, &link)

	// Bob — admin in his own workspace but not Alice's — cannot revoke by
	// id; workspace scoping means the row is invisible to him.
	status, _ = env.httpRequest(http.MethodDelete, "/api/share-links/"+link.ID.String(), bob.Token, nil)
	if status != http.StatusNotFound {
		t.Errorf("expected 404 for cross-tenant revoke, got %d", status)
	}

	// The public resolve endpoint is workspace-agnostic by design (a
	// token-only URL is the point); confirm it still returns the link so
	// we haven't accidentally regressed that behaviour.
	status, _ = env.httpRequest(http.MethodGet, "/api/share-links/"+link.Token, "", nil)
	if status != http.StatusOK {
		t.Errorf("expected 200 resolving alice's token, got %d", status)
	}
}

func TestCreateAndAcceptGuestInvite(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	f := createFolder(t, env, tok.Token, nil, "Invited")

	status, body := env.httpRequest(http.MethodPost, "/api/guest-invites", tok.Token, createGuestInvitePayload{
		Email:    "guest@example.test",
		FolderID: f.ID.String(),
		Role:     "viewer",
	})
	if status != http.StatusCreated {
		t.Fatalf("create invite: status=%d body=%s", status, string(body))
	}
	var inv sharing.GuestInvite
	env.decodeJSON(body, &inv)
	if inv.PermissionID == uuid.Nil {
		t.Errorf("expected permission_id to be populated on invite")
	}
	if inv.AcceptedAt != nil {
		t.Errorf("invite should not be accepted at creation time")
	}

	// Accept with the creator's token (integration tests don't exercise
	// the guest-handshake flow; any authenticated caller in the invite's
	// workspace can accept in this test setup).
	status, body = env.httpRequest(http.MethodPost, "/api/guest-invites/"+inv.ID.String()+"/accept", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("accept: status=%d body=%s", status, string(body))
	}
	var accepted sharing.GuestInvite
	env.decodeJSON(body, &accepted)
	if accepted.AcceptedAt == nil {
		t.Errorf("expected accepted_at to be set")
	}

	// Second accept returns 409 (already used).
	status, _ = env.httpRequest(http.MethodPost, "/api/guest-invites/"+inv.ID.String()+"/accept", tok.Token, nil)
	if status != http.StatusConflict {
		t.Errorf("expected 409 on second accept, got %d", status)
	}
}

func TestExpiredGuestInviteRejected(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	f := createFolder(t, env, tok.Token, nil, "Expired")

	future := time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)
	status, body := env.httpRequest(http.MethodPost, "/api/guest-invites", tok.Token, createGuestInvitePayload{
		Email:     "guest@example.test",
		FolderID:  f.ID.String(),
		Role:      "viewer",
		ExpiresAt: &future,
	})
	if status != http.StatusCreated {
		t.Fatalf("create invite: status=%d body=%s", status, string(body))
	}
	var inv sharing.GuestInvite
	env.decodeJSON(body, &inv)

	env.expireGuestInviteRow(inv.ID, time.Now().UTC().Add(-1*time.Hour))

	status, _ = env.httpRequest(http.MethodPost, "/api/guest-invites/"+inv.ID.String()+"/accept", tok.Token, nil)
	if status != http.StatusGone {
		t.Errorf("expected 410 for expired invite, got %d", status)
	}
}

func TestRevokeGuestInvite(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	f := createFolder(t, env, tok.Token, nil, "Revokable")

	status, body := env.httpRequest(http.MethodPost, "/api/guest-invites", tok.Token, createGuestInvitePayload{
		Email:    "guest@example.test",
		FolderID: f.ID.String(),
		Role:     "editor",
	})
	if status != http.StatusCreated {
		t.Fatalf("create: %d body=%s", status, string(body))
	}
	var inv sharing.GuestInvite
	env.decodeJSON(body, &inv)

	status, _ = env.httpRequest(http.MethodDelete, "/api/guest-invites/"+inv.ID.String(), tok.Token, nil)
	if status != http.StatusNoContent {
		t.Fatalf("revoke: %d", status)
	}

	// Second revoke returns 404.
	status, _ = env.httpRequest(http.MethodDelete, "/api/guest-invites/"+inv.ID.String(), tok.Token, nil)
	if status != http.StatusNotFound {
		t.Errorf("expected 404 on second revoke, got %d", status)
	}
}

func TestGuestInviteCrossTenantIsolation(t *testing.T) {
	env := setupEnv(t)
	alice := env.signupAndLogin("Acme", "alice@acme.test", "Alice", "pw1")
	bob := env.signupAndLogin("Globex", "bob@globex.test", "Bob", "pw2")

	aliceFolder := createFolder(t, env, alice.Token, nil, "AliceFolder")

	// Bob cannot create an invite against Alice's folder.
	status, _ := env.httpRequest(http.MethodPost, "/api/guest-invites", bob.Token, createGuestInvitePayload{
		Email:    "guest@example.test",
		FolderID: aliceFolder.ID.String(),
		Role:     "viewer",
	})
	if status != http.StatusNotFound {
		t.Errorf("expected 404 cross-tenant invite create, got %d", status)
	}

	// Alice creates a legitimate invite.
	status, body := env.httpRequest(http.MethodPost, "/api/guest-invites", alice.Token, createGuestInvitePayload{
		Email:    "guest@example.test",
		FolderID: aliceFolder.ID.String(),
		Role:     "viewer",
	})
	if status != http.StatusCreated {
		t.Fatalf("alice create: %d body=%s", status, string(body))
	}
	var inv sharing.GuestInvite
	env.decodeJSON(body, &inv)

	// Bob cannot accept or revoke Alice's invite (404, not 403, because
	// the row isn't visible in Bob's workspace scope).
	status, _ = env.httpRequest(http.MethodPost, "/api/guest-invites/"+inv.ID.String()+"/accept", bob.Token, nil)
	if status != http.StatusNotFound {
		t.Errorf("expected 404 cross-tenant accept, got %d", status)
	}
	status, _ = env.httpRequest(http.MethodDelete, "/api/guest-invites/"+inv.ID.String(), bob.Token, nil)
	if status != http.StatusNotFound {
		t.Errorf("expected 404 cross-tenant revoke, got %d", status)
	}
}

func TestShareLinkInvalidResourceType(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	f := createFolder(t, env, tok.Token, nil, "Any")

	status, _ := env.httpRequest(http.MethodPost, "/api/share-links", tok.Token, createShareLinkPayload{
		ResourceType: "unknown",
		ResourceID:   f.ID.String(),
	})
	if status != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid resource_type, got %d", status)
	}
}

func TestGuestInviteInvalidRole(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	f := createFolder(t, env, tok.Token, nil, "Any")

	status, _ := env.httpRequest(http.MethodPost, "/api/guest-invites", tok.Token, createGuestInvitePayload{
		Email:    "guest@example.test",
		FolderID: f.ID.String(),
		Role:     "superadmin",
	})
	if status != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid role, got %d", status)
	}
}

func TestGuestInviteInvalidEmail(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	f := createFolder(t, env, tok.Token, nil, "Any")

	status, _ := env.httpRequest(http.MethodPost, "/api/guest-invites", tok.Token, createGuestInvitePayload{
		Email:    "not-an-email",
		FolderID: f.ID.String(),
		Role:     "viewer",
	})
	if status != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid email, got %d", status)
	}
}

// TestSearchFiltersByWorkspace verifies the FTS endpoint filters by
// workspace and returns folders + files that match the query. It stands
// in as the integration coverage asked for by the Phase 2 checklist
// (alongside the sharing tests).
func TestSearchFiltersByWorkspace(t *testing.T) {
	env := setupEnv(t)
	alice := env.signupAndLogin("Acme", "alice@acme.test", "Alice", "pw1")
	bob := env.signupAndLogin("Globex", "bob@globex.test", "Bob", "pw2")

	createFolder(t, env, alice.Token, nil, "Quarterly Report")
	createFolder(t, env, alice.Token, nil, "Meeting Notes")
	createFolder(t, env, bob.Token, nil, "Quarterly Results") // different workspace

	status, body := env.httpRequest(http.MethodGet, "/api/search?q=quarterly", alice.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("search: status=%d body=%s", status, string(body))
	}
	var out struct {
		Results []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"hits"`
	}
	env.decodeJSON(body, &out)
	if len(out.Results) != 1 {
		t.Fatalf("expected 1 result in alice's workspace, got %d: %+v", len(out.Results), out.Results)
	}
	if out.Results[0].Name != "Quarterly Report" || out.Results[0].Type != "folder" {
		t.Errorf("unexpected result: %+v", out.Results[0])
	}

	// Bob should only see his Quarterly Results row.
	status, body = env.httpRequest(http.MethodGet, "/api/search?q=quarterly", bob.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("bob search: status=%d body=%s", status, string(body))
	}
	env.decodeJSON(body, &out)
	if len(out.Results) != 1 || out.Results[0].Name != "Quarterly Results" {
		t.Errorf("expected bob's workspace result only, got %+v", out.Results)
	}
}

func TestSearchRequiresQuery(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	status, _ := env.httpRequest(http.MethodGet, "/api/search", tok.Token, nil)
	if status != http.StatusBadRequest {
		t.Errorf("expected 400 without q, got %d", status)
	}
}
