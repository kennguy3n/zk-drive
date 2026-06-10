package notification

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/kennguy3n/zk-drive/internal/logging"
	"github.com/kennguy3n/zk-drive/internal/typednil"

	"github.com/google/uuid"
)

// maxDeviceTokenLen bounds the device token a client may register. Real
// APNs tokens are ~64 hex chars and FCM tokens ~163+ chars; 4 KiB is
// generous headroom while stopping an authenticated client from
// persisting an arbitrarily large string. Mirrors the
// length(token) <= 4096 CHECK in migration 039 (this is the
// application-layer guard that rejects with a 400 before the DB
// constraint would).
const maxDeviceTokenLen = 4096

// ErrInvalidDeviceToken marks a device registration the caller supplied
// that fails validation (unknown platform, empty / over-long token). The
// drive handler maps it to a 400; see api/drive/helpers.go.
var ErrInvalidDeviceToken = errors.New("mobilepush: invalid device token")

// httpStatusErr carries an unexpected HTTP status from a push provider
// so MobilePushService can log the code without the provider needing to
// format a message. Providers return it for any status that is neither
// "delivered" nor "token is permanently dead".
type httpStatusErr struct {
	provider string
	status   int
	detail   string
}

func (e *httpStatusErr) Error() string {
	if e.detail != "" {
		return fmt.Sprintf("%s push: unexpected status %d: %s", e.provider, e.status, e.detail)
	}
	return fmt.Sprintf("%s push: unexpected status %d", e.provider, e.status)
}

// MobilePushProvider delivers a single notification to one device token
// over a platform push transport (APNs or FCM). Implementations are
// FCMProvider and APNsProvider; the server wires whichever ones are
// configured into the MobilePushService.
//
// Send reports dead=true when the provider told us the token is
// permanently invalid (APNs 410 Unregistered / 400 BadDeviceToken, FCM
// 404 UNREGISTERED / INVALID_ARGUMENT) so the service prunes it; in that
// case err is nil. A non-nil err is a transient / configuration failure
// (network, 5xx, auth) that must NOT prune the token — it is logged and
// the token is retried on the next notification.
type MobilePushProvider interface {
	// Platform returns the platform this provider serves, used to route
	// device tokens to it.
	Platform() Platform
	// Send delivers payload to a single token. See the interface doc for
	// the (dead, err) contract.
	Send(ctx context.Context, token string, payload NotificationPayload) (dead bool, err error)
}

// MobilePushService fans a notification out to a user's registered
// native devices via the configured per-platform providers, pruning
// tokens a provider reports as permanently dead. It is the mobile
// analogue of WebPushService.
//
// A nil *MobilePushService means "mobile push disabled": every method is
// a nil-safe no-op and the register endpoint responds 501. The service
// is also considered disabled when no providers are configured.
type MobilePushService struct {
	repo      DeviceTokenRepository
	providers map[Platform]MobilePushProvider
}

// NewMobilePushService returns a service backed by repo. Providers are
// added with WithProvider. Returns nil when repo is nil so callers'
// `if svc != nil` guards engage the disabled path.
func NewMobilePushService(repo DeviceTokenRepository) *MobilePushService {
	if typednil.IsTypedNil(repo) {
		return nil
	}
	return &MobilePushService{
		repo:      repo,
		providers: make(map[Platform]MobilePushProvider),
	}
}

// WithProvider registers p under its Platform(). A typed-nil provider
// (e.g. a nil *FCMProvider wrapped in the interface, returned when FCM is
// unconfigured) is ignored so SupportsPlatform reports it correctly and
// the fan-out never dispatches to a nil receiver. Fluent so it composes
// with the constructor and repeated calls.
func (s *MobilePushService) WithProvider(p MobilePushProvider) *MobilePushService {
	if s == nil || typednil.IsTypedNil(p) {
		return s
	}
	s.providers[p.Platform()] = p
	return s
}

// Enabled reports whether at least one provider is configured. The
// server uses it to decide whether to wrap the publisher and expose the
// register endpoint.
func (s *MobilePushService) Enabled() bool {
	return s != nil && len(s.providers) > 0
}

// SupportsPlatform reports whether a provider is configured for p. The
// register handler uses it to 501 a token whose platform has no
// configured provider (so the client learns push won't work) rather than
// persisting a token nothing can ever deliver to.
func (s *MobilePushService) SupportsPlatform(p Platform) bool {
	if s == nil {
		return false
	}
	_, ok := s.providers[p]
	return ok
}

// Register validates and persists a device token for (workspace, user).
// Validation failures are wrapped in ErrInvalidDeviceToken (→ 400);
// an unsupported platform returns ErrPlatformUnsupported (→ 501).
func (s *MobilePushService) Register(ctx context.Context, workspaceID, userID uuid.UUID, dt DeviceToken) error {
	if s == nil {
		return ErrPlatformUnsupported
	}
	if !dt.Platform.Valid() {
		return fmt.Errorf("%w: unknown platform %q", ErrInvalidDeviceToken, dt.Platform)
	}
	if dt.Token == "" {
		return fmt.Errorf("%w: token is required", ErrInvalidDeviceToken)
	}
	if len(dt.Token) > maxDeviceTokenLen {
		return fmt.Errorf("%w: token exceeds %d bytes", ErrInvalidDeviceToken, maxDeviceTokenLen)
	}
	if !s.SupportsPlatform(dt.Platform) {
		return fmt.Errorf("%w: %s", ErrPlatformUnsupported, dt.Platform)
	}
	if err := s.repo.SaveDeviceToken(ctx, workspaceID, userID, dt); err != nil {
		return fmt.Errorf("mobilepush: register device: %w", err)
	}
	return nil
}

// Unregister removes a device token for (workspace, user). Idempotent:
// removing an unknown token returns nil. Validation mirrors Register so a
// malformed unregister is a 400, not a silent no-op.
//
// Unlike Register it deliberately does NOT call SupportsPlatform: a user
// who registered while a provider was configured must still be able to
// unregister after that provider is removed, so cleanup never depends on
// current provider configuration.
func (s *MobilePushService) Unregister(ctx context.Context, workspaceID, userID uuid.UUID, dt DeviceToken) error {
	if s == nil {
		return ErrPlatformUnsupported
	}
	if !dt.Platform.Valid() {
		return fmt.Errorf("%w: unknown platform %q", ErrInvalidDeviceToken, dt.Platform)
	}
	if dt.Token == "" {
		return fmt.Errorf("%w: token is required", ErrInvalidDeviceToken)
	}
	if err := s.repo.DeleteDeviceToken(ctx, workspaceID, userID, dt); err != nil {
		return fmt.Errorf("mobilepush: unregister device: %w", err)
	}
	return nil
}

// ErrPlatformUnsupported is returned when a device registration targets a
// platform with no configured provider. The handler maps it to 501.
var ErrPlatformUnsupported = errors.New("mobilepush: platform not configured")

// Send fans payload out to every registered device of (workspace, user)
// whose platform has a configured provider, pruning any token a provider
// reports as permanently dead. It is fail-soft: a provider error is
// logged and the remaining tokens are still attempted, so one bad token
// or a single provider outage never blocks the others. Returns an error
// only when the token lookup itself fails (a real persistence problem
// the caller should see); per-token delivery failures are swallowed
// after logging, exactly like WebPushService.Send.
func (s *MobilePushService) Send(ctx context.Context, workspaceID, userID uuid.UUID, payload NotificationPayload) error {
	if s == nil || len(s.providers) == 0 {
		return nil
	}
	tokens, err := s.repo.ListDeviceTokens(ctx, workspaceID, userID)
	if err != nil {
		return fmt.Errorf("mobilepush: list device tokens: %w", err)
	}
	if len(tokens) == 0 {
		return nil
	}
	log := logging.FromContext(ctx)
	for _, dt := range tokens {
		provider, ok := s.providers[dt.Platform]
		if !ok {
			// A token whose provider was removed/unconfigured: nothing can
			// deliver it. Leave it in place (the provider may be reconfigured)
			// rather than pruning a token the user might still want.
			continue
		}
		dead, err := provider.Send(ctx, dt.Token, payload)
		if err != nil {
			log.Error("mobile push delivery failed",
				"workspace_id", workspaceID, "user_id", userID,
				"platform", dt.Platform, "err", err)
			continue
		}
		if dead {
			if derr := s.repo.DeleteDeviceToken(ctx, workspaceID, userID, dt); derr != nil {
				log.Error("mobile push prune dead token failed",
					"workspace_id", workspaceID, "user_id", userID,
					"platform", dt.Platform, "err", derr)
			}
		}
	}
	return nil
}

// payloadData renders a NotificationPayload's optional metadata as the
// string→string data map both providers carry alongside the visible
// alert (FCM `data`, APNs custom keys). The native app reads these to
// deep-link and de-duplicate, mirroring the fields the Web Push service
// worker consumes. Empty fields are omitted so the map stays minimal.
func payloadData(payload NotificationPayload) map[string]string {
	data := make(map[string]string, 3)
	if payload.Type != "" {
		data["type"] = payload.Type
	}
	if payload.URL != "" {
		data["url"] = payload.URL
	}
	if payload.Tag != "" {
		data["tag"] = payload.Tag
	}
	if len(data) == 0 {
		return nil
	}
	return data
}

// drainAndClose drains and closes a response body so the underlying
// keep-alive connection can be reused. Shared by the providers.
func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	// Limit the drain so a misbehaving provider returning a huge error
	// body cannot make us read it all just to reuse the connection.
	_, _ = io.CopyN(io.Discard, resp.Body, 4<<10)
	_ = resp.Body.Close()
}
