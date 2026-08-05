package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go-code-agent/internal/config"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/tool"
)

type denyNetwork struct{}

func (denyNetwork) AllowURL(string) bool  { return false }
func (denyNetwork) AllowHost(string) bool { return false }

type redactSanitizer struct{}

func (redactSanitizer) Sanitize(value string) string {
	return strings.ReplaceAll(value, "secret-token", "[REDACTED]")
}

func TestSubagentExecutorAppliesSharedSecurityAndTruncation(t *testing.T) {
	catalog := tool.NewToolCatalog()
	catalog.RegisterAll([]tool.ToolDefinition{
		{
			Name:    "web_fetch",
			Effects: tool.Effects(tool.EffectNetworkAccess),
			Handler: func(*tool.ToolScope, json.RawMessage) tool.Result {
				return tool.Succeeded("should-not-run")
			},
		},
		{
			Name:    "read_file",
			Effects: tool.Effects(tool.EffectReadFile),
			Handler: func(*tool.ToolScope, json.RawMessage) tool.Result {
				return tool.Succeeded("token=secret-token " + strings.Repeat("x", config.SubagentToolOutputMaxChars+256))
			},
		},
	})

	runner := &SubagentRunner{}
	runner.SetExecutorSecurity(denyNetwork{}, redactSanitizer{})
	exec := runner.newExecutor(catalog)

	denied := exec.Execute(context.Background(), &tool.ToolScope{
		Role: "explore", CanNetwork: true,
	}, llm.ToolCall{Name: "web_fetch", Arguments: `{"url":"https://127.0.0.1/"}`})
	if denied.Status != tool.StatusDenied {
		t.Fatalf("web_fetch status = %s, want denied", denied.Status)
	}

	out := exec.Execute(context.Background(), &tool.ToolScope{
		Role: "explore", CanRead: true,
	}, llm.ToolCall{Name: "read_file", Arguments: `{}`})
	if out.Status != tool.StatusSucceeded {
		t.Fatalf("read_file status = %s, want succeeded", out.Status)
	}
	if strings.Contains(out.Output, "secret-token") {
		t.Fatalf("secret leaked through subagent sanitizer chain: %q", out.Output)
	}
	if !strings.Contains(out.Output, "[REDACTED]") {
		t.Fatalf("expected redacted marker, got %q", out.Output)
	}
	if !strings.Contains(out.Output, "output capped") {
		t.Fatalf("expected truncation notice after redaction, got %q", out.Output)
	}
}

func TestTeammateExecutorAppliesSharedSecurity(t *testing.T) {
	catalog := tool.NewToolCatalog()
	catalog.RegisterAll([]tool.ToolDefinition{
		{
			Name:    "web_fetch",
			Effects: tool.Effects(tool.EffectNetworkAccess),
			Handler: func(*tool.ToolScope, json.RawMessage) tool.Result {
				return tool.Succeeded("should-not-run")
			},
		},
		{
			Name:    "bash",
			Effects: tool.Effects(tool.EffectExecuteProcess),
			Handler: func(*tool.ToolScope, json.RawMessage) tool.Result {
				return tool.Succeeded("export TOKEN=secret-token")
			},
		},
	})

	tm := &TeammateManager{catalog: catalog}
	tm.SetExecutorSecurity(denyNetwork{}, redactSanitizer{})
	exec := tm.newExecutor()

	denied := exec.Execute(context.Background(), &tool.ToolScope{
		Role: "teammate", CanNetwork: true,
	}, llm.ToolCall{Name: "web_fetch", Arguments: `{"url":"https://169.254.169.254/"}`})
	if denied.Status != tool.StatusDenied {
		t.Fatalf("web_fetch status = %s, want denied", denied.Status)
	}

	out := exec.Execute(context.Background(), &tool.ToolScope{
		Role: "teammate", CanExecute: true,
	}, llm.ToolCall{Name: "bash", Arguments: `{}`})
	if out.Status != tool.StatusSucceeded {
		t.Fatalf("bash status = %s, want succeeded", out.Status)
	}
	if strings.Contains(out.Output, "secret-token") || !strings.Contains(out.Output, "[REDACTED]") {
		t.Fatalf("teammate output was not sanitized: %q", out.Output)
	}
}
