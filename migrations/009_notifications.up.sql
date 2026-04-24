-- Phase 2: in-app notifications.
--
-- Notifications are workspace- and user-scoped events surfaced to the
-- recipient's notification center. Types in use today:
--   share_link.created      — resource owner was informed a new link
--                             was minted
--   guest_invite.sent       — invitee (if they have an account)
--   guest_invite.accepted   — invite creator
--   scan.quarantined        — workspace admins when ClamAV quarantines
--
-- read_at is NULL until the recipient marks the notification read, so
-- indexes prioritise unread lookups without scanning the full table.

CREATE TABLE notifications (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id),
    user_id UUID NOT NULL REFERENCES users(id),
    type TEXT NOT NULL,
    title TEXT NOT NULL,
    body TEXT NOT NULL DEFAULT '',
    resource_type TEXT,
    resource_id UUID,
    read_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_notifications_user_created
    ON notifications(workspace_id, user_id, created_at DESC);
CREATE INDEX idx_notifications_user_unread
    ON notifications(workspace_id, user_id, created_at DESC)
    WHERE read_at IS NULL;
