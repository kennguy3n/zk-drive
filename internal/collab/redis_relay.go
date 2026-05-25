package collab

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/zk-drive/internal/logging"
)

// RedisCollabRelay multiplexes per-document collab frames across
// replicas via Redis pub/sub. The wiring mirrors
// notification.RedisPublisher and changefeed.RedisPublisher:
//
//   - PublishFrame: invoked by the local DocumentHub after a
//     successful local broadcast; publishes the framed payload
//     to `collab:{documentID}` so every other replica subscribed
//     to `collab:*` receives it.
//   - Subscribe: started in a dedicated goroutine at server
//     startup; reads from `collab:*` and calls
//     hub.BroadcastFromRelay on every received message.
//
// Channel format: `collab:{documentID}`. The colon-delimited
// scheme matches the workspace/user pattern used by the
// notification publisher so all live-WS Redis traffic is grouped
// under inspectable prefixes.
//
// We deliberately publish the FULL framed payload (i.e. the same
// bytes the local broadcast pumped into client.send) rather than
// the un-framed update bytes — this means the subscribe path on
// the other replica doesn't need to re-frame, which keeps the
// subscribe loop trivial and ensures wire-format consistency
// across replicas. The trade-off is one extra type byte (or two
// for sync updates) per Redis publish; at the volumes this is
// expected to operate (a 4KB Y.Update per keystroke is high
// end), the overhead is rounding error.
type RedisCollabRelay struct {
	rdb *redis.Client
}

// NewRedisCollabRelay returns a relay backed by rdb. The relay
// does not own rdb; the caller closes it during shutdown.
//
// A nil rdb returns a nil relay, which the hub treats as
// "single-replica mode" — every CollabRelayPublisher call site
// nil-checks first. This makes the wiring code in
// cmd/server/main.go a clean `hub.WithRelay(maybeNilRelay)`.
func NewRedisCollabRelay(rdb *redis.Client) *RedisCollabRelay {
	if rdb == nil {
		return nil
	}
	return &RedisCollabRelay{rdb: rdb}
}

// channelForCollab returns the Redis pub/sub channel for a given
// document. Public so tests can reach in and PUBLISH directly to
// verify the subscriber path without going through the hub.
func channelForCollab(documentID uuid.UUID) string {
	return "collab:" + documentID.String()
}

// collabChannelPattern is the psubscribe pattern that the relay's
// Subscribe loop consumes — every document, every workspace.
const collabChannelPattern = "collab:*"

// PublishFrame implements CollabRelayPublisher. It writes the
// framed payload to the per-document Redis channel. A nil
// receiver is a no-op (single-replica fallback).
func (r *RedisCollabRelay) PublishFrame(ctx context.Context, documentID uuid.UUID, payload []byte) error {
	if r == nil || r.rdb == nil {
		return nil
	}
	if err := r.rdb.Publish(ctx, channelForCollab(documentID), payload).Err(); err != nil {
		return fmt.Errorf("collab: redis publish: %w", err)
	}
	return nil
}

// CollabRelayConsumer is the subset of *DocumentHub that the
// relay's Subscribe loop needs. The relay package depends on the
// abstraction; the concrete hub is wired in cmd/server/main.go.
type CollabRelayConsumer interface {
	BroadcastFromRelay(documentID uuid.UUID, payload []byte)
}

// Subscribe runs the Redis subscribe loop, forwarding every
// received message to consumer.BroadcastFromRelay(documentID,
// payload). Returns when ctx is canceled or the underlying
// Redis connection terminates fatally — callers typically run
// this in a dedicated goroutine.
//
// A nil receiver returns nil immediately. A nil consumer returns
// an error rather than silently dropping messages — the wiring
// bug should be visible at startup.
//
// Channel parsing is deliberately strict: an unparseable channel
// name (e.g. `collab:not-a-uuid`) is dropped and logged so a
// producer bug never panics the subscribe loop. Other replicas
// could conceivably PUBLISH directly to a custom collab:* channel
// for testing; the relay sheds those frames safely.
func (r *RedisCollabRelay) Subscribe(ctx context.Context, consumer CollabRelayConsumer) error {
	if r == nil || r.rdb == nil {
		return nil
	}
	if consumer == nil {
		return errors.New("collab: RedisCollabRelay.Subscribe consumer is nil")
	}
	sub := r.rdb.PSubscribe(ctx, collabChannelPattern)
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
			documentID, err := parseCollabChannel(msg.Channel)
			if err != nil {
				logging.FromContext(ctx).Warn("collab redis drop message: invalid channel", "channel", msg.Channel, "err", err)
				continue
			}
			consumer.BroadcastFromRelay(documentID, []byte(msg.Payload))
		}
	}
}

// parseCollabChannel pulls a UUID document ID out of a
// "collab:{uuid}" channel name. Returns an error rather than
// uuid.Nil so callers can distinguish a parse failure from a
// legitimate nil-UUID document (which shouldn't exist in
// practice but defense-in-depth is cheap).
func parseCollabChannel(channel string) (uuid.UUID, error) {
	rest, ok := strings.CutPrefix(channel, "collab:")
	if !ok {
		return uuid.Nil, fmt.Errorf("expected collab: prefix, got %q", channel)
	}
	id, err := uuid.Parse(rest)
	if err != nil {
		return uuid.Nil, fmt.Errorf("parse document id %q: %w", rest, err)
	}
	return id, nil
}
