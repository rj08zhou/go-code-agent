package application_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go-code-agent/internal/application"
	"go-code-agent/internal/session"
)

func TestBuildRejectsInvalidSessionRequests(t *testing.T) {
	tests := []struct {
		name string
		opts application.BuildOptions
		want error
	}{
		{"missing", application.BuildOptions{SessionID: "missing"}, session.ErrSessionNotFound},
		{"invalid identifier", application.BuildOptions{SessionID: "../outside"}, session.ErrInvalidSessionID},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			app, _, _ := newTestApp(t)
			defer app.Shutdown(context.Background())

			built, err := app.Build(context.Background(), tc.opts)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Build error = %v, want %v", err, tc.want)
			}
			if built != nil {
				t.Fatalf("Build returned runner for failed request: %#v", built)
			}
			if got := app.Catalog(); got != nil {
				t.Fatalf("failed Build installed a catalog: %#v", got)
			}
		})
	}
}

func TestBuildRejectsMutuallyExclusiveSessionOptions(t *testing.T) {
	app, _, _ := newTestApp(t)
	defer app.Shutdown(context.Background())

	built, err := app.Build(context.Background(), application.BuildOptions{
		SessionID:  "session-1",
		NewSession: true,
	})
	if err == nil || !strings.Contains(err.Error(), "cannot be used together") {
		t.Fatalf("Build error = %v, want mutually exclusive option error", err)
	}
	if built != nil {
		t.Fatalf("Build returned runner for invalid options: %#v", built)
	}
}

func TestBuildRejectsCanceledParentContext(t *testing.T) {
	app, _, _ := newTestApp(t)
	defer app.Shutdown(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	built, err := app.Build(ctx, application.BuildOptions{NewSession: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Build error = %v, want context canceled", err)
	}
	if built != nil {
		t.Fatalf("Build returned runner for canceled context: %#v", built)
	}
}

func TestBuildRequiresClosingActiveSession(t *testing.T) {
	app, _, _ := newTestApp(t)
	defer app.Shutdown(context.Background())

	first, err := app.Build(context.Background(), application.BuildOptions{NewSession: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.Session.Context == nil {
		t.Fatal("Build did not expose the session context")
	}
	if _, err := app.Build(context.Background(), application.BuildOptions{NewSession: true}); err == nil ||
		!strings.Contains(err.Error(), "must be closed") {
		t.Fatalf("Build with active session error = %v, want active-runtime error", err)
	}
	if err := app.CloseSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := app.Build(context.Background(), application.BuildOptions{NewSession: true})
	if err != nil {
		t.Fatal(err)
	}
	if second.Session.ID == first.Session.ID {
		t.Fatal("two fresh sessions have the same ID")
	}
	if err := app.CloseSession(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBuildActivatesExplicitAndImplicitSessions(t *testing.T) {
	app, _, _ := newTestApp(t)
	defer app.Shutdown(context.Background())

	first, err := app.Build(context.Background(), application.BuildOptions{NewSession: true})
	if err != nil {
		t.Fatal(err)
	}
	firstID := first.Session.ID
	if err := app.CloseSession(context.Background()); err != nil {
		t.Fatal(err)
	}

	second, err := app.Build(context.Background(), application.BuildOptions{NewSession: true})
	if err != nil {
		t.Fatal(err)
	}
	if second.Session.ID == firstID {
		t.Fatal("two fresh builds returned the same session ID")
	}
	if err := app.CloseSession(context.Background()); err != nil {
		t.Fatal(err)
	}

	resumed, err := app.Build(context.Background(), application.BuildOptions{SessionID: firstID})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Session.ID != firstID {
		t.Fatalf("resumed ID = %q, want %q", resumed.Session.ID, firstID)
	}
	if err := app.CloseSession(context.Background()); err != nil {
		t.Fatal(err)
	}

	implicit, err := app.Build(context.Background(), application.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if implicit.Session.ID != firstID {
		t.Fatalf("implicit resume ID = %q, want %q", implicit.Session.ID, firstID)
	}
}

func TestBuildRepairsUnavailableImplicitActiveSession(t *testing.T) {
	app, _, _ := newTestApp(t)
	defer app.Shutdown(context.Background())

	original, err := app.Build(context.Background(), application.BuildOptions{NewSession: true})
	if err != nil {
		t.Fatal(err)
	}
	originalID := original.Session.ID
	if err := app.CloseSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	originalDir, err := app.SessionRepo().SessionDir(originalID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(originalDir, "meta.json")); err != nil {
		t.Fatal(err)
	}

	replacement, err := app.Build(context.Background(), application.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Session.ID == originalID {
		t.Fatal("unavailable implicit active session was not replaced")
	}
}

func TestSessionRuntimeCloseRunsHooksInReverseOrder(t *testing.T) {
	runtime := application.NewSessionRuntime(context.Background(), nil, "", nil, nil, &session.State{ID: "ordered-close"})
	var order []string
	runtime.AddHook("first", func(context.Context) error {
		order = append(order, "first")
		return nil
	})
	runtime.AddHook("second", func(context.Context) error {
		order = append(order, "second")
		return nil
	})

	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got, want := strings.Join(order, ","), "second,first"; got != want {
		t.Fatalf("hook order = %q, want %q", got, want)
	}
}

func TestSessionRuntimeCloseStopsAfterContextDeadline(t *testing.T) {
	runtime := application.NewSessionRuntime(context.Background(), nil, "", nil, nil, &session.State{ID: "timeout-close"})
	release := make(chan struct{})
	ranAfterTimeout := false
	runtime.AddHook("after-blocked", func(context.Context) error {
		ranAfterTimeout = true
		return nil
	})
	runtime.AddHook("blocked", func(context.Context) error {
		<-release
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := runtime.Close(ctx); err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("Close error = %v, want context deadline exceeded", err)
	}
	if ranAfterTimeout {
		t.Fatal("hook after timed-out hook ran")
	}
	close(release)
}

func TestSessionRuntimeInheritsParentContext(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	runtime := application.NewSessionRuntime(parent, nil, "", nil, nil, &session.State{ID: "parent-context"})

	cancel()
	select {
	case <-runtime.Ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("session context was not canceled with its parent")
	}
}
