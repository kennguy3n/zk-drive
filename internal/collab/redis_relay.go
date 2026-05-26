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

// originIDByteLen is the size of the per-relay origin tag
// prepended to every published frame. It matches uuid.UUID's wire
// size and is used by the subscribe loop to recognise — and drop
// — frames the same replica just published. Without this tag the
// publishing replica would receive its own broadcasts back via
// Redis pub/sub fan-out (a publisher subscribed to the same
// pattern always sees its own messages) and re-deliver them to
// the originating room, causing duplicate frame delivery to every
// local client and visible UI flicker on awareness updates.
const originIDByteLen = 16

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
	// originID is a per-process random tag prepended to every
	// publish and used by Subscribe to drop the publisher's own
	// echoes. Stored as the raw 16-byte representation so the
	// per-publish overhead is one append-to-slice, not a UUID
	// formatting round-trip.
	originID [originIDByteLen]byte
}

// NewRedisCollabRelay returns a relay backed by rdb. The relay
// does not own rdb; the caller closes it during shutdown.
//
// A nil rdb returns a nil relay, which the hub treats as
// "single-replica mode" — every CollabRelayPublisher call site
// nil-checks first. cmd/server/main.go nil-checks this before
// calling DocumentHub.WithRelay (typed-nil interface guard).
//
// Each relay instance receives a fresh random originID. Two relay
// objects in the same process — e.g. one created by the
// production wiring and one created by a parallel integration
// test in the same binary — therefore see each other's frames as
// "from elsewhere" and deliver them normally, which is the
// desired semantics: each relay represents a logical replica.
func NewRedisCollabRelay(rdb *redis.Client) *RedisCollabRelay {
	if rdb == nil {
		return nil
	}
	r := &RedisCollabRelay{rdb: rdb}
	// uuid.New() panics only on a CSPRNG failure, which would
	// also break the rest of the server's bootstrap (JWT
	// signing, password hashing). Letting it propagate as a
	// panic during startup is the right behaviour.
	r.originID = [originIDByteLen]byte(uuid.New())
	return r
}

// OriginID returns the relay's per-process origin tag. Exported
// so multi-replica tests can assert that frames published by one
// relay are delivered to a peer relay (but NOT echoed back to the
// publisher's own subscribe loop).
func (r *RedisCollabRelay) OriginID() [originIDByteLen]byte {
	if r == nil {
		return [originIDByteLen]byte{}
	}
	return r.originID
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
// framed payload to the per-document Redis channel, prefixed
// with the relay's 16-byte originID so the subscribe loop on the
// same relay can recognise and drop the echo.
//
// A nil receiver is a no-op (single-replica fallback). The wire
// shape on Redis is `originID(16) || payload(N)`.
func (r *RedisCollabRelay) PublishFrame(ctx context.Context, documentID uuid.UUID, payload []byte) error {
	if r == nil || r.rdb == nil {
		return nil
	}
	// Build a fresh slice rather than mutating payload: callers
	// (the hub) re-use the same frame buffer for the local
	// broadcast and the relay publish, so prepending in-place
	// would corrupt the local fan-out path.
	tagged := make([]byte, 0, originIDByteLen+len(payload))
	tagged = append(tagged, r.originID[:]...)
	tagged = append(tagged, payload...)
	if err := r.rdb.Publish(ctx, channelForCollab(documentID), tagged).Err(); err != nil {
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
			payload := []byte(msg.Payload)
			if len(payload) < originIDByteLen {
				// A frame shorter than the origin tag is
				// malformed (e.g. a foreign producer
				// publishing directly to collab:* without the
				// origin prefix). Drop with a warn-log so
				// the bug is visible at the receiver rather
				// than producing a confusing decode failure
				// in the hub layer.
				logging.FromContext(ctx).Warn("collab redis drop message: payload shorter than origin tag", "channel", msg.Channel, "len", len(payload))
				continue
			}
			// Drop our own echo: Redis pub/sub delivers a
			// publisher's frames back to the publisher when
			// the publisher is also subscribed to a matching
			// pattern (which we always are, via PSubscribe
			// collab:*). Without this check every local
			// broadcast would be re-delivered to the same
			// replica via BroadcastFromRelay, doubling the
			// frame count to every client in the room and
			// causing awareness flicker.
			if [originIDByteLen]byte(payload[:originIDByteLen]) == r.originID {
				continue
			}
			consumer.BroadcastFromRelay(documentID, payload[originIDByteLen:])
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
