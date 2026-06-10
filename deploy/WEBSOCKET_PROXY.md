# WebSocket connection proxy tier (Centrifugo / Pusher)

This guide covers running zk-drive's real-time notifications behind an
**external WebSocket connection proxy** for deployments past ~10k
concurrent connections, via `WS_PROXY_MODE`.

It complements the env-var reference in
[`docs/CONFIGURATION.md`](../docs/CONFIGURATION.md) → *WebSocket proxy
tier* — read that first.

---

## Why a proxy tier

By default each zk-drive API pod terminates its own WebSocket
connections in the in-process `api/ws.Hub` and fans events out either
locally or — when `REDIS_URL` is set — across replicas via Redis pub/sub
(`ws:*` channels; see [`ARCHITECTURE.md`](../docs/ARCHITECTURE.md) §10).

That model is simple and correct, but every live connection costs a file
descriptor, a read goroutine, a write goroutine, and a send buffer **on
an API pod**. Past ~10k concurrent connections per pod the connection
overhead starts competing with request-serving work, and you are forced
to scale the (relatively expensive, stateful-at-the-edge) API fleet just
to hold idle sockets.

A dedicated connection proxy (Centrifugo, Pusher, or equivalent) is
purpose-built to hold millions of mostly-idle connections cheaply. In
proxy mode the API pods do **no** connection holding: they only
*publish* events to Redis, and the proxy tier — subscribed to the same
`ws:*` channels — does the fan-out to clients.

```
                       publish ws:{ws}:{user}
   ┌──────────────┐    ─────────────────────►   ┌───────────┐   WS   ┌────────┐
   │ zk-drive API │           Redis pub/sub      │ Centrifugo│ ─────► │ client │
   │   (no conns) │    ◄─────────────────────    │  / Pusher │        └────────┘
   └──────────────┘        subscribe ws:*        └───────────┘
```

The API fleet scales on request load; the proxy fleet scales on
connection count. The two concerns are decoupled.

---

## Enabling proxy mode

Set on every API pod:

```bash
WS_PROXY_MODE=true
REDIS_URL=redis://redis:6379/0   # REQUIRED in proxy mode
```

`WS_PROXY_MODE` requires `REDIS_URL` — the API and the proxy tier
communicate only through Redis. If `WS_PROXY_MODE` is set but
`REDIS_URL` is empty, the server logs a warning and **falls back to the
in-process hub** so a fat-fingered rollout degrades to the
single-process path rather than dropping every notification silently.

In proxy mode:

- The API publishes every real-time event to Redis pub/sub exactly as in
  multi-replica mode — **no event-format change**, so the proxy tier and
  the in-process hub are wire-compatible.
- The API does **not** run the `ws:*` → hub subscribe loop (the proxy is
  the subscriber now).
- `GET /api/ws` on the API responds **501 Not Implemented**, so a client
  still dialing the API directly fails loudly instead of opening a socket
  that will never receive events. Point clients at the proxy's WS URL.

## Wire contract the proxy must implement

The contract is intentionally tiny — any proxy that can subscribe to
Redis and relay JSON to authenticated connections works.

**Channels.** zk-drive `PUBLISH`es to per-user channels:

```
ws:{workspaceID}:{userID}
```

both UUIDs in canonical hyphenated form (e.g.
`ws:1f2e…:9a8b…`). Workspace-wide events (change feed) are published to
the recipients' per-user channels as well, so a single channel grammar
covers both.

**Payload.** The channel message body is the JSON envelope clients
already expect:

```json
{ "type": "notification", "payload": { "...": "..." } }
```

**Routing.** The proxy must deliver a message on `ws:{ws}:{user}` only to
connections authenticated as that `(workspaceID, userID)`. Cross-user
delivery would leak notifications across tenants — this is the one
security-critical invariant of the proxy tier.

### Centrifugo

Centrifugo's [Redis engine](https://centrifugal.dev/docs/server/engines)
consumes a Redis pub/sub stream natively. Map the zk-drive channel to a
Centrifugo channel namespace and authenticate connections with the same
JWT zk-drive already issues (`sub` = userID, `workspaceID` in a claim):

```jsonc
// config.json (excerpt)
{
  "engine": "redis",
  "redis_address": "redis://redis:6379/0",
  "token_hmac_secret_key": "<same secret the API signs WS JWTs with>",
  "namespaces": [
    { "name": "ws", "presence": false, "join_leave": false }
  ],
  // Subscribe-side: a small relay (or Centrifugo's consumer) maps
  // Redis `ws:{ws}:{user}` → Centrifugo channel `ws:#{userID}` so
  // per-user private delivery is enforced by Centrifugo's channel
  // authorization (the `#` private-channel suffix).
}
```

The client connects to Centrifugo (not the API) with its zk-drive JWT
and subscribes to its own `ws:#{userID}` channel.

### Pusher / pusher-compatible (e.g. Soketi)

Run a thin relay that `PSUBSCRIBE ws:*`, parses `(workspaceID, userID)`
from the channel name, and calls `trigger` on the
private channel `private-ws-{workspaceID}-{userID}`. Authorize the
private channel in the Pusher auth endpoint by validating the same JWT.

---

## Operational notes

- **At-most-once, like the in-process hub.** Redis pub/sub does not
  persist; an event published while a client is briefly disconnected is
  dropped. This matches the existing guarantee — the Postgres row is the
  source of truth and clients re-fetch on reconnect (see the writePump
  drop policy in `api/ws/handler.go`). The proxy tier does not change
  this.
- **Presence / web-push fallback.** The API's `IsConnected` check is
  replica-local and only reflects in-process connections, so in proxy
  mode it always reports "not connected" and the web-push fallback (if
  configured) fires for every notification. If you rely on web push,
  drive presence from the proxy (Centrifugo exposes presence) rather than
  the API in proxy deployments.
- **Rollout.** Flipping `WS_PROXY_MODE` is safe to do per-pod: pods still
  in in-process mode keep serving their own connections while proxy-mode
  pods publish to the same Redis channels. Cut clients over to the proxy
  URL once all pods carry the flag.
