package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-code-agent/internal/agent"
	"go-code-agent/internal/application"
	"go-code-agent/internal/background"
	"go-code-agent/internal/history"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/model"
	"go-code-agent/internal/tool"
)

func TestAppendHistoryMessagesPersistsAssistantAndTool(t *testing.T) {
	store, err := history.New(filepath.Join(t.TempDir(), history.FileName))
	if err != nil {
		t.Fatal(err)
	}
	messages := []llm.Message{
		llm.UserMessage("already persisted"),
		{
			Role:    llm.RoleAssistant,
			Content: "calling tool",
			ToolCalls: []llm.ToolCall{
				{ID: "call-1", Name: "read_file", Arguments: `{}`},
			},
		},
		llm.ToolMessage("result", "call-1"),
	}

	if err := appendHistoryMessages(store, messages); err != nil {
		t.Fatal(err)
	}
	entries, err := store.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("persisted entries = %d, want assistant and tool only", len(entries))
	}
}

func TestAppendHistoryMessagesReturnsWriteError(t *testing.T) {
	dir := t.TempDir()
	historyDir := filepath.Join(dir, "history")
	store, err := history.New(filepath.Join(historyDir, history.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(historyDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(historyDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	err = appendHistoryMessages(store, []llm.Message{llm.AssistantMessage("answer")})
	if err == nil || !strings.Contains(err.Error(), "append assistant message") {
		t.Fatalf("error = %v, want assistant persistence error", err)
	}
}

type replPersistenceProvider struct{}

func (replPersistenceProvider) Name() string { return "repl-persistence-test" }
func (replPersistenceProvider) Capabilities() model.ProviderCapabilities {
	return model.ProviderCapabilities{Streaming: true}
}
func (replPersistenceProvider) Call(context.Context, llm.CallParams) (*llm.Completion, error) {
	return &llm.Completion{Content: "answer", FinishReason: "stop"}, nil
}
func (replPersistenceProvider) Stream(_ context.Context, _ llm.CallParams, sink model.StreamSink) (*llm.StreamResult, error) {
	sink.OnTextDelta("answer")
	sink.OnDone()
	return &llm.StreamResult{Content: "answer", FinishReason: "stop"}, nil
}

func TestReplContinuesInMemoryAfterUserHistoryFailure(t *testing.T) {
	root := t.TempDir()
	historyDir := filepath.Join(root, "history")
	histStore, err := history.New(filepath.Join(historyDir, history.FileName))
	if err != nil {
		t.Fatal(err)
	}

	catalog := tool.NewToolCatalog()
	runner := agent.NewRunner(
		agent.NewLeadProfile("system"),
		model.NewGateway(replPersistenceProvider{}, model.NewRoleThrottle(1)),
		tool.NewExecutor(catalog, nil, nil),
		nil,
		nil,
	)

	reads := 0
	r := newRepl(&application.BuiltRunner{
		Session: application.SessionFacade{
			ID: "session-1", AgentID: "lead", Workdir: root,
			SysPrompt: "system", HistStore: histStore,
		},
		Team:    application.TeamFacade{BG: background.New(root)},
		Runtime: application.RuntimeFacade{Runner: runner},
	}, context.Background(), func() (string, error) {
		reads++
		if reads > 1 {
			return "", io.EOF
		}
		if err := os.RemoveAll(historyDir); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(historyDir, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		return "continue despite save failure", nil
	})

	stderr := captureReplStderr(t, r.run)
	if reads != 2 {
		t.Fatalf("read calls = %d, want REPL to continue to the next prompt", reads)
	}
	if got := strings.Count(stderr, "[warn] history save failed"); got != 1 {
		t.Fatalf("history warnings = %d, want one; stderr=%q", got, stderr)
	}
	if !strings.Contains(stderr, "continuing in memory") {
		t.Fatalf("warning does not explain in-memory continuation: %q", stderr)
	}
}

func captureReplStderr(t *testing.T, run func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = writer
	run()
	_ = writer.Close()
	os.Stderr = previous
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
