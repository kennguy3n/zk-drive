package notification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/kennguy3n/zk-drive/internal/logging"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/google/uuid"
)

// ErrInvalidSubscription marks a push subscription the caller supplied
// as unacceptable: missing fields, an over-long endpoint, or an
// endpoint that fails SSRF validation (non-https / private / blocked
// address). It is a client-caused error, so the HTTP layer maps it to
// 400 Bad Request rather than logging a 500 — see writeServiceError in
// api/drive/helpers.go. Validation failures are wrapped with this
// sentinel via fmt.Errorf("%w: ...").
var ErrInvalidSubscription = errors.New("webpush: invalid subscription")

// maxPushEndpointLen bounds the stored endpoint URL. Real push-service
// endpoints (FCM, Mozilla autopush, Apple) run a few hundred bytes; a
// 2 KiB ceiling leaves generous headroom while stopping an
// authenticated client from persisting arbitrarily large strings
// (storage abuse). Enforced here for a clean 400 and mirrored by a
// CHECK constraint in migration 038 as defence in depth.
const maxPushEndpointLen = 2048

// defaultVAPIDSubscriber is the `sub` claim embedded in the VAPID JWT
// when the operator does not configure one explicitly. Push services
// require a mailto: or https: subscriber so they can contact the
// application-server operator about a misbehaving sender.
const defaultVAPIDSubscriber = "mailto:ops@zk-drive.example.com"

// pushTTLSeconds is the Time-To-Live the push service holds an
// undelivered message for. The whole point of Web Push (vs the live
// WebSocket path) is to reach a user whose device is briefly offline
// — laptop asleep, phone in a tunnel — so a 30s TTL defeated the
// feature. One day keeps a notification deliverable across an
// overnight-offline window while still letting the push service expire
// truly stale messages. The notification is persisted in Postgres
// regardless, so this only governs the out-of-band push copy.
const pushTTLSeconds = 24 * 60 * 60

// PushSubscription mirrors the browser PushSubscription shape the
// frontend POSTs to /api/push/subscribe. p256dh and auth are the
// base64url-encoded keys returned by PushSubscription.getKey(); they
// feed the RFC 8291 payload encryption performed by webpush-go.
type PushSubscription struct {
	Endpoint string `json:"endpoint"`
	P256dh   string `json:"p256dh"`
	Auth     string `json:"auth"`
}

// NotificationPayload is the JSON body delivered to the service
// worker's `push` event listener. The frontend reads Title / Body to
// call self.registration.showNotification, and uses URL (when set) to
// focus / open the relevant page on notificationclick.
type NotificationPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Type  string `json:"type,omitempty"`
	URL   string `json:"url,omitempty"`
}

// WebPushRepository persists browser push subscriptions. The Postgres
// implementation lives alongside the other notification repositories
// in repository.go.
type WebPushRepository interface {
	// SaveSubscription upserts a subscription for (workspace, user,
	// endpoint). Re-registering the same endpoint refreshes its keys
	// rather than creating a duplicate row.
	SaveSubscription(ctx context.Context, workspaceID, userID uuid.UUID, sub PushSubscription) error
	// DeleteSubscription removes a single subscription by endpoint.
	DeleteSubscription(ctx context.Context, workspaceID, userID uuid.UUID, endpoint string) error
	// ListSubscriptions returns every push subscription registered for
	// (workspace, user) — a user with the PWA on multiple devices has
	// multiple rows.
	ListSubscriptions(ctx context.Context, workspaceID, userID uuid.UUID) ([]PushSubscription, error)
}

// httpDoer is the subset of *http.Client webpush-go needs. Declared so
// tests can inject a stub that records requests and returns canned
// responses (e.g. 410 Gone) without hitting a real push service.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// EndpointValidator performs DNS-resolving SSRF validation of a push
// endpoint URL. It is satisfied by *webhooks.URLValidator, injected by
// the server wiring via WithEndpointValidator. Defining it here (rather
// than importing the webhooks package) keeps the notification package
// free of that dependency and lets tests supply a fake-resolver
// validator. Validate is called at subscribe time and again before each
// delivery: re-resolving on every send is the DNS-rebinding defence —
// a hostname that resolved to a public IP at subscribe time but is
// later repointed at 169.254.169.254 is caught before we POST to it.
type EndpointValidator interface {
	Validate(ctx context.Context, raw string) (*url.URL, error)
}

// WebPushService delivers RFC 8030 / VAPID web-push messages to a
// user's registered browser subscriptions. It is constructed only
// when both VAPID keys are configured; callers treat a nil service as
// "web push disabled" (every method is a nil-safe no-op).
type WebPushService struct {
	repo            WebPushRepository
	vapidPublicKey  string
	vapidPrivateKey string
	subscriber      string
	httpClient      httpDoer
	validator       EndpointValidator
}

// NewWebPushService returns a service that signs push messages with
// the supplied VAPID key pair. Returns nil when either key is empty so
// the caller's `if svc != nil` guards engage the graceful-degradation
// path (the /api/push endpoints respond 501 and the publisher skips
// the push fan-out).
func NewWebPushService(repo WebPushRepository, vapidPublicKey, vapidPrivateKey string) *WebPushService {
	if repo == nil || vapidPublicKey == "" || vapidPrivateKey == "" {
		return nil
	}
	return &WebPushService{
		repo:            repo,
		vapidPublicKey:  vapidPublicKey,
		vapidPrivateKey: vapidPrivateKey,
		subscriber:      defaultVAPIDSubscriber,
	}
}

// WithSubscriber overrides the VAPID `sub` claim (a mailto: or https:
// URI identifying the operator). Empty values are ignored so the
// default subscriber stays in place. Fluent so it composes with the
// constructor.
func (s *WebPushService) WithSubscriber(sub string) *WebPushService {
	if s != nil && sub != "" {
		s.subscriber = sub
	}
	return s
}

// WithHTTPClient injects the HTTP client used to POST encrypted
// payloads to push endpoints. Primarily a test seam; production wiring
// leaves it nil so webpush-go uses its default *http.Client.
func (s *WebPushService) WithHTTPClient(c httpDoer) *WebPushService {
	if s != nil {
		s.httpClient = c
	}
	return s
}

// WithEndpointValidator injects the DNS-resolving SSRF validator used
// to vet push endpoints at subscribe and delivery time. Production
// wires a *webhooks.URLValidator; when nil the service falls back to
// the lightweight literal-IP checks in validatePushEndpoint (which do
// not resolve DNS).
func (s *WebPushService) WithEndpointValidator(v EndpointValidator) *WebPushService {
	if s != nil {
		s.validator = v
	}
	return s
}

// PublicKey returns the VAPID public key the frontend needs to pass as
// applicationServerKey when calling pushManager.subscribe.
func (s *WebPushService) PublicKey() string {
	if s == nil {
		return ""
	}
	return s.vapidPublicKey
}

// Subscribe stores (or refreshes) a browser push subscription for the
// caller. A nil service is a no-op so handlers can call it
// unconditionally when web push is disabled.
func (s *WebPushService) Subscribe(ctx context.Context, workspaceID, userID uuid.UUID, sub PushSubscription) error {
	if s == nil {
		return nil
	}
	if sub.Endpoint == "" || sub.P256dh == "" || sub.Auth == "" {
		return fmt.Errorf("%w: endpoint, p256dh and auth are required", ErrInvalidSubscription)
	}
	if err := s.validateEndpoint(ctx, sub.Endpoint); err != nil {
		return err
	}
	return s.repo.SaveSubscription(ctx, workspaceID, userID, sub)
}

// validateEndpoint enforces the length bound and runs SSRF validation,
// returning every failure wrapped in ErrInvalidSubscription so the HTTP
// layer answers 400 (not 500). When an EndpointValidator is injected it
// does the full DNS-resolving check; otherwise it falls back to the
// literal-IP checks. The detailed validator reason is logged (so an
// operator can spot SSRF probes) but not echoed to the client.
func (s *WebPushService) validateEndpoint(ctx context.Context, endpoint string) error {
	if len(endpoint) > maxPushEndpointLen {
		return fmt.Errorf("%w: endpoint exceeds %d bytes", ErrInvalidSubscription, maxPushEndpointLen)
	}
	if s.validator != nil {
		if _, err := s.validator.Validate(ctx, endpoint); err != nil {
			logging.FromContext(ctx).Warn("webpush rejected endpoint", "err", err)
			return fmt.Errorf("%w: endpoint not allowed", ErrInvalidSubscription)
		}
		return nil
	}
	if err := validatePushEndpoint(endpoint); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidSubscription, err.Error())
	}
	return nil
}

// validatePushEndpoint rejects endpoints that are not plausible public
// push-service URLs. The endpoint is attacker-controlled (any logged-in
// client picks it), and the server later POSTs to it on every
// notification, so an unvalidated endpoint turns the server into a
// blind SSRF probe against internal addresses (cloud metadata at
// 169.254.169.254, loopback, RFC 1918). We require https and block
// literal loopback / private / link-local / unspecified IPs and
// localhost. Note: this does not resolve DNS, so a hostname pointing
// at an internal IP is not caught here — defence against that belongs
// in a custom http.Transport dialer (tracked as a follow-up); blocking
// the literal-IP vectors closes the cheap, obvious holes.
func validatePushEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("webpush: invalid endpoint url: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("webpush: endpoint must be an https url")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("webpush: endpoint url missing host")
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return fmt.Errorf("webpush: endpoint host not allowed")
	}
	if ip := net.ParseIP(host); ip != nil && isDisallowedIP(ip) {
		return fmt.Errorf("webpush: endpoint host not allowed")
	}
	return nil
}

// isDisallowedIP reports whether ip is in a range a push endpoint must
// never target: loopback, RFC 1918 private, link-local (incl. the
// 169.254.0.0/16 cloud-metadata range), unspecified, or multicast.
func isDisallowedIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}

// Unsubscribe removes a single subscription identified by its push
// endpoint.
func (s *WebPushService) Unsubscribe(ctx context.Context, workspaceID, userID uuid.UUID, endpoint string) error {
	if s == nil {
		return nil
	}
	if endpoint == "" {
		return fmt.Errorf("%w: endpoint is required", ErrInvalidSubscription)
	}
	return s.repo.DeleteSubscription(ctx, workspaceID, userID, endpoint)
}

// Send delivers payload to every push subscription registered for
// (workspace, user) via a VAPID-signed, RFC 8291-encrypted message.
// Per-subscription failures are logged and do not abort the fan-out;
// a 410 Gone (or 404 Not Found) response means the browser expired the
// subscription, so the row is auto-removed. A nil service is a no-op.
func (s *WebPushService) Send(ctx context.Context, workspaceID, userID uuid.UUID, payload NotificationPayload) error {
	if s == nil {
		return nil
	}
	subs, err := s.repo.ListSubscriptions(ctx, workspaceID, userID)
	if err != nil {
		return fmt.Errorf("webpush: list subscriptions: %w", err)
	}
	if len(subs) == 0 {
		return nil
	}
	message, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("webpush: marshal payload: %w", err)
	}
	log := logging.FromContext(ctx)
	for _, sub := range subs {
		status, err := s.deliver(ctx, message, sub)
		if err != nil {
			log.Error("webpush delivery failed",
				"workspace_id", workspaceID, "user_id", userID, "err", err)
			continue
		}
		// 410 Gone / 404 Not Found: the push service has expired the
		// subscription. Remove it so we stop trying on every publish.
		if status == http.StatusGone || status == http.StatusNotFound {
			if derr := s.repo.DeleteSubscription(ctx, workspaceID, userID, sub.Endpoint); derr != nil {
				log.Error("webpush prune expired subscription failed",
					"workspace_id", workspaceID, "user_id", userID, "err", derr)
			}
		}
	}
	return nil
}

// deliver sends a single encrypted push message and returns the push
// service's HTTP status code. The response body is always drained and
// closed so the underlying connection can be reused.
func (s *WebPushService) deliver(ctx context.Context, message []byte, sub PushSubscription) (int, error) {
	// Re-validate (re-resolving DNS) immediately before sending so a
	// subscription whose hostname was repointed at an internal address
	// after it was stored is never POSTed to. Subscriptions are stored
	// pre-validated, so this only trips on a deliberate DNS rebind.
	if s.validator != nil {
		if _, err := s.validator.Validate(ctx, sub.Endpoint); err != nil {
			return 0, fmt.Errorf("webpush: endpoint failed pre-send revalidation: %w", err)
		}
	}
	opts := &webpush.Options{
		Subscriber:      s.subscriber,
		VAPIDPublicKey:  s.vapidPublicKey,
		VAPIDPrivateKey: s.vapidPrivateKey,
		TTL:             pushTTLSeconds,
	}
	if s.httpClient != nil {
		opts.HTTPClient = s.httpClient
	}
	resp, err := webpush.SendNotificationWithContext(ctx, message, &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys: webpush.Keys{
			P256dh: sub.P256dh,
			Auth:   sub.Auth,
		},
	}, opts)
	if err != nil {
		return 0, err
	}
	// Drain then close so the keep-alive connection can be reused for
	// the next device in the fan-out regardless of response size.
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	return resp.StatusCode, nil
}
