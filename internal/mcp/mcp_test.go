package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/internal/config"
	"go-code-agent/internal/tool"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func init() { config.SetConfig(config.Load()) }

// --- inferMCPEffects ---

func TestInferMCPEffects_Write(t *testing.T) {
	eff := inferMCPEffects("db_write", "write row to database", nil)
	if !eff.Has(tool.EffectNetworkAccess) {
		t.Error("MCP tools should always have EffectNetworkAccess")
	}
	if !eff.Has(tool.EffectWriteFile) {
		t.Error("db_write should have EffectWriteFile")
	}
}

func TestInferMCPEffects_Read(t *testing.T) {
	eff := inferMCPEffects("db_query", "query database records", nil)
	if !eff.Has(tool.EffectReadFile) {
		t.Error("db_query should have EffectReadFile")
	}
}

func TestInferMCPEffects_Execute(t *testing.T) {
	eff := inferMCPEffects("remote_exec", "execute command on remote", nil)
	if !eff.Has(tool.EffectExecuteProcess) {
		t.Error("remote_exec should have EffectExecuteProcess")
	}
}

func TestInferMCPEffects_NoEffect(t *testing.T) {
	eff := inferMCPEffects("ping", "check connectivity", nil)
	if eff.Has(tool.EffectWriteFile) || eff.Has(tool.EffectExecuteProcess) {
		t.Error("ping should not have write/execute effects")
	}
	if !eff.Has(tool.EffectNetworkAccess) {
		t.Error("all MCP tools should have EffectNetworkAccess")
	}
}

// --- ToolCatalogAdapter ---

func TestToolCatalogAdapter_RegisterMCPTools(t *testing.T) {
	catalog := tool.NewToolCatalog()
	mgr := NewManager(t.TempDir())
	adapter := NewToolCatalogAdapter(catalog, mgr)

	tools := []ToolInfo{
		{Name: "list_users", Description: "List all users", Schema: map[string]any{}},
		{Name: "delete_user", Description: "Delete a user", Schema: map[string]any{}},
	}

	adapter.RegisterMCPTools("auth", tools)

	// Verify registration
	fullName1 := "mcp__auth__list_users"
	if !catalog.IsKnown(fullName1) {
		t.Errorf("%s should be registered in catalog", fullName1)
	}
	fullName2 := "mcp__auth__delete_user"
	if !catalog.IsKnown(fullName2) {
		t.Errorf("%s should be registered in catalog", fullName2)
	}

	// Verify effects inference
	snap := catalog.Load()
	if def, ok := snap.Definitions[fullName2]; ok {
		if !def.Effects.Has(tool.EffectWriteFile) {
			t.Error("delete_user should have EffectWriteFile")
		}
		if def.Timeout != 30*time.Second {
			t.Errorf("MCP tool timeout = %s, want 30s", def.Timeout)
		}
	}
}

func TestToolCatalogAdapter_CallMCPToolHandler(t *testing.T) {
	catalog := tool.NewToolCatalog()
	mgr := NewManager(t.TempDir())
	adapter := NewToolCatalogAdapter(catalog, mgr)

	tools := []ToolInfo{
		{Name: "echo", Description: "Echo back input", Schema: map[string]any{}},
	}
	adapter.RegisterMCPTools("test", tools)

	snap := catalog.Load()
	handler, ok := snap.Handlers["mcp__test__echo"]
	if !ok {
		t.Fatal("handler not registered")
	}

	// Call handler — will fail because MCP server is not running, but handler
	// should return a structured error, not panic.
	scope := &tool.ToolScope{Role: "lead", CanNetwork: true}
	result := handler(scope, []byte(`{"msg":"hello"}`))
	// MCP server is not running, so the handler should return a non-empty result
	if result.Status == "" {
		t.Error("result status should not be empty even on failure")
	}
	t.Logf("MCP call result (expected error): %s", result.Status)
}

// --- IsMCPTool ---

func TestIsMCPTool(t *testing.T) {
	if !IsMCPTool("mcp__github__list_repos") {
		t.Error("expected true for MCP tool name")
	}
	if IsMCPTool("read_file") {
		t.Error("expected false for non-MCP tool name")
	}
}

// --- Manager lifecycle ---

func TestManager_CountAndList(t *testing.T) {
	mgr := NewManager(t.TempDir())
	if mgr.Count() != 0 {
		t.Error("empty manager should have count 0")
	}
	if mgr.List() == "" {
		t.Error("list should return a non-empty string")
	}
}

func TestManager_ShutdownIdempotent(t *testing.T) {
	mgr := NewManager(t.TempDir())
	mgr.Shutdown()
	mgr.Shutdown() // should not panic
}

// --- Manager CallTool with no server ---

func TestManager_CallTool_NoServer(t *testing.T) {
	mgr := NewManager(t.TempDir())
	_, err := mgr.CallTool(context.Background(), "mcp__nonexistent__tool", nil)
	if err == nil {
		t.Error("expected error for nonexistent server")
	}
}

func TestManager_CallTool_InvalidName(t *testing.T) {
	mgr := NewManager(t.TempDir())
	_, err := mgr.CallTool(context.Background(), "not-an-mcp-tool", nil)
	if err == nil {
		t.Error("expected error for invalid MCP tool name")
	}
}

func newPipeClient(t *testing.T) (*Client, *bufio.Reader, io.WriteCloser) {
	t.Helper()
	serverRead, clientWrite := io.Pipe()
	clientRead, serverWrite := io.Pipe()
	c := NewClient(ServerConfig{Name: "test"})
	c.stdin = clientWrite
	c.stdout = clientRead
	c.reader = bufio.NewReader(clientRead)
	c.running = true
	t.Cleanup(func() {
		_ = c.Stop()
		_ = serverRead.Close()
		_ = serverWrite.Close()
	})
	return c, bufio.NewReader(serverRead), serverWrite
}

func TestClientCallToolHonorsContextCancellation(t *testing.T) {
	c, requests, _ := newPipeClient(t)
	go func() {
		_, _ = requests.ReadBytes('\n') // consume request but never respond
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.CallTool(ctx, "slow", nil)
	if err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("CallTool error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("CallTool cancellation took %s", elapsed)
	}
}

func TestClientConcurrentCallsRemainFramed(t *testing.T) {
	c, requests, responses := newPipeClient(t)
	go func() {
		for i := 0; i < 2; i++ {
			line, err := requests.ReadBytes('\n')
			if err != nil {
				return
			}
			var req rpcRequest
			if json.Unmarshal(line, &req) != nil {
				return
			}
			params, _ := req.Params.(map[string]any)
			name, _ := params["name"].(string)
			resp := fmt.Sprintf(
				`{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":%q}]}}`+"\n",
				req.ID, name,
			)
			_, _ = io.WriteString(responses, resp)
		}
	}()

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, name := range []string{"one", "two"} {
		name := name
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := c.CallTool(context.Background(), name, nil)
			if err == nil && got != "["+name+"]" {
				err = fmt.Errorf("result = %q, want [%s]", got, name)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}
