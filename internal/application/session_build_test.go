package application_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-code-agent/internal/application"
	"go-code-agent/internal/session"
)

func TestBuildRejectsMissingExplicitSessionWithoutCreatingOne(t *testing.T) {
	app, _, _ := newTestApp(t)
	defer app.Shutdown(context.Background())

	built, rt, err := app.Build(application.BuildOptions{SessionID: "missing"})
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("Build error = %v, want ErrSessionNotFound", err)
	}
	if built != nil || rt != nil {
		t.Fatalf("Build returned runtime for missing session: built=%v rt=%v", built, rt)
	}
	if app.Runtime() != nil {
		t.Fatal("missing explicit session unexpectedly installed an active runtime")
	}
	if got := app.SessionRepo().ListSessions(); got != "No sessions." {
		t.Fatalf("sessions after failed resume = %q, want no sessions", got)
	}
}

func TestBuildRejectsInvalidExplicitSessionID(t *testing.T) {
	app, _, _ := newTestApp(t)
	defer app.Shutdown(context.Background())

	built, rt, err := app.Build(application.BuildOptions{SessionID: "../outside"})
	if !errors.Is(err, session.ErrInvalidSessionID) {
		t.Fatalf("Build error = %v, want ErrInvalidSessionID", err)
	}
	if built != nil || rt != nil {
		t.Fatalf("Build returned runtime for invalid session ID: built=%v rt=%v", built, rt)
	}
}

func TestBuildRejectsMutuallyExclusiveSessionOptions(t *testing.T) {
	app, _, _ := newTestApp(t)
	defer app.Shutdown(context.Background())

	built, rt, err := app.Build(application.BuildOptions{SessionID: "session-1", NewSession: true})
	if err == nil || !strings.Contains(err.Error(), "cannot be used together") {
		t.Fatalf("Build error = %v, want mutually exclusive option error", err)
	}
	if built != nil || rt != nil {
		t.Fatalf("Build returned runtime for invalid options: built=%v rt=%v", built, rt)
	}
}

func TestBuildExplicitSessionBecomesActive(t *testing.T) {
	app, _, _ := newTestApp(t)
	defer app.Shutdown(context.Background())

	first, firstRT, err := app.Build(application.BuildOptions{NewSession: true})
	if err != nil {
		t.Fatal(err)
	}
	firstID := first.Session.ID
	if err := firstRT.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	second, secondRT, err := app.Build(application.BuildOptions{NewSession: true})
	if err != nil {
		t.Fatal(err)
	}
	if second.Session.ID == firstID {
		t.Fatal("two fresh builds returned the same session ID")
	}
	if err := secondRT.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	resumed, resumedRT, err := app.Build(application.BuildOptions{SessionID: firstID})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Session.ID != firstID {
		t.Fatalf("resumed ID = %q, want %q", resumed.Session.ID, firstID)
	}
	if err := resumedRT.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	implicit, implicitRT, err := app.Build(application.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer implicitRT.Close(context.Background())
	if implicit.Session.ID != firstID {
		t.Fatalf("implicit resume ID = %q, want explicitly activated %q", implicit.Session.ID, firstID)
	}
}

func TestBuildRepairsUnavailableImplicitActiveSession(t *testing.T) {
	app, _, _ := newTestApp(t)
	defer app.Shutdown(context.Background())

	original, originalRT, err := app.Build(application.BuildOptions{NewSession: true})
	if err != nil {
		t.Fatal(err)
	}
	originalID := original.Session.ID
	if err := originalRT.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	originalDir, err := app.SessionRepo().SessionDir(originalID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(originalDir, "meta.json")); err != nil {
		t.Fatal(err)
	}

	replacement, replacementRT, err := app.Build(application.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Session.ID == originalID {
		t.Fatal("unavailable implicit active session was not replaced")
	}
	if err := replacementRT.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	resumed, resumedRT, err := app.Build(application.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer resumedRT.Close(context.Background())
	if resumed.Session.ID != replacement.Session.ID {
		t.Fatalf("repaired active ID = %q, want %q", resumed.Session.ID, replacement.Session.ID)
	}
}
