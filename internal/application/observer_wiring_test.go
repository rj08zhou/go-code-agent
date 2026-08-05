package application_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-code-agent/internal/application"
	"go-code-agent/internal/llm"
)

func countUsageLines(t *testing.T, sessionDir string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(sessionDir, "usage.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

func TestBuildSessionContextCarriesCallObservers(t *testing.T) {
	app, _, _ := newTestApp(t)
	defer app.Shutdown(context.Background())

	built, err := app.Build(context.Background(), application.BuildOptions{NewSession: true})
	if err != nil {
		t.Fatal(err)
	}
	if built.Session.Usage == nil {
		t.Fatal("expected session usage tracker")
	}
	sessionDir, err := app.SessionRepo().SessionDir(built.Session.ID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := app.Gateway().Call(built.Session.Context, "lead", llm.CallParams{Model: "gpt-test"}); err != nil {
		t.Fatalf("session Call: %v", err)
	}
	if got := countUsageLines(t, sessionDir); got != 1 {
		t.Fatalf("usage lines after session Call = %d, want 1", got)
	}

	// Backfill uses a bare context; it must not write into the active session tracker.
	if _, err := app.Gateway().Call(context.Background(), "lead", llm.CallParams{Model: "gpt-test"}); err != nil {
		t.Fatalf("background Call: %v", err)
	}
	if got := countUsageLines(t, sessionDir); got != 1 {
		t.Fatalf("background Call polluted session usage: lines=%d, want 1", got)
	}
}
