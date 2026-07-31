package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go-code-agent/internal/application"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/session"
)

func TestSessionSwitchCommandDefersActivationUntilBuild(t *testing.T) {
	dataDir := t.TempDir()
	repo := session.NewRepository(dataDir)
	first := &session.State{ID: "first", Title: "First", Status: session.StatusActive}
	second := &session.State{ID: "second", Title: "Second", Status: session.StatusActive}
	for _, state := range []*session.State{first, second} {
		if err := repo.CreateSession(state); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(
		filepath.Join(dataDir, "sessions.json"),
		[]byte(`{"active_id":"","sessions":[]}`),

		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := repo.SwitchActive(first.ID); err != nil {
		t.Fatal(err)
	}

	r := &repl{built: &application.BuiltRunner{
		Session: application.SessionFacade{ID: first.ID, Repo: repo},
	}}
	messages := []llm.Message{}
	if stop := r.handleCommand(context.Background(), "/session switch "+second.ID, &messages); !stop {
		t.Fatal("session switch did not request REPL restart")
	}
	if r.next == nil || r.next.SessionID != second.ID {
		t.Fatalf("next Build options = %#v, want session %q", r.next, second.ID)
	}
	index, err := repo.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if index.ActiveID != first.ID {
		t.Fatalf("active session changed before Build: got %q, want %q", index.ActiveID, first.ID)
	}
}
