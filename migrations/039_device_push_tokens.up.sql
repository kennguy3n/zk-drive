-- Native mobile push device tokens (APNs for iOS, FCM for Android).
--
-- The native iOS (SwiftUI) and Android (Jetpack Compose) apps register
-- the device token their platform push service hands them
-- (APNsdeviceToken / FirebaseMessaging token) via
-- POST /api/push/register-device so the server can deliver
-- notifications to the phone while the app is backgrounded or killed —
-- the mobile counterpart to webpush_subscriptions (migration 038),
-- which covers PWA / browser clients only.
--
-- One row per (workspace, user, platform, token): a user signed in on
-- an iPhone and an Android tablet has two rows. The same physical
-- install re-registers its token on every cold start (and whenever the
-- OS rotates it), so registration is an UPSERT that refreshes
-- updated_at rather than inserting duplicates.
--
-- The number of rows per (workspace, user, platform) is capped in the
-- application layer (notification.MaxDeviceTokensPerUserPlatform) by
-- LRU eviction inside the same transaction as the upsert: registering a
-- new device past the cap drops the least-recently-updated token rather
-- than rejecting the registration. This bounds the per-notification
-- fan-out cost (one APNs/FCM POST per token) so an authenticated client
-- cannot grow its own delivery cost without bound by minting distinct
-- tokens. The cap lives in Go rather than a DB trigger so it stays
-- testable and colocated with the upsert it guards.
--
-- # Platform
--
-- `platform` selects the delivery provider downstream:
--   'ios'     -> APNs  (token = 32-byte APNs device token, hex; ~64 chars)
--   'android' -> FCM   (token = FCM registration token; ~163+ chars)
-- The CHECK pins it to the two providers the publisher fans out to so a
-- typo ('android ', 'fcm', …) is rejected at write time instead of
-- silently never receiving a push. The 4 KiB length cap is defence in
-- depth against an authenticated client persisting an arbitrarily large
-- string; real tokens are well under it (the MobilePushService rejects
-- over-long tokens with a 400 before they reach here).
--
-- # Lifecycle
--
-- Rows are created/refreshed by POST /api/push/register-device and
-- removed either by DELETE /api/push/register-device (the app on
-- sign-out / when the user disables notifications) or automatically by
-- the MobilePushService when a delivery attempt comes back with a
-- permanent "this token is dead" signal — APNs 410 Unregistered /
-- 400 BadDeviceToken, or FCM UNREGISTERED / INVALID_ARGUMENT — exactly
-- mirroring how the WebPushService prunes a 410 Gone subscription.
--
-- # RLS
--
-- Same `tenant_isolation` pattern as the rest of the schema
-- (migration 024) and webpush_subscriptions (038). Request-scoped
-- queries (the register / unregister handlers) set the app.workspace_id
-- GUC and see only their own workspace; the notification publisher runs
-- without the GUC set so the `app_current_workspace_id() IS NULL`
-- bypass branch fires when it fans push messages out to a user's
-- offline devices.

CREATE TABLE device_push_tokens (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    platform     TEXT NOT NULL CHECK (platform IN ('ios', 'android')),
    token        TEXT NOT NULL CHECK (length(token) <= 4096),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Bumped to now() on every re-registration (the ON CONFLICT upsert in
    -- SaveDeviceToken). A live install re-registers on each cold start, so
    -- a row untouched for a long time flags an app that was uninstalled
    -- without an explicit unregister and never produced a dead-token
    -- bounce, letting an operator prune orphans with a periodic
    -- `DELETE ... WHERE updated_at < ...`.
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(workspace_id, user_id, platform, token)
);

-- The hot path on every notification publish — "find every device token
-- for (workspace, user)" to fan a push out to a user's offline phones —
-- is served by the UNIQUE(workspace_id, user_id, platform, token)
-- constraint's backing B-tree via a leading-column prefix scan on
-- (workspace_id, user_id). A separate (workspace_id, user_id) index
-- would be redundant write amplification with no read benefit, so we
-- omit it (same reasoning as webpush_subscriptions in migration 038).

ALTER TABLE device_push_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE device_push_tokens FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON device_push_tokens
    USING (
        app_current_workspace_id() IS NULL
        OR workspace_id = app_current_workspace_id()
    )
    WITH CHECK (
        app_current_workspace_id() IS NULL
        OR workspace_id = app_current_workspace_id()
    );
