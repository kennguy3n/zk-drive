-- Outbound webhook subscriptions for workspace events.
--
-- Workspace admins create subscriptions that POST a signed JSON
-- payload to their own HTTPS endpoint whenever an event of interest
-- occurs in their workspace. Patterns after Stripe's webhook scheme
-- (HMAC-SHA256 over `t=<timestamp>.<body>`, 5-minute timestamp
-- tolerance) so consumers can reuse familiar verification snippets.
--
-- The B2B value: ZK Drive sits behind KChat (the team chat product
-- documented in README.md) AND ships as a standalone product.
-- Both flavours want integrations with Zapier, n8n, customer-built
-- automation, and KChat itself — without webhooks every integration
-- has to poll, which is bandwidth-inefficient AND can't react to
-- events with low latency.
--
-- # Schema overview
--
-- webhook_subscriptions  — workspace admin's declared interest in
--                          one event type, with the endpoint URL, an
--                          HMAC secret, and a failure counter that
--                          auto-pauses a flapping endpoint at 50
--                          consecutive failures.
-- webhook_deliveries     — every attempt to deliver an event payload
--                          to a subscription. One row per attempt
--                          (an event with 3 retries produces 4 rows
--                          all sharing event_id). status_code = 0
--                          means the attempt never produced an HTTP
--                          response (DNS, dial, TLS, timeout); the
--                          error_message column carries the cause.
--                          response_body is capped at 4 KiB so a
--                          chatty endpoint can't blow up the table.
--
-- # RLS
--
-- Both tables use the same `tenant_isolation` pattern as the rest of
-- the schema (migration 024). When the worker drains the delivery
-- queue it does NOT call set_config('app.workspace_id', ...) so the
-- `app_current_workspace_id() IS NULL` bypass branch fires; admin
-- console queries always set the GUC and see only their own data.
--
-- # Indexes
--
-- The hot query paths are:
--   1. "list subscriptions for this workspace ordered by created_at"
--      → idx_webhook_subs_workspace
--   2. "list recent deliveries for this subscription"
--      → idx_webhook_deliveries_subscription
--   3. "show deliveries for a specific event id" (admin debugging)
--      → idx_webhook_deliveries_event
--   4. "find subscriptions interested in event type X for ws Y"
--      (hot path on every event publish — covered by the partial
--      index restricted to active + non-paused rows)
--      → idx_webhook_subs_active_lookup

CREATE TABLE webhook_subscriptions (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id             UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    -- created_by is the admin user who clicked "Add webhook". Kept
    -- as a UUID reference (no ON DELETE CASCADE) so deleting the
    -- user doesn't silently remove their subscriptions — the audit
    -- trail of who set up the integration survives.
    created_by               UUID NOT NULL REFERENCES users(id),
    -- url MUST be HTTPS in production. URL validation enforces:
    --   - https only (the API rejects http unless explicit dev opt-in)
    --   - hostname resolves to a public IP at create-time (SSRF defense)
    --   - max 512 chars (column limit doubles as a sanity cap)
    url                      TEXT NOT NULL,
    -- event_type uses dotted-namespace strings: "file.upload.confirmed",
    -- "permission.granted", "member.joined", etc. Stored as TEXT
    -- rather than ENUM so adding new event types is a code-only change
    -- (no migration). A CHECK ensures non-empty.
    event_type               TEXT NOT NULL,
    -- description is the admin-facing label ("Slack #ops channel"
    -- or "Zapier customer-feedback workflow"). Nullable; the UI
    -- falls back to the URL hostname when empty.
    description              TEXT,
    -- secret is the per-subscription HMAC key. 32 random bytes
    -- (hex-encoded → 64 chars) chosen at create time. NEVER returned
    -- by any list/get endpoint after creation — only the initial
    -- POST response includes the secret so the admin can paste it
    -- into their consumer. If they lose it they have to rotate.
    secret                   TEXT NOT NULL,
    -- active=false stops new deliveries from being enqueued but
    -- preserves the row so an admin can re-enable without losing
    -- history. The /api/.../webhooks/{id} DELETE endpoint hard-
    -- deletes (with delivery history cascading); active=false is
    -- the "pause" path used by the auto-pause logic.
    active                   BOOLEAN NOT NULL DEFAULT TRUE,
    -- consecutive_failures counts deliveries that returned status
    -- code outside 2xx OR failed at the network layer. Reset to 0
    -- on any 2xx response. When this hits auto_pause_threshold (50)
    -- the worker flips active=false and stops trying.
    consecutive_failures     INTEGER NOT NULL DEFAULT 0,
    -- last_succeeded_at + last_attempted_at let the admin UI show
    -- "Last delivered: 5 min ago / Last attempted: 12s ago". Both
    -- nullable for newly-created subscriptions that haven't yet
    -- received an event.
    last_succeeded_at        TIMESTAMPTZ,
    last_attempted_at        TIMESTAMPTZ,
    -- auto_paused_at is set when consecutive_failures crosses the
    -- threshold. NULL when the subscription has not been auto-
    -- paused (or was manually re-enabled after an auto-pause).
    -- Persisting the timestamp lets the UI surface "Auto-paused 3h
    -- ago after 50 failed deliveries — fix your endpoint and click
    -- Resume".
    auto_paused_at           TIMESTAMPTZ,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT webhook_subs_url_nonempty   CHECK (length(url) > 0 AND length(url) <= 512),
    CONSTRAINT webhook_subs_event_nonempty CHECK (length(event_type) > 0 AND length(event_type) <= 128),
    CONSTRAINT webhook_subs_secret_len     CHECK (length(secret) >= 32)
);

CREATE INDEX idx_webhook_subs_workspace
    ON webhook_subscriptions (workspace_id, created_at DESC);

-- idx_webhook_subs_active_lookup is the hot-path index for the
-- event publisher: "find every active subscription in workspace W
-- for event type E". The partial predicate keeps the index small
-- (paused/inactive rows excluded) and the (workspace_id, event_type)
-- prefix supports the equality lookup directly.
CREATE INDEX idx_webhook_subs_active_lookup
    ON webhook_subscriptions (workspace_id, event_type)
    WHERE active = TRUE;

ALTER TABLE webhook_subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_subscriptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON webhook_subscriptions
    USING (
        app_current_workspace_id() IS NULL
        OR workspace_id = app_current_workspace_id()
    )
    WITH CHECK (
        app_current_workspace_id() IS NULL
        OR workspace_id = app_current_workspace_id()
    );


CREATE TABLE webhook_deliveries (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subscription_id          UUID NOT NULL REFERENCES webhook_subscriptions(id) ON DELETE CASCADE,
    workspace_id             UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    -- event_id is the idempotency key the subscriber sees in the
    -- X-ZkDrive-Event-ID header. Multiple delivery attempts for the
    -- same event share event_id. UUID rather than serial so the value
    -- is unguessable (event_id is included in the signed payload and
    -- a guessable ID could enable replay attacks if signature
    -- verification is naively implemented downstream).
    event_id                 UUID NOT NULL,
    event_type               TEXT NOT NULL,
    -- attempt_number is 1-indexed; 1 = initial attempt, 2-5 = retries.
    -- A successful delivery on attempt 3 produces a row with
    -- attempt_number=3 and outcome='success'. A permanent failure
    -- after 5 attempts produces 5 rows, the last with outcome='failed'.
    attempt_number           INTEGER NOT NULL CHECK (attempt_number >= 1),
    -- outcome enumerates the terminal state of THIS attempt:
    --   success   : 2xx response
    --   http_error: non-2xx response (subscriber rejected the call)
    --   net_error : DNS/dial/TLS/timeout failure (no HTTP response)
    --   blocked   : URL resolved to a forbidden range at delivery
    --               time (SSRF re-check after DNS rebinding); no
    --               request was sent
    outcome                  TEXT NOT NULL CHECK (outcome IN ('success', 'http_error', 'net_error', 'blocked')),
    -- status_code is the HTTP status returned by the subscriber.
    -- 0 means "no HTTP response was received" (net_error / blocked).
    status_code              INTEGER NOT NULL DEFAULT 0,
    -- response_body is capped at 4 KiB so a chatty endpoint can't
    -- blow up the table. The delivery client truncates with a clear
    -- "[truncated]" suffix. Useful for admin debugging ("why did my
    -- webhook return 422?").
    response_body            TEXT,
    -- error_message is the Go error string for net_error/blocked
    -- outcomes, NULL for success/http_error.
    error_message            TEXT,
    -- duration_ms measures the total time from request-start to
    -- response-end (or to the dial/TLS/timeout failure). Used for
    -- admin dashboards + the delivery histogram metric.
    duration_ms              INTEGER NOT NULL DEFAULT 0,
    attempted_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- next_retry_at is set to a future timestamp when this attempt
    -- failed AND we plan to retry. NULL when the delivery is in a
    -- terminal state (success or final failure). Surfaced in the
    -- admin UI as "Next attempt: in 3m 12s".
    next_retry_at            TIMESTAMPTZ
);

-- Hot query: "list the most recent deliveries for subscription X"
-- (admin UI history view + auto-pause counter).
CREATE INDEX idx_webhook_deliveries_subscription
    ON webhook_deliveries (subscription_id, attempted_at DESC);

-- Admin debugging: "show me every attempt for this event id".
CREATE INDEX idx_webhook_deliveries_event
    ON webhook_deliveries (event_id, attempt_number);

-- Workspace-scoped query for the admin dashboard's "recent webhook
-- activity" panel — covers all subscriptions in the workspace.
CREATE INDEX idx_webhook_deliveries_workspace
    ON webhook_deliveries (workspace_id, attempted_at DESC);

ALTER TABLE webhook_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_deliveries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON webhook_deliveries
    USING (
        app_current_workspace_id() IS NULL
        OR workspace_id = app_current_workspace_id()
    )
    WITH CHECK (
        app_current_workspace_id() IS NULL
        OR workspace_id = app_current_workspace_id()
    );

-- updated_at is maintained explicitly by the repository layer
-- (internal/webhooks/repository.go sets it on every UPDATE) — this
-- matches the convention used by the rest of the schema (no
-- shared trigger function exists; see migrations 001, 002, etc.).
