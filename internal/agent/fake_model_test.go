package agent

import (
	"context"
	"encoding/json"
	"go-code-agent/internal/config"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/model"
	"go-code-agent/internal/tool"
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	config.SetConfig(config.Load())
	os.Exit(m.Run())
}

// fakeProvider implements model.Provider for integration testing.
type fakeProvider struct {
	name string

	toolCalls    []llm.ToolCall
	content      string
	finishReason string
	callErr      error

	// oneShot: if true, toolCalls are cleared after the first invocation
	// so the model stops returning tool calls, simulating a real conversation.
	oneShot bool
	// multiShot: if >0, toolCalls are returned for the first N calls only.
	multiShot  int
	callCount  int
	lastParams *llm.CallParams
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Call(ctx context.Context, params llm.CallParams) (*llm.Completion, error) {
	f.lastParams = &params
	f.callCount++
	if f.callErr != nil {
		return nil, f.callErr
	}
	tc := f.toolCalls
	if f.oneShot && f.callCount > 1 {
		tc = nil
	}
	if f.multiShot > 0 && f.callCount > f.multiShot {
		tc = nil
	}
	return &llm.Completion{
		Content:      f.content,
		ToolCalls:    tc,
		FinishReason: f.finishReason,
	}, nil
}

func (f *fakeProvider) Stream(ctx context.Context, params llm.CallParams, sink model.StreamSink) (*llm.StreamResult, error) {
	f.lastParams = &params
	f.callCount++
	if f.callErr != nil {
		return nil, f.callErr
	}
	tc := f.toolCalls
	if f.oneShot && f.callCount > 1 {
		tc = nil
	}
	if f.multiShot > 0 && f.callCount > f.multiShot {
		tc = nil
	}
	sink.OnTextDelta(f.content)
	sink.OnDone()
	return &llm.StreamResult{
		Content:      f.content,
		ToolCalls:    tc,
		FinishReason: f.finishReason,
	}, nil
}

// withOneShot configures the fake to only return tool calls on the first call.
func (f *fakeProvider) withOneShot() *fakeProvider {
	f.oneShot = true
	return f
}

// TestRunner_Integration_ModelReceivesTools verifies the Runner passes tool defs to the model.
func TestRunner_Integration_ModelReceivesTools(t *testing.T) {
	fakeModel := &fakeProvider{name: "fake"}
	gw := model.NewGateway(fakeModel, model.NewRoleThrottle(10))
	catalog := tool.NewToolCatalog()
	catalog.RegisterAll([]tool.ToolDefinition{
		{Name: "bash", Description: "Run shell", Effects: tool.Effects(tool.EffectExecuteProcess), Handler: func(scope *tool.ToolScope, args json.RawMessage) tool.Result { return tool.Succeeded("ok") }},
		{Name: "read_file", Description: "Read file", Effects: tool.Effects(tool.EffectReadFile), Handler: func(scope *tool.ToolScope, args json.RawMessage) tool.Result { return tool.Succeeded("ok") }},
	})

	exec := tool.NewExecutor(catalog, nil, nil)
	scope := &tool.ToolScope{Role: "lead", CanRead: true, CanWrite: true, CanExecute: true, CanNetwork: true}
	profile := NewLeadProfile("You are a test agent.")
	runner := NewRunner(profile, gw, exec, scope)
	fakeModel.content = "done"

	outcome := runner.Run(context.Background(), []llm.Message{llm.UserMessage("hello")}, "trace-1")

	if outcome.Error != nil {
		t.Fatalf("unexpected error: %v", outcome.Error)
	}
	if fakeModel.lastParams == nil {
		t.Fatal("model was never called")
	}
	if len(fakeModel.lastParams.Tools) != 2 {
		t.Fatalf("expected 2 tools passed to model, got %d", len(fakeModel.lastParams.Tools))
	}
	names := map[string]bool{}
	for _, t := range fakeModel.lastParams.Tools {
		names[t.Name] = true
	}
	if !names["bash"] || !names["read_file"] {
		t.Fatalf("missing expected tool names, got: %v", names)
	}
}

// TestRunner_Integration_ExecutesToolAndCollectsResult verifies full tool execution pipeline.
func TestRunner_Integration_ExecutesToolAndCollectsResult(t *testing.T) {
	tmpDir := t.TempDir()

	fakeModel := &fakeProvider{name: "fake"}
	gw := model.NewGateway(fakeModel, model.NewRoleThrottle(10))
	catalog := tool.NewToolCatalog()
	catalog.RegisterAll([]tool.ToolDefinition{
		{
			Name:        "write_file",
			Description: "Write a file",
			RiskLevel:   tool.RiskSafe,
			Effects:     tool.Effects(tool.EffectWriteFile),
			Handler: func(scope *tool.ToolScope, args json.RawMessage) tool.Result {
				var a struct {
					Path    string `json:"path"`
					Content string `json:"content"`
				}
				json.Unmarshal(args, &a)
				fp := filepath.Join(scope.Workdir, a.Path)
				os.MkdirAll(filepath.Dir(fp), 0755)
				os.WriteFile(fp, []byte(a.Content), 0644)
				return tool.Succeeded("Wrote " + a.Path)
			},
		},
	})

	exec := tool.NewExecutor(catalog, nil, nil)
	scope := &tool.ToolScope{
		Role:       "lead",
		Workdir:    tmpDir,
		CanRead:    true,
		CanWrite:   true,
		CanExecute: true,
	}
	profile := NewLeadProfile("You are a test agent.")
	runner := NewRunner(profile, gw, exec, scope)

	fakeModel.toolCalls = []llm.ToolCall{{
		ID:        "call_1",
		Name:      "write_file",
		Arguments: `{"path":"test.txt","content":"hello world"}`,
	}}
	fakeModel.content = "I wrote the file"
	fakeModel.withOneShot()

	outcome := runner.Run(context.Background(), []llm.Message{llm.UserMessage("write test.txt")}, "trace-2")

	if outcome.Error != nil {
		t.Fatalf("unexpected error: %v", outcome.Error)
	}
	if !outcome.Completed {
		t.Fatal("expected completed outcome")
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, "test.txt"))
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	if string(data) != "hello world" {
		t.Fatalf("wrong content: %q", string(data))
	}

	found := false
	for _, tr := range outcome.ToolResults {
		if tr.Name == "write_file" && tr.Status == tool.StatusSucceeded {
			found = true
		}
	}
	if !found {
		t.Error("write_file result not found in outcome.ToolResults")
	}
}

// TestRunner_Integration_FailingToolProducesStructuredResult verifies structured failure status.
func TestRunner_Integration_FailingToolProducesStructuredResult(t *testing.T) {
	fakeModel := &fakeProvider{name: "fake"}
	gw := model.NewGateway(fakeModel, model.NewRoleThrottle(10))
	catalog := tool.NewToolCatalog()
	catalog.RegisterAll([]tool.ToolDefinition{
		{
			Name:        "risky_op",
			Description: "Always fails",
			RiskLevel:   tool.RiskDanger,
			Effects:     tool.Effects(tool.EffectExecuteProcess),
			Handler: func(scope *tool.ToolScope, args json.RawMessage) tool.Result {
				return tool.Failed("intentional error")
			},
		},
	})

	exec := tool.NewExecutor(catalog, nil, nil)
	scope := &tool.ToolScope{Role: "lead", CanExecute: true}
	profile := NewLeadProfile("You are a test agent.")
	runner := NewRunner(profile, gw, exec, scope)

	fakeModel.toolCalls = []llm.ToolCall{{ID: "call_1", Name: "risky_op", Arguments: `{}`}}
	fakeModel.content = "done"
	fakeModel.withOneShot()

	outcome := runner.Run(context.Background(), []llm.Message{llm.UserMessage("do risky")}, "trace-3")

	if outcome.ToolFailures != 1 {
		t.Fatalf("expected 1 tool failure, got %d", outcome.ToolFailures)
	}
	for _, tr := range outcome.ToolResults {
		if tr.Name == "risky_op" {
			if tr.Status != tool.StatusFailed {
				t.Fatalf("expected StatusFailed, got %s", tr.Status)
			}
			return
		}
	}
	t.Fatal("risky_op result not found in outcome")
}

// TestRunner_Integration_CapabilityDenied verifies capability gating.
func TestRunner_Integration_CapabilityDenied(t *testing.T) {
	fakeModel := &fakeProvider{name: "fake"}
	gw := model.NewGateway(fakeModel, model.NewRoleThrottle(10))
	catalog := tool.NewToolCatalog()
	catalog.RegisterAll([]tool.ToolDefinition{
		{
			Name:        "write_file",
			Description: "Write a file",
			Effects:     tool.Effects(tool.EffectWriteFile),
			Handler: func(scope *tool.ToolScope, args json.RawMessage) tool.Result {
				return tool.Succeeded("should not execute")
			},
		},
	})

	exec := tool.NewExecutor(catalog, nil, nil)
	scope := &tool.ToolScope{Role: "explore", CanRead: true, CanWrite: false}
	profile := NewExploreProfile()
	runner := NewRunner(profile, gw, exec, scope)

	fakeModel.toolCalls = []llm.ToolCall{{ID: "call_1", Name: "write_file", Arguments: `{"path":"secret.txt","content":"data"}`}}
	fakeModel.content = "done"
	fakeModel.withOneShot()

	outcome := runner.Run(context.Background(), []llm.Message{llm.UserMessage("write secret")}, "trace-4")

	for _, tr := range outcome.ToolResults {
		if tr.Name == "write_file" {
			if tr.Status != tool.StatusDenied {
				t.Fatalf("expected StatusDenied, got %s", tr.Status)
			}
			return
		}
	}
	t.Fatal("write_file result not found in outcome")
}

// TestRunner_Integration_ApprovalCheck verifies the approval checker.
func TestRunner_Integration_ApprovalCheck(t *testing.T) {
	fakeModel := &fakeProvider{name: "fake"}
	gw := model.NewGateway(fakeModel, model.NewRoleThrottle(10))
	catalog := tool.NewToolCatalog()
	catalog.RegisterAll([]tool.ToolDefinition{
		{
			Name:        "bash",
			Description: "Run command",
			Effects:     tool.Effects(tool.EffectExecuteProcess),
			Handler: func(scope *tool.ToolScope, args json.RawMessage) tool.Result {
				return tool.Succeeded("ok")
			},
		},
	})

	denyAll := &denyAllApproval{}
	exec := tool.NewExecutor(catalog, denyAll, nil)
	scope := &tool.ToolScope{Role: "lead", CanExecute: true}
	profile := NewLeadProfile("You are a test agent.")
	runner := NewRunner(profile, gw, exec, scope)

	fakeModel.toolCalls = []llm.ToolCall{{ID: "call_1", Name: "bash", Arguments: `{"command":"rm -rf /"}`}}
	fakeModel.content = "done"
	fakeModel.withOneShot()

	outcome := runner.Run(context.Background(), []llm.Message{llm.UserMessage("destroy")}, "trace-5")

	for _, tr := range outcome.ToolResults {
		if tr.Name == "bash" {
			if tr.Status != tool.StatusRejected {
				t.Fatalf("expected StatusRejected for approval denial, got %s", tr.Status)
			}
			return
		}
	}
	t.Fatal("bash result not found in outcome")
}

// TestRunner_Integration_TimeoutTools verifies timeout produces structured status.
func TestRunner_Integration_TimeoutTools(t *testing.T) {
	fakeModel := &fakeProvider{name: "fake"}
	gw := model.NewGateway(fakeModel, model.NewRoleThrottle(10))
	catalog := tool.NewToolCatalog()
	catalog.RegisterAll([]tool.ToolDefinition{
		{
			Name:        "sleep_forever",
			Description: "Blocks forever",
			Effects:     tool.Effects(tool.EffectExecuteProcess),
			Timeout:     1, // 1 nanosecond triggers immediate timeout
			Handler: func(scope *tool.ToolScope, args json.RawMessage) tool.Result {
				select {} // block forever
			},
		},
	})

	exec := tool.NewExecutor(catalog, nil, nil)
	scope := &tool.ToolScope{Role: "lead", CanExecute: true}
	profile := NewLeadProfile("You are a test agent.")
	runner := NewRunner(profile, gw, exec, scope)

	fakeModel.toolCalls = []llm.ToolCall{{ID: "call_1", Name: "sleep_forever", Arguments: `{}`}}
	fakeModel.content = "done"
	fakeModel.withOneShot()

	outcome := runner.Run(context.Background(), []llm.Message{llm.UserMessage("sleep")}, "trace-6")

	if outcome.Error != nil {
		t.Fatalf("unexpected error: %v", outcome.Error)
	}
	for _, tr := range outcome.ToolResults {
		if tr.Name == "sleep_forever" {
			if tr.Status != tool.StatusTimeout {
				t.Fatalf("expected StatusTimeout, got %s", tr.Status)
			}
			return
		}
	}
	t.Fatal("sleep_forever result not found in outcome")
}

// TestRunner_Integration_MaxRoundsTermination verifies max rounds enforcement.
func TestRunner_Integration_MaxRoundsTermination(t *testing.T) {
	fakeModel := &fakeProvider{name: "fake"}
	gw := model.NewGateway(fakeModel, model.NewRoleThrottle(10))
	catalog := tool.NewToolCatalog()
	catalog.RegisterAll([]tool.ToolDefinition{
		{
			Name:        "noop",
			Description: "No-op",
			Effects:     tool.Effects(),
			Handler: func(scope *tool.ToolScope, args json.RawMessage) tool.Result {
				return tool.Succeeded("ok")
			},
		},
	})

	exec := tool.NewExecutor(catalog, nil, nil)
	scope := &tool.ToolScope{Role: "lead", CanRead: true}
	profile := NewLeadProfile("You are a test agent.")
	profile.MaxRounds = 3
	runner := NewRunner(profile, gw, exec, scope)

	fakeModel.toolCalls = []llm.ToolCall{{ID: "call_1", Name: "noop", Arguments: `{}`}}
	// NOT oneShot — we want it to loop until max rounds, then trigger wrap-up

	outcome := runner.Run(context.Background(), []llm.Message{llm.UserMessage("loop")}, "trace-7")

	// After maxRounds is hit, the runner makes a final wrap-up LLM call
	// (Tools=nil). The fake provider still returns content, so wrap-up succeeds.
	if outcome.Error != nil {
		t.Fatalf("expected no error from wrap-up, got %v", outcome.Error)
	}
	if outcome.Rounds != 4 { // 3 tool rounds + 1 wrap-up
		t.Fatalf("expected 4 rounds, got %d", outcome.Rounds)
	}
}

// TestRunner_Integration_MemoryCapabilityDenied verifies memory mutation gating.
func TestRunner_Integration_MemoryCapabilityDenied(t *testing.T) {
	fakeModel := &fakeProvider{name: "fake"}
	gw := model.NewGateway(fakeModel, model.NewRoleThrottle(10))
	catalog := tool.NewToolCatalog()
	catalog.RegisterAll([]tool.ToolDefinition{
		{
			Name:        "memory_write",
			Description: "Write memory",
			Effects:     tool.Effects(tool.EffectMemoryMutation),
			Handler: func(scope *tool.ToolScope, args json.RawMessage) tool.Result {
				return tool.Succeeded("should not run")
			},
		},
	})

	exec := tool.NewExecutor(catalog, nil, nil)
	scope := &tool.ToolScope{Role: "explore", CanRead: true, CanMemory: false}
	profile := NewExploreProfile()
	runner := NewRunner(profile, gw, exec, scope)

	fakeModel.toolCalls = []llm.ToolCall{{ID: "call_1", Name: "memory_write", Arguments: `{"content":"test"}`}}
	fakeModel.content = "done"
	fakeModel.withOneShot()

	outcome := runner.Run(context.Background(), []llm.Message{llm.UserMessage("save memory")}, "trace-8")

	for _, tr := range outcome.ToolResults {
		if tr.Name == "memory_write" {
			if tr.Status != tool.StatusDenied {
				t.Fatalf("expected StatusDenied for memory_write, got %s", tr.Status)
			}
			return
		}
	}
	t.Fatal("memory_write result not found in outcome")
}

// TestRunner_Integration_AutoLessonSkipsSubagent verifies that auto-lesson
// does NOT fire for agents with CanMemory=false (explore/teammate).
func TestRunner_Integration_AutoLessonSkipsSubagent(t *testing.T) {
	fakeModel := &fakeProvider{name: "fake"}
	gw := model.NewGateway(fakeModel, model.NewRoleThrottle(10))
	catalog := tool.NewToolCatalog()
	catalog.RegisterAll([]tool.ToolDefinition{
		{
			Name:        "noop",
			Description: "No-op",
			Effects:     tool.Effects(),
			Handler: func(scope *tool.ToolScope, args json.RawMessage) tool.Result {
				return tool.Succeeded("ok")
			},
		},
	})

	exec := tool.NewExecutor(catalog, nil, nil)
	scope := &tool.ToolScope{Role: "explore", CanRead: true, CanMemory: false}
	profile := NewExploreProfile()
	runner := NewRunner(profile, gw, exec, scope)

	spy := &spyLessonWriter{}
	runner.SetLessonWriter(spy)

	// multiShot=3: return tool_calls for 3 rounds → r.rounds=3, then stop.
	// 4th call returns no tool_calls → hits the auto-lesson check.
	fakeModel.toolCalls = []llm.ToolCall{{ID: "call_1", Name: "noop", Arguments: `{}`}}
	fakeModel.multiShot = 3

	outcome := runner.Run(context.Background(), []llm.Message{llm.UserMessage("explore task")}, "trace-auto-lesson-sub")

	// With CanMemory=false, RecordFailure should never be called.
	if spy.called {
		t.Fatal("auto-lesson should NOT fire for explore profile (CanMemory=false)")
	}
	if outcome.Error != nil {
		t.Fatalf("expected no error, got %v", outcome.Error)
	}
}

// TestRunner_Integration_AutoLessonFiresForLead verifies that auto-lesson
// DOES fire for lead agent (CanMemory=true) after enough rounds.
func TestRunner_Integration_AutoLessonFiresForLead(t *testing.T) {
	fakeModel := &fakeProvider{name: "fake"}
	gw := model.NewGateway(fakeModel, model.NewRoleThrottle(10))
	catalog := tool.NewToolCatalog()
	catalog.RegisterAll([]tool.ToolDefinition{
		{
			Name:        "noop",
			Description: "No-op",
			Effects:     tool.Effects(),
			Handler: func(scope *tool.ToolScope, args json.RawMessage) tool.Result {
				return tool.Succeeded("ok")
			},
		},
	})

	exec := tool.NewExecutor(catalog, nil, nil)
	scope := &tool.ToolScope{Role: "lead", CanRead: true, CanMemory: true}
	profile := NewLeadProfile("You are a test agent.")
	runner := NewRunner(profile, gw, exec, scope)

	spy := &spyLessonWriter{}
	runner.SetLessonWriter(spy)

	// multiShot=3: 3 rounds with tool_calls → r.rounds=3 (>= LessonThreshold),
	// then 4th call returns no tool_calls → auto-lesson fires.
	fakeModel.toolCalls = []llm.ToolCall{{ID: "call_1", Name: "noop", Arguments: `{}`}}
	fakeModel.multiShot = 3

	outcome := runner.Run(context.Background(), []llm.Message{llm.UserMessage("test")}, "trace-auto-lesson-lead")

	// With CanMemory=true and rounds >= LessonThreshold, RecordFailure should be called.
	if !spy.called {
		t.Fatal("auto-lesson SHOULD fire for lead profile (CanMemory=true)")
	}
	if outcome.Error != nil {
		t.Fatalf("expected no error, got %v", outcome.Error)
	}
}

// spyLessonWriter records whether RecordFailure was called.
type spyLessonWriter struct {
	called bool
}

func (s *spyLessonWriter) RecordFailure(ctx context.Context, messages []llm.Message) {
	s.called = true
}
func (s *spyLessonWriter) HasLesson(issue string) bool { return false }

// --- helpers ---

type denyAllApproval struct{}

func (d *denyAllApproval) AllowTool(toolName string, args json.RawMessage) (bool, string) {
	return false, "all tools denied in test"
}
