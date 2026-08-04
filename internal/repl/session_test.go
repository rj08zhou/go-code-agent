package repl

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chzyer/readline"

	"go-code-agent/internal/application"
	"go-code-agent/internal/history"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/session"
)

func TestFormatSessionListPreservesActiveMarkerAndFields(t *testing.T) {
	got := formatSessionList("active", []session.State{
		{ID: "active", Status: session.StatusActive, Title: "Current"},
		{ID: "old", Status: session.StatusArchived, Title: "Previous"},
	})
	for _, want := range []string{"* active", "active", "Current", "old", "archived", "Previous"} {
		if !strings.Contains(got, want) {
			t.Errorf("formatted session list %q does not contain %q", got, want)
		}
	}
	if got := formatSessionList("", nil); got != "No sessions." {
		t.Fatalf("empty session list = %q", got)
	}
}

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

	r := &Loop{built: &application.BuiltRunner{
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

func TestFormatTurnOutcomeError(t *testing.T) {
	if got := formatTurnOutcomeError(context.Canceled, nil); !strings.Contains(got, "[interrupted]") {
		t.Fatalf("cancelled turn feedback = %q", got)
	}
	if got := formatTurnOutcomeError(context.Canceled, context.Canceled); got != "" {
		t.Fatalf("session shutdown feedback = %q, want empty", got)
	}
	boom := errors.New("provider failed")
	if got := formatTurnOutcomeError(boom, nil); !strings.Contains(got, "[error]") || !strings.Contains(got, boom.Error()) {
		t.Fatalf("ordinary error feedback = %q", got)
	}
}

func TestReplPromptCtrlCContinuesAndCtrlDExits(t *testing.T) {
	histStore, err := history.New(filepath.Join(t.TempDir(), history.FileName))
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	r := New(&application.BuiltRunner{
		Session: application.SessionFacade{HistStore: histStore},
	}, context.Background(), func() (string, error) {
		reads++
		if reads == 1 {
			return "discard me", readline.ErrInterrupt
		}
		return "", io.EOF
	})

	r.Run()
	if reads != 2 {
		t.Fatalf("read calls = %d, want Ctrl-C to continue to the next prompt", reads)
	}
}
