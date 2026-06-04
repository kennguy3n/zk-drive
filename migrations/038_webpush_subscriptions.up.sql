-- Web Push (RFC 8030 + VAPID) browser push subscriptions.
--
-- PWA clients that have granted the Notification permission register a
-- PushSubscription here so the server can deliver notifications even
-- when no tab is open / no WebSocket is connected. One row per
-- (workspace, user, endpoint): a user with the PWA installed on a
-- laptop and a phone has two rows. The browser-supplied `endpoint` is
-- the push service URL (FCM / Mozilla autopush / Apple), and
-- (p256dh, auth) are the base64url ECDH/auth keys used by the
-- webpush-go library to encrypt the payload per RFC 8291.
--
-- # Lifecycle
--
-- Rows are created by POST /api/push/subscribe and removed either by
-- DELETE /api/push/subscribe (the user revokes / logs out) or
-- automatically by the WebPushService when a push attempt returns
-- 410 Gone (the browser expired the subscription).
--
-- # RLS
--
-- Same `tenant_isolation` pattern as the rest of the schema
-- (migration 024). Request-scoped queries set the app.workspace_id
-- GUC and see only their own workspace; the notification publisher
-- runs without the GUC set so the `app_current_workspace_id() IS NULL`
-- bypass branch fires when it fans push messages out to offline users.

CREATE TABLE webpush_subscriptions (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- Real push-service endpoints are a few hundred bytes; cap at 2 KiB
    -- as defence in depth against an authenticated client persisting
    -- arbitrarily large strings. Mirrors maxPushEndpointLen in the
    -- WebPushService (which rejects over-long endpoints with a 400
    -- before they reach here).
    endpoint     TEXT NOT NULL CHECK (length(endpoint) <= 2048),
    -- p256dh / auth are tiny RFC 8291 key material (~88 and ~24 base64url
    -- chars). The app rejects raw values over maxPushKeyLen (256 B) with a
    -- 400 before storage; this CHECK is defence in depth on the stored
    -- value, sized at 1 KiB so it also accommodates the at-rest AES-GCM
    -- ciphertext ("aesgcm:" + base64(nonce||ct||tag)), which expands a
    -- 256 B input to under ~400 B.
    p256dh       TEXT NOT NULL CHECK (length(p256dh) <= 1024),
    auth         TEXT NOT NULL CHECK (length(auth) <= 1024),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Bumped to now() every time the client re-registers the endpoint
    -- (the ON CONFLICT upsert in SaveSubscription). The frontend
    -- re-subscribes on each load, so a live browser refreshes this
    -- regularly; a row that has not been touched in a long time signals
    -- a browser that was uninstalled / cleared without an explicit
    -- unsubscribe and never returned 410, letting an operator prune
    -- those orphans with a periodic `DELETE ... WHERE updated_at < ...`.
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(workspace_id, user_id, endpoint)
);

-- The hot path on every notification publish — "find every push
-- subscription for (workspace, user)" so we can fan out to a user's
-- registered devices when they're offline — is already served by the
-- UNIQUE(workspace_id, user_id, endpoint) constraint's backing B-tree:
-- a leading-column prefix scan on (workspace_id, user_id) uses it
-- directly. A separate index on (workspace_id, user_id) would be
-- redundant (extra write amplification, no read benefit), so we omit it.

ALTER TABLE webpush_subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE webpush_subscriptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON webpush_subscriptions
    USING (
        app_current_workspace_id() IS NULL
        OR workspace_id = app_current_workspace_id()
    )
    WITH CHECK (
        app_current_workspace_id() IS NULL
        OR workspace_id = app_current_workspace_id()
    );
