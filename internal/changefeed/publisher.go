package changefeed

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/zk-drive/internal/logging"
)

// Event is the JSON envelope pushed over WebSocket. The Type is
// always "change" so frontends can switch on a single field across
// the multiplexed ws:* stream that already carries
// "notification" envelopes (see internal/notification.Event).
type Event struct {
	Type    string   `json:"type"`
	Payload Mutation `json:"payload"`
}

// WSPublisher publishes a change event to every live client of a
// workspace. The change feed broadcasts workspace-wide rather than
// per-user because every authenticated user in a workspace is
// entitled to see folder/file mutation metadata (the access-level
// filter happens later on the client side when it decides whether
// to fetch the content). For per-user-targeted notifications use
// internal/notification.WSPublisher.
type WSPublisher interface {
	Publish(ctx context.Context, workspaceID uuid.UUID, event Event) error
}

// WorkspaceBroadcaster is the subset of *ws.Hub the in-process
// publisher needs. The package depends on the abstraction; the
// concrete *ws.Hub is wired from cmd/server/main.go.
type WorkspaceBroadcaster interface {
	BroadcastJSONWorkspace(workspaceID uuid.UUID, payload []byte)
}

// LocalPublisher pushes events directly to a hub running in the same
// process. Used in single-replica deployments and when REDIS_URL is
// not configured.
type LocalPublisher struct {
	bc WorkspaceBroadcaster
}

// NewLocalPublisher returns a LocalPublisher that fans events to bc.
// A nil bc is allowed; Publish becomes a no-op so the service stays
// runnable in tests / development without a real hub.
func NewLocalPublisher(bc WorkspaceBroadcaster) *LocalPublisher {
	return &LocalPublisher{bc: bc}
}

// Publish marshals event and hands the bytes to the local hub.
func (p *LocalPublisher) Publish(_ context.Context, workspaceID uuid.UUID, event Event) error {
	if p == nil || p.bc == nil {
		return nil
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal change event: %w", err)
	}
	p.bc.BroadcastJSONWorkspace(workspaceID, payload)
	return nil
}

// channelFor returns the Redis pub/sub channel for workspace-wide
// changes. We use a different prefix ("ws-workspace:") than the
// per-user notification channel ("ws:") so the two streams are
// independently subscribable and can be migrated / partitioned
// independently in the future.
func channelFor(workspaceID uuid.UUID) string {
	return "ws-workspace:" + workspaceID.String()
}

// channelPattern is the psubscribe pattern matching every workspace
// channel.
const channelPattern = "ws-workspace:*"

// RedisPublisher publishes events to Redis pub/sub so every replica
// subscribed to channelPattern can fan them out to its local
// workspace clients.
type RedisPublisher struct {
	rdb *redis.Client
}

// NewRedisPublisher returns a publisher backed by rdb. The publisher
// does not own rdb; the caller is responsible for Close.
func NewRedisPublisher(rdb *redis.Client) *RedisPublisher {
	return &RedisPublisher{rdb: rdb}
}

// Publish marshals event and PUBLISHes the bytes to
// ws-workspace:{workspaceID}.
func (p *RedisPublisher) Publish(ctx context.Context, workspaceID uuid.UUID, event Event) error {
	if p == nil || p.rdb == nil {
		return nil
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal change event: %w", err)
	}
	if err := p.rdb.Publish(ctx, channelFor(workspaceID), payload).Err(); err != nil {
		return fmt.Errorf("redis publish change: %w", err)
	}
	return nil
}

// Subscribe psubscribes to channelPattern and forwards every received
// payload to bc.BroadcastJSONWorkspace. Blocks until ctx is cancelled
// or the Redis connection fatally terminates; callers run it in a
// dedicated goroutine.
//
// Channel parsing is strict — malformed names (anything that doesn't
// split into "ws-workspace:{workspaceUUID}") are dropped and logged
// so that a producer bug never panics the subscriber loop.
func (p *RedisPublisher) Subscribe(ctx context.Context, bc WorkspaceBroadcaster) error {
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
			workspaceID, err := parseChannel(msg.Channel)
			if err != nil {
				logging.FromContext(ctx).Warn("changefeed redis drop message: invalid channel",
					"channel", msg.Channel, "err", err)
				continue
			}
			bc.BroadcastJSONWorkspace(workspaceID, []byte(msg.Payload))
		}
	}
}

func parseChannel(channel string) (uuid.UUID, error) {
	const prefix = "ws-workspace:"
	if !strings.HasPrefix(channel, prefix) {
		return uuid.Nil, fmt.Errorf("changefeed: unexpected channel prefix")
	}
	id, err := uuid.Parse(channel[len(prefix):])
	if err != nil {
		return uuid.Nil, fmt.Errorf("changefeed: workspace id: %w", err)
	}
	return id, nil
}
