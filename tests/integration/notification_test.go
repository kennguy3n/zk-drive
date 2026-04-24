package integration

import (
	"net/http"
	"testing"

	"github.com/kennguy3n/zk-drive/internal/notification"
	"github.com/kennguy3n/zk-drive/internal/sharing"
)

// TestShareLinkCreatesNotification verifies that creating a share
// link fans out a share_link.created notification to the caller,
// exercising the end-to-end wiring:
//   handler → sharing.Service → notification.Service → repository
func TestShareLinkCreatesNotification(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Marketing")

	status, _ := env.httpRequest(http.MethodPost, "/api/share-links", tok.Token, createShareLinkPayload{
		ResourceType: sharing.ResourceFolder,
		ResourceID:   fold.ID.String(),
	})
	if status != http.StatusCreated {
		t.Fatalf("create link: status=%d", status)
	}

	status, body := env.httpRequest(http.MethodGet, "/api/notifications", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("list notifications: status=%d body=%s", status, string(body))
	}
	var resp struct {
		Notifications []notification.Notification `json:"notifications"`
	}
	env.decodeJSON(body, &resp)
	if len(resp.Notifications) == 0 {
		t.Fatalf("expected at least one notification, got none")
	}
	var found bool
	for _, n := range resp.Notifications {
		if n.Type == notification.TypeShareLinkCreated {
			found = true
			if n.ReadAt != nil {
				t.Errorf("expected unread notification, got read_at=%v", n.ReadAt)
			}
			break
		}
	}
	if !found {
		t.Errorf("expected a %s notification, got %+v", notification.TypeShareLinkCreated, resp.Notifications)
	}
}

// TestMarkNotificationReadAndAll covers both single-mark and
// bulk-mark endpoints. A second share link fan-out gives us two
// notifications to flip, so the bulk call has something to do.
func TestMarkNotificationReadAndAll(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Bulk")

	for i := 0; i < 2; i++ {
		status, _ := env.httpRequest(http.MethodPost, "/api/share-links", tok.Token, createShareLinkPayload{
			ResourceType: sharing.ResourceFolder,
			ResourceID:   fold.ID.String(),
		})
		if status != http.StatusCreated {
			t.Fatalf("create link %d: status=%d", i, status)
		}
	}

	status, body := env.httpRequest(http.MethodGet, "/api/notifications", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("list: status=%d body=%s", status, string(body))
	}
	var resp struct {
		Notifications []notification.Notification `json:"notifications"`
	}
	env.decodeJSON(body, &resp)
	if len(resp.Notifications) < 2 {
		t.Fatalf("expected >=2 notifications, got %d", len(resp.Notifications))
	}

	first := resp.Notifications[0]
	status, _ = env.httpRequest(http.MethodPost, "/api/notifications/"+first.ID.String()+"/read", tok.Token, nil)
	if status != http.StatusNoContent {
		t.Errorf("mark single read: expected 204, got %d", status)
	}

	status, _ = env.httpRequest(http.MethodPost, "/api/notifications/read-all", tok.Token, nil)
	if status != http.StatusNoContent {
		t.Errorf("mark all read: expected 204, got %d", status)
	}

	status, body = env.httpRequest(http.MethodGet, "/api/notifications", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("list after: status=%d body=%s", status, string(body))
	}
	var after struct {
		Notifications []notification.Notification `json:"notifications"`
	}
	env.decodeJSON(body, &after)
	for _, n := range after.Notifications {
		if n.ReadAt == nil {
			t.Errorf("expected every notification to be read after mark-all, got unread %s", n.ID)
		}
	}
}

// TestGuestInviteAcceptNotifiesCreator verifies the accept flow
// writes a guest_invite.accepted notification back to the user who
// minted the invite.
func TestGuestInviteAcceptNotifiesCreator(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Shared")

	status, body := env.httpRequest(http.MethodPost, "/api/guest-invites", tok.Token, map[string]any{
		"folder_id": fold.ID.String(),
		"email":     "guest@example.test",
		"role":      "viewer",
	})
	if status != http.StatusCreated {
		t.Fatalf("create invite: status=%d body=%s", status, string(body))
	}
	var inv sharing.GuestInvite
	env.decodeJSON(body, &inv)

	status, _ = env.httpRequest(http.MethodPost, "/api/guest-invites/"+inv.ID.String()+"/accept", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("accept: status=%d", status)
	}

	status, body = env.httpRequest(http.MethodGet, "/api/notifications", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("list: status=%d body=%s", status, string(body))
	}
	var resp struct {
		Notifications []notification.Notification `json:"notifications"`
	}
	env.decodeJSON(body, &resp)
	var sawAccept bool
	for _, n := range resp.Notifications {
		if n.Type == notification.TypeGuestInviteAccepted {
			sawAccept = true
			break
		}
	}
	if !sawAccept {
		t.Errorf("expected %s notification, got %+v", notification.TypeGuestInviteAccepted, resp.Notifications)
	}
}
