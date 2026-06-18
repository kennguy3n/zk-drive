package main

import (
	"context"
	"testing"

	"github.com/kennguy3n/zk-drive/api/middleware"
)

// stubReverifier and stubRefresher are minimal implementations of the
// collab auth interfaces; validateCollabAuthWiring only inspects
// nil-ness, so the method bodies are never exercised.
type stubReverifier struct{}

func (stubReverifier) Reverify(context.Context, string) error { return nil }

type stubRefresher struct{}

func (stubRefresher) RefreshCollabAuth(context.Context, string) (middleware.CollabAuthRefresh, error) {
	return middleware.CollabAuthRefresh{}, nil
}

func TestValidateCollabAuthWiring(t *testing.T) {
	var nilReverifier middleware.TokenReverifier
	var nilRefresher middleware.CollabTokenRefresher

	t.Run("built-in auth: refresher only is allowed", func(t *testing.T) {
		if err := validateCollabAuthWiring(nilReverifier, stubRefresher{}); err != nil {
			t.Fatalf("refresher-only wiring rejected: %v", err)
		}
	})

	t.Run("iam-core: reverifier only is allowed", func(t *testing.T) {
		if err := validateCollabAuthWiring(stubReverifier{}, nilRefresher); err != nil {
			t.Fatalf("reverifier-only wiring rejected: %v", err)
		}
	})

	t.Run("neither wired is allowed", func(t *testing.T) {
		if err := validateCollabAuthWiring(nilReverifier, nilRefresher); err != nil {
			t.Fatalf("empty wiring rejected: %v", err)
		}
	})

	t.Run("both wired is a fail-fast error", func(t *testing.T) {
		if err := validateCollabAuthWiring(stubReverifier{}, stubRefresher{}); err == nil {
			t.Fatal("both reverifier and refresher set: want error, got nil")
		}
	})
}
