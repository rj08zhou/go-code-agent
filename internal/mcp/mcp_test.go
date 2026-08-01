package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go-code-agent/internal/tool"
)

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
	if !eff.Has(tool.EffectUnclassified) {
		t.Error("MCP tools without a recognized behavior must be marked unclassified")
	}
}

func TestInferMCPEffectsAvoidsSubstringAndMixedSemanticBypasses(t *testing.T) {
	tests := []struct {
		name             string
		description      string
		wantWrite        bool
		wantRead         bool
		wantUnclassified bool
	}{
		{name: "set_budget", wantWrite: true},
		{name: "forget_user", wantWrite: true},
		{name: "list_and_archive", wantWrite: true, wantRead: true, wantUnclassified: true},
		{name: "target_status", description: "return current status", wantUnclassified: true},
		{name: "list_users", wantRead: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eff := inferMCPEffects(tc.name, tc.description, nil)
			if eff.Has(tool.EffectWriteFile) != tc.wantWrite {
				t.Errorf("write effect = %v, want %v", eff.Has(tool.EffectWriteFile), tc.wantWrite)
			}
			if eff.Has(tool.EffectReadFile) != tc.wantRead {
				t.Errorf("read effect = %v, want %v", eff.Has(tool.EffectReadFile), tc.wantRead)
			}
			if eff.Has(tool.EffectUnclassified) != tc.wantUnclassified {
				t.Errorf("unclassified effect = %v, want %v", eff.Has(tool.EffectUnclassified), tc.wantUnclassified)
			}
		})
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

func TestMCPHandlerHonorsScopeContextCancellation(t *testing.T) {
	catalog := tool.NewToolCatalog()
	mgr := NewManager(t.TempDir())
	adapter := NewToolCatalogAdapter(catalog, mgr)
	adapter.RegisterMCPTools("test", []ToolInfo{
		{Name: "slow", Description: "hangs until cancelled", Schema: map[string]any{}},
	})

	c, requests, _ := newPipeClient(t)
	c.config.Name = "test"
	mgr.mu.Lock()
	mgr.clients["test"] = c
	mgr.mu.Unlock()

	go func() {
		_, _ = requests.ReadBytes('\n') // consume request, never respond
	}()

	handler := catalog.Load().Handlers["mcp__test__slow"]
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	result := handler(&tool.ToolScope{
		Context:    ctx,
		Role:       "lead",
		CanNetwork: true,
	}, []byte(`{}`))
	elapsed := time.Since(start)

	if result.Succeeded() {
		t.Fatalf("expected failure on cancel, got %#v", result)
	}
	if !strings.Contains(result.Output, context.DeadlineExceeded.Error()) &&
		!strings.Contains(result.Output, "context deadline exceeded") {
		t.Fatalf("result = %q, want deadline exceeded", result.Output)
	}
	if elapsed > time.Second {
		t.Fatalf("handler ignored scope.Context cancel: took %s", elapsed)
	}
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

func TestManagerLoadAndStartReturnsAndRecordsFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-mcp-server")
	config, err := json.Marshal([]map[string]any{{"name": "auto-broken", "command": missing}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCP_SERVERS", string(config))
	mgr := NewManager(t.TempDir())

	if err := mgr.LoadAndStart(context.Background()); err == nil {
		t.Fatal("LoadAndStart returned nil error for a missing executable")
	}
	if mgr.Count() != 0 || mgr.FailedCount() != 1 {
		t.Fatalf("counts after auto-start failure: active=%d failed=%d", mgr.Count(), mgr.FailedCount())
	}
}

func TestManagerConnectReportsStartFailure(t *testing.T) {
	mgr := NewManager(t.TempDir())
	missing := filepath.Join(t.TempDir(), "missing-mcp-server")

	if _, err := mgr.Connect(context.Background(), "broken", missing, nil); err == nil {
		t.Fatal("Connect returned nil error for a missing executable")
	}
	if got := mgr.Count(); got != 0 {
		t.Fatalf("active server count = %d, want 0", got)
	}
	if got := mgr.FailedCount(); got != 1 {
		t.Fatalf("failed server count = %d, want 1", got)
	}
	status := mgr.Status()
	if !strings.Contains(status, "failed:") || !strings.Contains(status, "broken") {
		t.Fatalf("status does not expose failed server: %q", status)
	}
}

func TestManagerConnectHonorsContextCancellation(t *testing.T) {
	mgr, _ := newHelperManager(t, "initialize-hang")
	command, args := helperCommand()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := mgr.Connect(ctx, "cancelled", command, args); err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("Connect error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancelled Connect took %s", elapsed)
	}
	if mgr.Count() != 0 || mgr.FailedCount() != 1 {
		t.Fatalf("counts after cancellation: active=%d failed=%d", mgr.Count(), mgr.FailedCount())
	}
}

func TestManagerConnectInitializationFailureRollsBack(t *testing.T) {
	mgr, catalog := newHelperManager(t, "initialize-error")
	command, args := helperCommand()

	if _, err := mgr.Connect(context.Background(), "init-fails", command, args); err == nil {
		t.Fatal("Connect returned nil error when initialize failed")
	}
	if got := mgr.Count(); got != 0 {
		t.Fatalf("active server count = %d, want 0", got)
	}
	if got := mgr.FailedCount(); got != 1 {
		t.Fatalf("failed server count = %d, want 1", got)
	}
	if catalog.IsKnown("mcp__init-fails__echo") {
		t.Fatal("initialization failure registered a tool")
	}
}

func TestManagerConnectDiscoveryFailureRollsBack(t *testing.T) {
	mgr, catalog := newHelperManager(t, "discover-error")
	command, args := helperCommand()

	if _, err := mgr.Connect(context.Background(), "discover-fails", command, args); err == nil {
		t.Fatal("Connect returned nil error when tools/list failed")
	}
	if got := mgr.Count(); got != 0 {
		t.Fatalf("active server count = %d, want 0", got)
	}
	if got := mgr.FailedCount(); got != 1 {
		t.Fatalf("failed server count = %d, want 1", got)
	}
	if catalog.IsKnown("mcp__discover-fails__echo") {
		t.Fatal("failed server tool remained registered")
	}
	if status := mgr.Status(); !strings.Contains(status, "discover-fails") || !strings.Contains(status, "failed:") {
		t.Fatalf("status does not expose discovery failure: %q", status)
	}
}

func TestManagerApproveFailureRemainsRetryable(t *testing.T) {
	mgr, catalog := newHelperManager(t, "success")
	name := "approval-retry"
	mgr.mu.Lock()
	mgr.pendingServers[name] = ServerConfig{
		Name: name, Command: filepath.Join(t.TempDir(), "missing-mcp-server"), Env: os.Environ(),
	}
	mgr.mu.Unlock()

	if _, err := mgr.Approve(context.Background(), name); err == nil {
		t.Fatal("Approve returned nil error for a missing executable")
	}
	status := mgr.Status()
	if !strings.Contains(status, "failed:") || !strings.Contains(status, "Retry: /mcp approve "+name) {
		t.Fatalf("failed approval is not shown as retryable: %q", status)
	}

	command, args := helperCommand()
	mgr.mu.Lock()
	cfg := mgr.pendingServers[name]
	cfg.Command = command
	cfg.Args = args
	cfg.Env = os.Environ()
	mgr.pendingServers[name] = cfg
	mgr.mu.Unlock()

	toolCount, err := mgr.Approve(context.Background(), name)
	if err != nil {
		t.Fatalf("Approve retry: %v", err)
	}
	if toolCount != 1 {
		t.Fatalf("registered tool count = %d, want 1", toolCount)
	}
	if mgr.FailedCount() != 0 || mgr.Count() != 1 {
		t.Fatalf("counts after retry: active=%d failed=%d", mgr.Count(), mgr.FailedCount())
	}
	if !catalog.IsKnown("mcp__" + name + "__echo") {
		t.Fatal("successful retry did not register discovered tool")
	}
}

func TestManagerConcurrentConnectPublishesOnlyOneServer(t *testing.T) {
	mgr, catalog := newHelperManager(t, "success")
	command, args := helperCommand()

	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := mgr.Connect(context.Background(), "single", command, args)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	successes := 0
	failures := 0
	for err := range errs {
		if err == nil {
			successes++
		} else {
			failures++
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("connect results: successes=%d failures=%d, want 1/1", successes, failures)
	}
	if mgr.Count() != 1 || !catalog.IsKnown("mcp__single__echo") {
		t.Fatalf("concurrent connection did not publish exactly one usable server")
	}
}

func TestManagerDisconnectUnregistersTools(t *testing.T) {
	mgr, catalog := newHelperManager(t, "success")
	command, args := helperCommand()

	toolCount, err := mgr.Connect(context.Background(), "healthy", command, args)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if toolCount != 1 || !catalog.IsKnown("mcp__healthy__echo") {
		t.Fatalf("successful connection did not register one tool")
	}
	status := mgr.Status()
	if !strings.Contains(status, "active:") || !strings.Contains(status, "healthy (healthy, 1 tool)") {
		t.Fatalf("active status missing health and tool count: %q", status)
	}

	if err := mgr.Disconnect("healthy"); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if mgr.Count() != 0 {
		t.Fatalf("active server count after disconnect = %d, want 0", mgr.Count())
	}
	if catalog.IsKnown("mcp__healthy__echo") {
		t.Fatal("disconnected server tool remained registered")
	}
}

func newHelperManager(t *testing.T, mode string) (*Manager, *tool.ToolCatalog) {
	t.Helper()
	t.Setenv("GO_WANT_MCP_HELPER", "1")
	t.Setenv("MCP_HELPER_MODE", mode)
	mgr := NewManager(t.TempDir())
	catalog := tool.NewToolCatalog()
	mgr.SetRegistry(NewToolCatalogAdapter(catalog, mgr))
	t.Cleanup(mgr.Shutdown)
	return mgr, catalog
}

func helperCommand() (string, []string) {
	return os.Args[0], []string{"-test.run=^TestMCPHelperProcess$"}
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

func TestClientCallToolMapsMCPErrorToFailure(t *testing.T) {
	c, requests, responses := newPipeClient(t)
	go func() {
		line, err := requests.ReadBytes('\n')
		if err != nil {
			return
		}
		var req rpcRequest
		if json.Unmarshal(line, &req) != nil {
			return
		}
		_, _ = fmt.Fprintf(responses,
			`{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"denied"}],"isError":true}}`+"\n",
			req.ID,
		)
	}()

	output, err := c.CallTool(context.Background(), "fails", nil)
	if err == nil {
		t.Fatalf("CallTool returned nil error for isError response: output=%q", output)
	}
	if !strings.Contains(err.Error(), "denied") {
		t.Fatalf("CallTool error = %q, want server error content", err)
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

// TestMCPHelperProcess runs as a subprocess for Manager lifecycle tests.
func TestMCPHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_MCP_HELPER") != "1" {
		return
	}

	mode := os.Getenv("MCP_HELPER_MODE")
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var req rpcRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			os.Exit(2)
		}
		switch req.Method {
		case "initialize":
			if mode == "initialize-hang" {
				time.Sleep(10 * time.Second)
				return
			}
			if mode == "initialize-error" {
				_ = encoder.Encode(map[string]any{
					"jsonrpc": "2.0", "id": req.ID,
					"error": map[string]any{"code": -32000, "message": "initialize rejected"},
				})
				return
			}
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"capabilities":    map[string]any{},
					"serverInfo":      map[string]any{"name": "test-helper", "version": "1.0.0"},
					"instructions":    "test helper instructions",
				},
			})
		case "notifications/initialized":
			// Notifications have no response.
		case "tools/list":
			if mode == "discover-error" {
				_ = encoder.Encode(map[string]any{
					"jsonrpc": "2.0", "id": req.ID,
					"error": map[string]any{"code": -32001, "message": "tool discovery rejected"},
				})
				return
			}
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": []map[string]any{{
					"name": "echo", "description": "Echo input", "inputSchema": map[string]any{"type": "object"},
				}}},
			})
		default:
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32601, "message": "method not found"},
			})
		}
	}
}
