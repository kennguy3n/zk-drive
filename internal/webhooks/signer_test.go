package webhooks

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// secret32 is exactly the minimum legal secret length (32 bytes /
// 256 bits). Used across the table tests so we exercise the floor
// case rather than something accidentally generous.
const secret32 = "abcdef0123456789abcdef0123456789"

func TestNewSigner_RejectsShortSecret(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		secret string
		want   bool // want error
	}{
		{"empty", "", true},
		{"one_byte", "a", true},
		{"31_bytes", strings.Repeat("a", 31), true},
		{"32_bytes", strings.Repeat("a", 32), false},
		{"64_bytes", strings.Repeat("a", 64), false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewSigner(c.secret)
			if (err != nil) != c.want {
				t.Fatalf("NewSigner(%q): err=%v want_err=%v", c.secret, err, c.want)
			}
		})
	}
}

func TestSigner_SignVerify_Roundtrip(t *testing.T) {
	t.Parallel()
	s, err := NewSigner(secret32)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	body := []byte(`{"event_id":"deadbeef","data":{"k":"v"}}`)
	ts := time.Unix(1_700_000_000, 0).UTC()
	sig := s.Sign(body, ts)
	if !strings.HasPrefix(sig, "t=1700000000,v1=") {
		t.Fatalf("sign produced unexpected prefix: %q", sig)
	}
	if err := s.Verify(body, sig, ts); err != nil {
		t.Fatalf("verify roundtrip: %v", err)
	}
}

func TestSigner_Verify_DetectsTampering(t *testing.T) {
	t.Parallel()
	s, err := NewSigner(secret32)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	body := []byte(`{"event_id":"abc","data":{"x":1}}`)
	ts := time.Unix(1_700_000_000, 0)
	sig := s.Sign(body, ts)
	// Tamper with a single byte in the body — must fail.
	tampered := append([]byte(nil), body...)
	tampered[len(tampered)-2] = 'Z'
	if err := s.Verify(tampered, sig, ts); !errors.Is(err, ErrSignatureMismatch) {
		t.Fatalf("verify tampered body: err=%v want ErrSignatureMismatch", err)
	}
}

func TestSigner_Verify_DifferentSecrets(t *testing.T) {
	t.Parallel()
	a, _ := NewSigner(secret32)
	b, _ := NewSigner(strings.Repeat("b", 32))
	body := []byte(`x`)
	ts := time.Unix(1_700_000_000, 0)
	if err := b.Verify(body, a.Sign(body, ts), ts); !errors.Is(err, ErrSignatureMismatch) {
		t.Fatalf("verify with wrong secret: err=%v want ErrSignatureMismatch", err)
	}
}

func TestSigner_Verify_ExpiredTimestamp(t *testing.T) {
	t.Parallel()
	s, _ := NewSigner(secret32)
	body := []byte(`x`)
	signedAt := time.Unix(1_700_000_000, 0)
	sig := s.Sign(body, signedAt)
	// Verifier's clock is 6 minutes ahead — outside the 5-minute
	// tolerance window.
	tooLate := signedAt.Add(6 * time.Minute)
	if err := s.Verify(body, sig, tooLate); !errors.Is(err, ErrSignatureExpired) {
		t.Fatalf("verify expired: err=%v want ErrSignatureExpired", err)
	}
	// And 6 minutes behind the signature — also outside the window.
	tooEarly := signedAt.Add(-6 * time.Minute)
	if err := s.Verify(body, sig, tooEarly); !errors.Is(err, ErrSignatureExpired) {
		t.Fatalf("verify pre-clock: err=%v want ErrSignatureExpired", err)
	}
}

func TestSigner_Verify_MalformedHeader(t *testing.T) {
	t.Parallel()
	s, _ := NewSigner(secret32)
	cases := []string{
		"",                       // empty
		"v1=abcdef",              // no t=
		"t=1700000000",           // no v1=
		"random-garbage",         // no kv pairs
		"t=,v1=abcdef",           // empty t
		"t=not-a-number,v1=abcd", // non-numeric t
	}
	for _, c := range cases {
		c := c
		t.Run(c, func(t *testing.T) {
			t.Parallel()
			if err := s.Verify([]byte("body"), c, time.Unix(1_700_000_000, 0)); !errors.Is(err, ErrSignatureMalformed) {
				t.Fatalf("malformed %q: err=%v want ErrSignatureMalformed", c, err)
			}
		})
	}
}

func TestSigner_Sign_DeterministicOutput(t *testing.T) {
	t.Parallel()
	// Pin the bytes so a future refactor of the signing function
	// that silently changes the algebra is caught by this test.
	s, _ := NewSigner(secret32)
	body := []byte("payload-bytes")
	ts := time.Unix(1_700_000_000, 0)
	got := s.Sign(body, ts)
	// Pinned hash computed once via NewSigner(secret32).Sign at the
	// fixed timestamp + body above. A drift here means the signing
	// algebra (input ordering, separator, encoding) has changed and
	// existing subscribers' verifiers would silently break.
	want := "t=1700000000,v1=072d7cfced7d12ecf302696a455163c7474cdc5e59a2af25e2295e9aa5fc7777"
	if got != want {
		t.Fatalf("sign drift: got=%q want=%q", got, want)
	}
}
