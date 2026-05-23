package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SignatureHeader is the canonical HTTP header name carrying the
// HMAC signature. Lowercased ascii so net/http's canonical-form
// rules keep it round-trip stable on both ends.
const SignatureHeader = "X-ZkDrive-Signature"

// EventIDHeader carries the Event.ID as the idempotency key the
// subscriber dedupes on.
const EventIDHeader = "X-ZkDrive-Event-Id"

// EventTypeHeader carries the dotted-namespace EventType so a
// subscriber can route to a per-type handler without having to parse
// the JSON body for the trivial case.
const EventTypeHeader = "X-ZkDrive-Event-Type"

// DeliveryIDHeader carries the per-attempt delivery ID. Distinct
// from EventIDHeader (which is stable across retries) — useful for
// the subscriber to correlate a specific attempt with our admin UI's
// delivery history view.
const DeliveryIDHeader = "X-ZkDrive-Delivery-Id"

// timestampToleranceWindow is the maximum acceptable skew between the
// signing timestamp and the subscriber's clock. Five minutes mirrors
// Stripe's default (https://stripe.com/docs/webhooks/signatures);
// short enough to make replay-by-rewind impractical, long enough to
// tolerate NTP-corrected clock drift.
const timestampToleranceWindow = 5 * time.Minute

// Signer computes the HMAC-SHA256 signature for an outbound payload.
// A zero-value Signer is unusable — callers MUST go through
// NewSigner which validates the secret meets the minimum length the
// migration's CHECK constraint enforces.
type Signer struct {
	secret []byte
}

// NewSigner constructs a Signer from a per-subscription secret. The
// secret is the raw bytes stored in webhook_subscriptions.secret (we
// store hex-encoded random bytes but the HMAC is keyed on the textual
// representation, not the decoded bytes — this matches Stripe's
// convention and lets subscribers paste the secret as-is without
// having to know about hex decoding). Returns an error when the secret
// is shorter than the migration's CHECK floor.
func NewSigner(secret string) (*Signer, error) {
	if len(secret) < 32 {
		return nil, fmt.Errorf("webhooks: signer secret too short (%d bytes, need >= 32)", len(secret))
	}
	return &Signer{secret: []byte(secret)}, nil
}

// Sign computes the signature for body at the given timestamp.
// Returns the canonical header value "t=<unix>,v1=<hex>" that the
// delivery client puts in SignatureHeader. Separating Sign from the
// caller's actual http.Header construction makes it trivial to unit-
// test the signature algebra independently from net/http.
//
// The signed bytes are exactly `<unix_seconds>.<body>` — note the
// literal '.' separator. Stripe's scheme picks the same separator so
// subscribers can reuse a Stripe verification snippet by swapping
// the prefix.
func (s *Signer) Sign(body []byte, ts time.Time) string {
	t := ts.UTC().Unix()
	mac := hmac.New(sha256.New, s.secret)
	// hash.Hash.Write never returns an error (per stdlib contract),
	// so we drop the error return explicitly; same for the
	// Fprintf-of-int-into-hash call below. The explicit discards
	// silence errcheck without weakening the actual writes.
	_, _ = fmt.Fprintf(mac, "%d.", t)
	_, _ = mac.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", t, hex.EncodeToString(mac.Sum(nil)))
}

// ErrSignatureMalformed is returned by Verify when the header value
// doesn't parse — missing fields, wrong format. ErrSignatureExpired
// is returned when the timestamp is outside the tolerance window.
// ErrSignatureMismatch is returned for a syntactically-valid but
// algebraically-wrong signature.
var (
	ErrSignatureMalformed = errors.New("webhooks: signature header malformed")
	ErrSignatureExpired   = errors.New("webhooks: signature timestamp outside tolerance window")
	ErrSignatureMismatch  = errors.New("webhooks: signature does not match payload")
)

// Verify checks header against body using the signer's secret. now is
// taken as a parameter (rather than calling time.Now() inline) so
// tests can pin clock state without monkey-patching. Returns one of
// the Err* sentinels above, or nil on success.
//
// This function is the symmetric counterpart of Sign and exists
// primarily for testing — production subscribers run their own
// verification code on the receiving side. The exposed reference
// implementation also lets us pin the contract: a change to Sign that
// silently breaks Verify against an old signature fails the unit
// tests.
func (s *Signer) Verify(body []byte, header string, now time.Time) error {
	if header == "" {
		return ErrSignatureMalformed
	}
	var (
		ts     int64
		sigHex string
		seenT  bool
		seenV  bool
	)
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			return ErrSignatureMalformed
		}
		switch kv[0] {
		case "t":
			parsed, err := strconv.ParseInt(kv[1], 10, 64)
			if err != nil {
				return ErrSignatureMalformed
			}
			ts = parsed
			seenT = true
		case "v1":
			sigHex = kv[1]
			seenV = true
		}
	}
	if !seenT || !seenV {
		return ErrSignatureMalformed
	}
	if abs(now.UTC().Unix()-ts) > int64(timestampToleranceWindow.Seconds()) {
		return ErrSignatureExpired
	}
	expected, err := hex.DecodeString(sigHex)
	if err != nil {
		return ErrSignatureMalformed
	}
	mac := hmac.New(sha256.New, s.secret)
	_, _ = fmt.Fprintf(mac, "%d.", ts)
	_, _ = mac.Write(body)
	if subtle.ConstantTimeCompare(expected, mac.Sum(nil)) != 1 {
		return ErrSignatureMismatch
	}
	return nil
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
