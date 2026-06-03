package notification

import (
	"context"
	"encoding/json"
	"fmt"

	"strings"

	"github.com/kennguy3n/zk-drive/internal/logging"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

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
}

// NewWebPushPublisher wraps inner so notifications also fan out via
// Web Push for offline users. conns may be nil (every user is then
// treated as offline); push must be non-nil for the wrapper to add
// value, but a nil push degrades to plain inner-publish behaviour.
func NewWebPushPublisher(inner WSPublisher, conns ConnectionChecker, push PushSender) *WebPushPublisher {
	return &WebPushPublisher{inner: inner, conns: conns, push: push}
}

// Publish delegates to the inner publisher, then best-effort delivers
// a Web Push message to offline recipients. The inner publish error
// (if any) is returned; push failures are swallowed by the service's
// own logging so a push-service outage never masks the WS result.
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
	if payload, ok := pushPayloadFromEvent(event); ok {
		if err := p.push.Send(ctx, workspaceID, userID, payload); err != nil {
			logging.FromContext(ctx).Error("notification web push failed",
				"workspace_id", workspaceID, "user_id", userID, "err", err)
		}
	}
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
	}, true
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
