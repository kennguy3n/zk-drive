package notification

import (
	"testing"

	"github.com/google/uuid"
)


func TestComposeNotificationURL(t *testing.T) {
	id := uuid.New()
	cases := []struct {
		name         string
		resourceType *string
		resourceID   *uuid.UUID
		want         string
	}{
		{"file deep links to document route", stringPtr("file"), &id, "/drive/document/" + id.String()},
		{"folder deep links to folder route", stringPtr("folder"), &id, "/drive/folder/" + id.String()},
		{"unknown type falls back to empty", stringPtr("guest_invite"), &id, ""},
		{"nil type yields empty", nil, &id, ""},
		{"nil id yields empty", stringPtr("file"), nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := composeNotificationURL(tc.resourceType, tc.resourceID); got != tc.want {
				t.Fatalf("composeNotificationURL(%v, %v) = %q, want %q", tc.resourceType, tc.resourceID, got, tc.want)
			}
		})
	}
}

func TestPushPayloadFromEventPopulatesURL(t *testing.T) {
	fileID := uuid.New()
	event := Event{
		Type: "notification",
		Payload: &Notification{
			Title:        "File quarantined",
			Body:         "ClamAV flagged a file version.",
			Type:         "scan.quarantined",
			ResourceType: stringPtr("file"),
			ResourceID:   &fileID,
		},
	}
	payload, ok := pushPayloadFromEvent(event)
	if !ok {
		t.Fatal("expected ok=true for notification event")
	}
	if want := "/drive/document/" + fileID.String(); payload.URL != want {
		t.Fatalf("payload.URL = %q, want %q", payload.URL, want)
	}
	if payload.Title != "File quarantined" {
		t.Fatalf("payload.Title = %q", payload.Title)
	}
}

func TestPushPayloadFromEventNonNavigableLeavesURLEmpty(t *testing.T) {
	inviteID := uuid.New()
	event := Event{
		Type: "notification",
		Payload: &Notification{
			Title:        "Guest invite accepted",
			Body:         "someone@example.com accepted your invitation.",
			Type:         "guest_invite.accepted",
			ResourceType: stringPtr("guest_invite"),
			ResourceID:   &inviteID,
		},
	}
	payload, ok := pushPayloadFromEvent(event)
	if !ok {
		t.Fatal("expected ok=true for notification event")
	}
	if payload.URL != "" {
		t.Fatalf("payload.URL = %q, want empty (service worker falls back to /drive)", payload.URL)
	}
}
