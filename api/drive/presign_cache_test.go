package drive

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestSetPresignedURLCacheControl(t *testing.T) {
	t.Run("derives private max-age below the URL lifetime", func(t *testing.T) {
		rec := httptest.NewRecorder()
		setPresignedURLCacheControl(rec, 15*time.Minute)
		// 15m - 60s safety = 840s.
		if got, want := rec.Header().Get("Cache-Control"), "private, max-age=840"; got != want {
			t.Fatalf("Cache-Control = %q, want %q", got, want)
		}
	})

	t.Run("never advertises a shared/CDN-cacheable response", func(t *testing.T) {
		rec := httptest.NewRecorder()
		setPresignedURLCacheControl(rec, 15*time.Minute)
		// A presigned URL is a per-user bearer capability: the
		// directive must be private so a shared cache cannot serve one
		// user's URL to another.
		if got := rec.Header().Get("Cache-Control"); got == "" || got[:7] != "private" {
			t.Fatalf("Cache-Control = %q, must start with private", got)
		}
	})

	t.Run("no-store when the URL is too short-lived to cache safely", func(t *testing.T) {
		for _, ttl := range []time.Duration{0, 30 * time.Second, 60 * time.Second} {
			rec := httptest.NewRecorder()
			setPresignedURLCacheControl(rec, ttl)
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("ttl=%s: Cache-Control = %q, want no-store", ttl, got)
			}
		}
	})
}
