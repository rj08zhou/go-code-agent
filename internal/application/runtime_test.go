package application

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-code-agent/internal/session"
)

func TestCloseSessionReleasesRuntimeAndPreservesCloseError(t *testing.T) {
	runtime := NewSessionRuntime(context.Background(), nil, "", nil, nil, &session.State{ID: "close-error"})
	calls := 0
	runtime.AddHook("failing", func(context.Context) error {
		calls++
		return errors.New("close failed")
	})
	app := &Application{runtime: runtime}

	first := app.CloseSession(context.Background())
	if first == nil || !strings.Contains(first.Error(), "close failed") {
		t.Fatalf("first CloseSession error = %v, want close failure", first)
	}
	if app.runtime != nil {
		t.Fatal("CloseSession retained the closed runtime")
	}
	second := app.Shutdown(context.Background())
	if second == nil || second.Error() != first.Error() {
		t.Fatalf("second Shutdown error = %v, want %v", second, first)
	}
	if calls != 1 {
		t.Fatalf("shutdown hook calls = %d, want 1", calls)
	}
}
