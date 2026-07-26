package agent

import (
	"context"
	"encoding/json"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/tool"
	"testing"
)

func TestExploreToolNames(t *testing.T) {
	base := exploreToolNames("explore")
	for _, name := range []string{"write_file", "delete_file", "spawn_teammate", "memory_write", "web_fetch"} {
		for _, got := range base {
			if got == name {
				t.Fatalf("explore catalog must not include %q", name)
			}
		}
	}
	web := exploreToolNames("web_fetch")
	want := map[string]bool{"web_fetch": true, "web_search": true}
	if len(web) != len(want) {
		t.Fatalf("web_fetch tools = %v, want exactly %v", web, []string{"web_fetch", "web_search"})
	}
	for _, got := range web {
		if !want[got] {
			t.Fatalf("web_fetch catalog must not include %q (got %v)", got, web)
		}
	}
}

type recordingApproval struct {
	calls []string
}

func (r *recordingApproval) AllowTool(toolName string, _ json.RawMessage) (bool, string) {
	r.calls = append(r.calls, toolName)
	return true, ""
}

func TestExploreCatalogHidesWriteToolsAndUsesApproval(t *testing.T) {
	parent := tool.NewToolCatalog()
	noop := func(scope *tool.ToolScope, args json.RawMessage) tool.Result { return tool.Succeeded("ok") }
	parent.RegisterAll([]tool.ToolDefinition{
		{Name: "bash", Effects: tool.Effects(tool.EffectExecuteProcess), RiskLevel: tool.RiskDanger, Handler: noop},
		{Name: "read_file", Effects: tool.Effects(tool.EffectReadFile), RiskLevel: tool.RiskAuto, Handler: noop},
		{Name: "write_file", Effects: tool.Effects(tool.EffectWriteFile), RiskLevel: tool.RiskDanger, Handler: noop},
		{Name: "list_dir", Effects: tool.Effects(tool.EffectReadFile), RiskLevel: tool.RiskAuto, Handler: noop},
		{Name: "search_file", Effects: tool.Effects(tool.EffectReadFile), RiskLevel: tool.RiskAuto, Handler: noop},
		{Name: "search_content", Effects: tool.Effects(tool.EffectReadFile), RiskLevel: tool.RiskAuto, Handler: noop},
		{Name: "delete_file", Effects: tool.Effects(tool.EffectDeleteFile), RiskLevel: tool.RiskDanger, Handler: noop},
	})

	approval := &recordingApproval{}
	exploreCatalog := parent.Subset(exploreToolNames("explore")...)
	exec := tool.NewExecutor(exploreCatalog, approval, nil)

	if exploreCatalog.IsKnown("write_file") {
		t.Fatal("explore subset must hide write_file")
	}
	denied := exec.Execute(context.Background(), &tool.ToolScope{
		Role: "explore", CanRead: true, CanWrite: false, CanExecute: true,
	}, llm.ToolCall{Name: "write_file", Arguments: `{"path":"x","content":"y"}`})
	if denied.Status != tool.StatusUnavailable {
		t.Fatalf("write_file via explore catalog: status=%s, want unavailable", denied.Status)
	}

	ok := exec.Execute(context.Background(), &tool.ToolScope{
		Role: "explore", CanRead: true, CanWrite: false, CanExecute: true,
	}, llm.ToolCall{Name: "bash", Arguments: `{"command":"ls"}`})
	if ok.Status != tool.StatusSucceeded {
		t.Fatalf("bash: status=%s output=%s", ok.Status, ok.Output)
	}
	if len(approval.calls) != 1 || approval.calls[0] != "bash" {
		t.Fatalf("approval calls=%v, want [bash]", approval.calls)
	}
}
