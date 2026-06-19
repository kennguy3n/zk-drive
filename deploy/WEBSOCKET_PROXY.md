# Proxying the real-time WebSocket relay

zk-drive serves its real-time features over long-lived WebSocket
upgrades on the same origin as the REST API (`:8080` by default). Any
reverse proxy, load balancer, or ingress in front of zk-drive must be
configured to pass those upgrades through. This guide covers the
upgrade-header and session-affinity requirements, the CSP `connect-src`
allow-list, and — for very large connection counts — offloading the
notifications socket to an external connection-proxy tier.

It complements the env-var reference in
[`docs/CONFIGURATION.md`](../docs/CONFIGURATION.md); the architectural
role of the hubs is in [`ARCHITECTURE.md`](../docs/ARCHITECTURE.md).

---

## The two WebSocket endpoints

zk-drive exposes two long-lived upgrade endpoints, both mounted under
`/api` and both served same-origin by the API server:

| Path                       | Purpose                                                              | Source                       |
| -------------------------- | ------------------------------------------------------------------- | ---------------------------- |
| `/api/ws`                  | Real-time notifications and the workspace change feed.              | `cmd/server/main.go:1725`    |
| `/api/documents/{id}/ws`   | Collaborative document editing — the per-document Yjs CRDT relay.   | `cmd/server/main.go:1735`    |

Both endpoints authenticate the connection from the same JWT the REST
API uses, and both run the suspension guard and the per-workspace IP
allowlist on the initial HTTP request *before* the upgrade completes
(`cmd/server/main.go:1704`, `:1714`). They deliberately skip the tenant
guard and the per-request rate limiter — the upgrade handshake doesn't
fit those middlewares' request model — so each handler performs its own
tenant and permission check on the upgrade path.

---

## Reverse-proxy configuration (upgrade headers)

A WebSocket starts as an HTTP/1.1 request carrying `Upgrade: websocket`
and `Connection: Upgrade`. A proxy that drops those headers, downgrades
to HTTP/1.0, buffers the response, or applies a short read timeout will
break the connection. Configure the proxy to forward the upgrade and to
treat these sockets as long-lived.

### nginx

```nginx
location /api/ {
    proxy_pass http://zk_drive_upstream;
    proxy_http_version 1.1;
    proxy_set_header Upgrade    $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host       $host;
    proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;

    # WebSockets are idle for long stretches; don't cut them off.
    proxy_read_timeout  3600s;
    proxy_send_timeout  3600s;
    proxy_buffering     off;
}
```

`X-Forwarded-For` matters: the IP allowlist and the auth-failure
reputation limiter read the client IP from the forwarded chain, trimmed
by `TRUSTED_PROXY_DEPTH` (see
[`docs/CONFIGURATION.md`](../docs/CONFIGURATION.md)). Set
`TRUSTED_PROXY_DEPTH` to the number of proxies you actually run so a
client can't spoof the header.

### Caddy

Caddy proxies WebSockets transparently — `reverse_proxy` forwards the
upgrade with no extra directives:

```caddy
drive.example.com {
    reverse_proxy zk-drive:8080
}
```

### Traefik

WebSocket upgrades pass through automatically; just route `/api` (or the
whole host) to the zk-drive service. Raise the server/forwarding
timeouts on the entrypoint if your defaults are short, so idle sockets
aren't reaped.

### AWS ALB / cloud L7 load balancers

Application Load Balancers support WebSockets natively (the upgrade is
preserved). Raise the **idle timeout** (default 60 s) above your
client's ping interval so idle editor or notification sockets aren't
dropped mid-session. The Terraform module sets this for you
(`deploy/terraform/aws`).

---

## Session affinity (sticky sessions)

Whether you need sticky sessions depends on whether `REDIS_URL` is set.

- **With `REDIS_URL` configured — no affinity required.** Both hubs fan
  out across replicas through Redis pub/sub: notifications publish to
  `ws:{workspaceID}:{userID}` channels, and the collaboration hub
  relays document frames over `collab:*` channels
  (`cmd/server/main.go:883`–`909`). A client can land on any pod and
  still receive every event; collaborators editing the same document can
  sit on different pods and converge. Plain round-robin load balancing
  is fine.
- **Without `REDIS_URL` — single replica, or affinity required.** Each
  pod only sees its own in-process connections. Notifications generated
  on another pod won't reach a client elsewhere, and — critically — two
  people editing the same document must reach the **same** pod to share
  a CRDT room. Either run a single API replica, or pin session affinity
  (e.g. cookie- or path-based) so all participants on a document land
  together. Configuring `REDIS_URL` is the recommended multi-replica
  path.

---

## CSP `connect-src` allow-list

The browser will only open a WebSocket to an origin permitted by the
Content-Security-Policy `connect-src` directive. zk-drive sets
`connect-src 'self'` by default
(`api/middleware/security_headers.go:326`, `:347`).

- **Same-origin (the default).** When the SPA and the API share an
  origin — the standard same-origin `:8080` deployment — the WebSocket
  endpoints are dialed on that same origin, and `'self'` already allows
  the `wss://` (or `ws://`) upgrade in every browser the SPA targets.
  No extra configuration is needed
  (`api/middleware/security_headers.go:317`–`322`).
- **Cross-origin WebSocket gateway.** If you terminate WebSockets on a
  *different* host (a dedicated socket domain, or the external proxy tier
  below), add that explicit origin to `connect-src` with
  `SECURITY_HEADERS_CSP_CONNECT_EXTRA` (comma-separated;
  `internal/config/config.go:300`, `:851`):

  ```bash
  SECURITY_HEADERS_CSP_CONNECT_EXTRA=wss://realtime.example.com
  ```

  List the **full origin** (scheme + host). A bare `wss:` scheme is
  intentionally *not* accepted — allowing it would let an XSS payload
  exfiltrate to any `wss://` host — so each cross-origin gateway must be
  named explicitly (`api/middleware/security_headers.go:322`–`325`).

---

## Scaling the notifications socket past ~10k connections

By default each API pod terminates its own notification WebSocket
connections in the in-process `api/ws.Hub` and fans events out either
locally or — with `REDIS_URL` set — across replicas via Redis pub/sub
(`ws:*` channels). That model is simple and correct, but every live
connection costs a file descriptor, a read goroutine, a write goroutine,
and a send buffer **on an API pod**. Past roughly 10k concurrent
connections per pod the connection overhead competes with
request-serving work, forcing you to scale the (relatively expensive)
API fleet just to hold idle sockets.

`WS_PROXY_MODE` offloads that connection holding to an external
connection proxy (Centrifugo, Pusher, or equivalent) that is
purpose-built to hold millions of mostly-idle sockets cheaply. In proxy
mode the API pods publish events to Redis but hold no client sockets;
the proxy tier subscribes to the same channels and fans out to clients.

```
                       publish ws:{ws}:{user}
   ┌──────────────┐    ─────────────────────►   ┌───────────┐   WS   ┌────────┐
   │ zk-drive API │           Redis pub/sub      │ Centrifugo│ ─────► │ client │
   │   (no conns) │    ◄─────────────────────    │  / Pusher │        └────────┘
   └──────────────┘        subscribe ws:*        └───────────┘
```

> **Scope.** `WS_PROXY_MODE` offloads only the **notifications** socket
> (`/api/ws`). The collaborative-editing relay
> (`/api/documents/{id}/ws`) always runs in-process on the API, fanning
> out across replicas through its own `collab:*` Redis channels. The
> proxy tier does not carry collaboration traffic.

### Enabling proxy mode

Set on every API pod:

```bash
WS_PROXY_MODE=true
REDIS_URL=redis://redis:6379/0   # REQUIRED in proxy mode
```

`WS_PROXY_MODE` requires `REDIS_URL` — the API and the proxy tier
communicate only through Redis. If `WS_PROXY_MODE` is set but `REDIS_URL`
is empty, the server logs a warning and **falls back to the in-process
hub**, so a fat-fingered rollout degrades to the single-process path
rather than dropping every notification silently.

In proxy mode:

- The API publishes every real-time event to Redis pub/sub exactly as in
  multi-replica mode — **no event-format change**, so the proxy tier and
  the in-process hub are wire-compatible.
- The API does **not** run the `ws:*` → hub subscribe loop (the proxy is
  the subscriber now).
- `GET /api/ws` responds **501 Not Implemented**
  (`cmd/server/main.go:1721`), so a client still dialing the API
  directly fails loudly instead of opening a socket that will never
  receive events. Point clients at the proxy's WebSocket URL.

### Wire contract the proxy must implement

The contract is intentionally tiny — any proxy that can subscribe to
Redis and relay JSON to authenticated connections works.

**Channels.** zk-drive `PUBLISH`es to per-user channels:

```
ws:{workspaceID}:{userID}
```

both UUIDs in canonical hyphenated form (e.g. `ws:1f2e…:9a8b…`).
Workspace-wide events (the change feed) are published to the recipients'
per-user channels as well, so a single channel grammar covers both.

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
  ]
  // Subscribe-side: a small relay (or Centrifugo's consumer) maps
  // Redis `ws:{ws}:{user}` → Centrifugo channel `ws:#{userID}` so
  // per-user private delivery is enforced by Centrifugo's channel
  // authorization (the `#` private-channel suffix).
}
```

The client connects to Centrifugo (not the API) with its zk-drive JWT
and subscribes to its own `ws:#{userID}` channel. Remember to add the
Centrifugo origin to `SECURITY_HEADERS_CSP_CONNECT_EXTRA` (above).

### Pusher / pusher-compatible (e.g. Soketi)

Run a thin relay that `PSUBSCRIBE ws:*`, parses `(workspaceID, userID)`
from the channel name, and calls `trigger` on the private channel
`private-ws-{workspaceID}-{userID}`. Authorize the private channel in the
Pusher auth endpoint by validating the same JWT.

---

## Operational notes

- **At-most-once, like the in-process hub.** Redis pub/sub does not
  persist; an event published while a client is briefly disconnected is
  dropped. This matches the in-process guarantee — the Postgres row is
  the source of truth and clients re-fetch on reconnect (see the
  writePump drop policy in `api/ws/handler.go`). The proxy tier does not
  change this.
- **Presence / web-push fallback.** The API's `IsConnected` check is
  replica-local and only reflects in-process connections, so in proxy
  mode it always reports "not connected" and the web-push fallback (if
  configured) fires for every notification. If you rely on web push,
  drive presence from the proxy (Centrifugo exposes presence) rather than
  the API in proxy deployments.
- **Per-pod rollout.** Flipping `WS_PROXY_MODE` is safe to do per-pod:
  pods still in in-process mode keep serving their own connections while
  proxy-mode pods publish to the same Redis channels. Cut clients over to
  the proxy URL once all pods carry the flag.
