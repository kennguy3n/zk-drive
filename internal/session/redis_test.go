package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// newTestStore spins up an in-process miniredis instance and returns
// a RedisSessionStore wired to it. Using miniredis keeps the test
// hermetic — CI doesn't need a real Redis.
func newTestStore(t *testing.T) (*RedisSessionStore, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewRedisSessionStore(client), mr
}

func TestSessionRevocation(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	userID := uuid.New()
	wsID := uuid.New()
	sessionID := uuid.NewString()

	if err := store.Set(ctx, sessionID, userID, wsID, time.Hour); err != nil {
		t.Fatalf("set: %v", err)
	}
	gotUser, gotWS, err := store.Get(ctx, wsID, sessionID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if gotUser != userID || gotWS != wsID {
		t.Fatalf("unexpected ids: user=%s ws=%s", gotUser, gotWS)
	}

	if err := store.Revoke(ctx, wsID, sessionID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, _, err := store.Get(ctx, wsID, sessionID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound after revoke, got %v", err)
	}
}

func TestRevokeAllForUser(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	userID := uuid.New()
	wsID := uuid.New()
	sessions := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	for _, sid := range sessions {
		if err := store.Set(ctx, sid, userID, wsID, time.Hour); err != nil {
			t.Fatalf("set %s: %v", sid, err)
		}
	}

	otherUser := uuid.New()
	survivor := uuid.NewString()
	if err := store.Set(ctx, survivor, otherUser, wsID, time.Hour); err != nil {
		t.Fatalf("set survivor: %v", err)
	}

	if err := store.RevokeAllForUser(ctx, wsID, userID); err != nil {
		t.Fatalf("revoke-all: %v", err)
	}
	for _, sid := range sessions {
		if _, _, err := store.Get(ctx, wsID, sid); !errors.Is(err, ErrSessionNotFound) {
			t.Fatalf("session %s should be revoked: %v", sid, err)
		}
	}
	if _, _, err := store.Get(ctx, wsID, survivor); err != nil {
		t.Fatalf("survivor session should still exist: %v", err)
	}
}

func TestSessionTTL(t *testing.T) {
	store, mr := newTestStore(t)
	ctx := context.Background()

	userID := uuid.New()
	wsID := uuid.New()
	sessionID := uuid.NewString()

	if err := store.Set(ctx, sessionID, userID, wsID, 30*time.Second); err != nil {
		t.Fatalf("set: %v", err)
	}
	// FastForward past the TTL — miniredis exposes a clock hook so we
	// don't have to actually sleep.
	mr.FastForward(time.Minute)
	if _, _, err := store.Get(ctx, wsID, sessionID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected expired session to be missing: %v", err)
	}
}
