package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"strings"

	"github.com/kennguy3n/zk-drive/internal/logging"
	"github.com/kennguy3n/zk-drive/internal/typednil"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// pushDeliveryTimeout caps how long the detached Web Push fan-out may
// run after the originating request returns. Push delivery makes
// blocking HTTPS POSTs to third-party push services (FCM / Mozilla /
// Apple), each 100-500ms; bounding the work keeps a slow push service
// from leaking goroutines indefinitely.
const pushDeliveryTimeout = 15 * time.Second

// Event is the JSON envelope pushed to live WebSocket clients when a
// notification is created. The shape mirrors api/ws.Event but is
// duplicated here so the notification package does not import the
// transport package (avoiding an import cycle: ws depends on
// middleware, and middleware is exercised by tests that already
// touch notification).
type Event struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

// WSPublisher publishes a notification event to live WebSocket
// clients for (workspaceID, userID). Errors are surfaced to the
// caller; the notification service logs and swallows them so a
// transport failure never aborts the underlying database write.
type WSPublisher interface {
	Publish(ctx context.Context, workspaceID, userID uuid.UUID, event Event) error
}

// LocalBroadcaster is the subset of *ws.Hub the in-process publisher
// needs. The notification package depends on the abstraction; the
// concrete *ws.Hub is wired from cmd/server/main.go.
type LocalBroadcaster interface {
	BroadcastJSON(workspaceID, userID uuid.UUID, payload []byte)
}

// LocalPublisher pushes events directly to a hub running in the same
// process. Used in single-replica deployments and when REDIS_URL is
// not configured.
type LocalPublisher struct {
	bc LocalBroadcaster
}

// NewLocalPublisher returns a publisher that fans events to bc.
func NewLocalPublisher(bc LocalBroadcaster) *LocalPublisher {
	return &LocalPublisher{bc: bc}
}

// Publish encodes event and hands the bytes to the local hub.
func (p *LocalPublisher) Publish(_ context.Context, workspaceID, userID uuid.UUID, event Event) error {
	if p == nil || p.bc == nil {
		return nil
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal ws event: %w", err)
	}
	p.bc.BroadcastJSON(workspaceID, userID, payload)
	return nil
}

// channelFor returns the Redis pub/sub channel for (workspaceID, userID).
// Channels are namespaced under "ws:" to keep them out of the way of
// other Redis traffic.
func channelFor(workspaceID, userID uuid.UUID) string {
	return "ws:" + workspaceID.String() + ":" + userID.String()
}

// channelPattern returns the psubscribe pattern used to consume
// every WS channel (any workspace, any user).
const channelPattern = "ws:*"

// RedisPublisher publishes events to Redis pub/sub so that every
// replica subscribed to channelPattern can fan them out to its local
// clients. Use NewRedisPublisher to construct one.
type RedisPublisher struct {
	rdb *redis.Client
}

// NewRedisPublisher returns a publisher backed by rdb. The publisher
// does not own rdb; the caller closes it during shutdown.
func NewRedisPublisher(rdb *redis.Client) *RedisPublisher {
	return &RedisPublisher{rdb: rdb}
}

// Publish marshals event and PUBLISHes the bytes to ws:{workspaceID}:{userID}.
func (p *RedisPublisher) Publish(ctx context.Context, workspaceID, userID uuid.UUID, event Event) error {
	if p == nil || p.rdb == nil {
		return nil
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal ws event: %w", err)
	}
	if err := p.rdb.Publish(ctx, channelFor(workspaceID, userID), payload).Err(); err != nil {
		return fmt.Errorf("redis publish: %w", err)
	}
	return nil
}

// Subscribe subscribes to channelPattern and forwards every received
// payload to bc.BroadcastJSON. It blocks until ctx is canceled or
// the underlying Redis connection terminates fatally; callers
// typically run it in a dedicated goroutine.
//
// Channel parsing is deliberately strict: malformed channel names
// (anything that does not split into "ws:{workspaceUUID}:{userUUID}")
// are dropped and logged so that a producer bug never panics the
// subscriber loop.
func (p *RedisPublisher) Subscribe(ctx context.Context, bc LocalBroadcaster) error {
	if p == nil || p.rdb == nil || bc == nil {
		return nil
	}
	sub := p.rdb.PSubscribe(ctx, channelPattern)
	defer func() { _ = sub.Close() }()
	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			workspaceID, userID, err := parseChannel(msg.Channel)
			if err != nil {
				logging.FromContext(ctx).Warn("ws redis drop message: invalid channel", "channel", msg.Channel, "err", err)
				continue
			}
			bc.BroadcastJSON(workspaceID, userID, []byte(msg.Payload))
		}
	}
}

// ConnectionChecker reports whether a user currently has at least one
// live WebSocket connection on this replica. Implemented by *ws.Hub
// (IsConnected). Used by WebPushPublisher to decide whether to fall
// back to a browser push notification.
type ConnectionChecker interface {
	IsConnected(workspaceID, userID uuid.UUID) bool
}

// PushSender delivers a browser push notification to a user's
// registered subscriptions. Implemented by *WebPushService.
type PushSender interface {
	Send(ctx context.Context, workspaceID, userID uuid.UUID, payload NotificationPayload) error
}

// WebPushPublisher decorates an inner WSPublisher: it first performs
// the normal WebSocket publish (local hub or Redis pub/sub fan-out),
// then — for a "notification" event whose recipient has no live
// WebSocket connection on this replica — also delivers the same
// notification via Web Push so PWA users see it with the tab closed.
//
// The connection check is best-effort and replica-local: in a
// multi-replica deployment a user connected to a different replica
// may still receive a push. That trade-off favours delivery over
// suppression (a redundant push is preferable to a missed one) and
// keeps the publisher free of cross-replica presence tracking.
type WebPushPublisher struct {
	inner WSPublisher
	conns ConnectionChecker
	push  PushSender
	wg    *sync.WaitGroup
}

// NewWebPushPublisher wraps inner so notifications also fan out via
// Web Push for offline users. conns may be nil (every user is then
// treated as offline); push must be non-nil for the wrapper to add
// value, but a nil push degrades to plain inner-publish behaviour.
//
// conns and push are normalised through typednil.IsTypedNil: a caller
// that passes a typed-nil concrete value (e.g. a nil *WebPushService
// wrapped in the PushSender interface) is treated as the plain-nil
// case, so the `p.push == nil` short-circuit in Publish actually fires
// instead of spawning a goroutine that calls into a nil receiver. This
// matches the With* setter convention used elsewhere in the codebase
// (api/drive, internal/ai).
func NewWebPushPublisher(inner WSPublisher, conns ConnectionChecker, push PushSender) *WebPushPublisher {
	if typednil.IsTypedNil(conns) {
		conns = nil
	}
	if typednil.IsTypedNil(push) {
		push = nil
	}
	return &WebPushPublisher{inner: inner, conns: conns, push: push}
}

// WithWaitGroup registers wg so each detached Web Push goroutine is
// tracked (Add before launch, Done on return). Wiring the server's
// background-goroutine WaitGroup in lets graceful shutdown drain
// in-flight push deliveries before the database pool is closed —
// otherwise a push that hit a 410 mid-shutdown would call
// DeleteSubscription against an already-closed pool. The per-delivery
// pushDeliveryTimeout still bounds how long the drain can block.
func (p *WebPushPublisher) WithWaitGroup(wg *sync.WaitGroup) *WebPushPublisher {
	p.wg = wg
	return p
}

// Publish delegates to the inner publisher, then best-effort delivers
// a Web Push message to offline recipients. The inner publish error
// (if any) is returned; push failures are swallowed by the service's
// own logging so a push-service outage never masks the WS result.
//
// Web Push delivery runs in a detached goroutine: it performs blocking
// HTTPS POSTs to external push services (hundreds of ms per device),
// and the synchronous WebSocket publish is what the notification HTTP
// handler actually waits on. Returning before the push completes keeps
// that handler's latency independent of the push services' health. The
// goroutine uses a cancellation-detached copy of ctx (so it survives
// the request returning) with its own timeout, and still honours the
// request's logger/trace values.
func (p *WebPushPublisher) Publish(ctx context.Context, workspaceID, userID uuid.UUID, event Event) error {
	var innerErr error
	if p.inner != nil {
		innerErr = p.inner.Publish(ctx, workspaceID, userID, event)
	}
	if p.push == nil {
		return innerErr
	}
	if p.conns != nil && p.conns.IsConnected(workspaceID, userID) {
		return innerErr
	}
	payload, ok := pushPayloadFromEvent(event)
	if !ok {
		return innerErr
	}
	pushCtx := context.WithoutCancel(ctx)
	// Add before launching (not inside the goroutine) so a concurrent
	// Wait at shutdown cannot observe the counter at zero between the
	// `go` statement and the goroutine actually starting. The HTTP
	// server is drained before bgGoroutines.Wait runs, so no Publish —
	// hence no Add — races a Wait that has already begun.
	if p.wg != nil {
		p.wg.Add(1)
	}
	go func() {
		if p.wg != nil {
			defer p.wg.Done()
		}
		ctx, cancel := context.WithTimeout(pushCtx, pushDeliveryTimeout)
		defer cancel()
		if err := p.push.Send(ctx, workspaceID, userID, payload); err != nil {
			logging.FromContext(ctx).Error("notification web push failed",
				"workspace_id", workspaceID, "user_id", userID, "err", err)
		}
	}()
	return innerErr
}

// pushPayloadFromEvent extracts a NotificationPayload from a
// "notification" event. Returns ok=false for other event types (only
// notifications are surfaced as browser push messages).
func pushPayloadFromEvent(event Event) (NotificationPayload, bool) {
	if event.Type != "notification" {
		return NotificationPayload{}, false
	}
	n, ok := event.Payload.(*Notification)
	if !ok || n == nil {
		return NotificationPayload{}, false
	}
	return NotificationPayload{
		Title: n.Title,
		Body:  n.Body,
		Type:  n.Type,
		URL:   deepLinkFor(n),
	}, true
}

// deepLinkFor maps a notification's resource to the SPA path the service
// worker should open when the user clicks the push. It returns "" when
// the resource has no dedicated route, in which case the service worker
// falls back to /drive (see frontend/public/push-sw.js) — so an empty
// result is the safe default, never a broken link.
//
// Only resource types that map to a real frontend route (App.tsx) and
// whose ResourceID is the ID that route expects are linked here.
// Today's notification resource types (share_link, guest_invite,
// file_version) carry the *event* id (link / invite / version), not a
// folder or document id, and the SPA has no route to view those by id,
// so they intentionally fall through to the /drive default. Wiring the
// URL end-to-end means the moment a notification references a folder or
// document the click lands on it with no further plumbing.
//
// Contract with the service worker: every value returned here MUST be an
// SPA-relative path (leading "/", no scheme or host). push-sw.js's
// notificationclick handler matches an already-open tab by comparing this
// value to new URL(client.url).pathname, so returning an absolute URL
// (e.g. "https://host/drive/...") would never match and would force a
// redundant navigation. Keep new cases returning bare paths only.
func deepLinkFor(n *Notification) string {
	if n == nil || n.ResourceType == nil || n.ResourceID == nil {
		return ""
	}
	id := n.ResourceID.String()
	switch *n.ResourceType {
	case "folder":
		return "/drive/folder/" + id
	case "document":
		return "/drive/document/" + id
	default:
		// No dedicated route for this resource type — let the service
		// worker apply its /drive fallback rather than emit a link that
		// would 404 or land on an unrelated page.
		return ""
	}
}

func parseChannel(channel string) (uuid.UUID, uuid.UUID, error) {
	parts := strings.SplitN(channel, ":", 3)
	if len(parts) != 3 || parts[0] != "ws" {
		return uuid.Nil, uuid.Nil, fmt.Errorf("unexpected channel format")
	}
	workspaceID, err := uuid.Parse(parts[1])
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("workspace id: %w", err)
	}
	userID, err := uuid.Parse(parts[2])
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("user id: %w", err)
	}
	return workspaceID, userID, nil
}
