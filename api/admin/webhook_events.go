package admin

import (
	"context"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/webhooks"
)

// MemberEventPublisher is the narrow interface the admin handler
// depends on for outbound-webhook emission of `member.*` events.
// Defined here (in the consumer) rather than re-using the concrete
// *webhooks.Publisher directly so (a) integration tests can inject
// a fake publisher without standing up a real NATS/JetStream
// connection, and (b) the handler's surface area on the publisher
// is documented to exactly the single emit-helper it uses. The
// concrete *webhooks.Publisher in internal/webhooks satisfies this
// interface, so production wiring is unchanged.
//
// This mirrors the WebhookEventPublisher pattern in api/drive
// (file + permission events). The two interfaces are intentionally
// kept separate because the consumers have disjoint webhook needs:
// the drive handler never publishes member.* events, and the admin
// handler never publishes file.* or permission.* events. Forcing
// either consumer to depend on the union of methods would
// over-broaden the contract.
type MemberEventPublisher interface {
	PublishMemberEvent(ctx context.Context, t webhooks.EventType, workspaceID uuid.UUID, actorID *uuid.UUID, data webhooks.MemberEventData) error
}
