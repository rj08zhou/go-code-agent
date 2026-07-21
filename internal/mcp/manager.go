package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent-refactor/internal/tool"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Manager manages MCP server subprocesses and their tool registrations.
type Manager struct {
	workdir        string
	mu             sync.Mutex
	clients        map[string]*Client
	pendingServers map[string]ServerConfig
	registry       ToolRegistry
}

// ToolRegistry is called when MCP tools are discovered, to register them into the ToolCatalog.
type ToolRegistry interface {
	RegisterMCPTools(serverName string, tools []ToolInfo)
}

func NewManager(workdir string) *Manager {
	return &Manager{
		workdir:        workdir,
		clients:        make(map[string]*Client),
		pendingServers: make(map[string]ServerConfig),
	}
}

// SetRegistry sets the tool catalog for MCP tool registration.
func (m *Manager) SetRegistry(registry ToolRegistry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registry = registry
}

// LoadAndStart parses MCP_SERVERS env var and .mcp.json, starts approved servers,
// discovering and registering their tools into the ToolCatalog.
// MCP_SERVERS servers auto-start; .mcp.json servers require /mcp approve first.
func (m *Manager) LoadAndStart(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 1. Auto-start from MCP_SERVERS env var
	envConfigs := parseMCPConfigEnv()
	for _, cfg := range envConfigs {
		if _, exists := m.clients[cfg.Name]; exists {
			continue
		}
		m.startServer(ctx, cfg)
	}

	// 2. Load .mcp.json into pending list (require /mcp approve)
	fileConfigs := parseMCPConfigFile(m.workdir)
	for _, cfg := range fileConfigs {
		if _, exists := m.clients[cfg.Name]; exists {
			continue
		}
		m.pendingServers[cfg.Name] = cfg
	}
	if len(fileConfigs) > 0 {
		log.Printf("[MCP] %d server(s) pending approval from .mcp.json", len(fileConfigs))
	}
	return nil
}

func (m *Manager) startServer(ctx context.Context, cfg ServerConfig) {
	client := NewClient(cfg)
	if err := client.Start(ctx); err != nil {
		log.Printf("[MCP] Failed to start %s: %v", cfg.Name, err)
		return
	}
	m.clients[cfg.Name] = client
	client.startHealthLoop(ctx)

	tools, err := client.DiscoverTools(ctx)
	if err != nil {
		log.Printf("[MCP] Failed to discover tools from %s: %v", cfg.Name, err)
		return
	}
	if m.registry != nil {
		m.registry.RegisterMCPTools(cfg.Name, tools)
	}
	log.Printf("[MCP] Server %s: %d tools registered", cfg.Name, len(tools))
}

// Approve starts a pending MCP server from .mcp.json.
func (m *Manager) Approve(ctx context.Context, name string) string {
	m.mu.Lock()
	cfg, ok := m.pendingServers[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Sprintf("Error: no pending server named '%s'", name)
	}
	delete(m.pendingServers, name)
	m.mu.Unlock()
	m.startServer(ctx, cfg)
	return fmt.Sprintf("Approved and started MCP server '%s'", name)
}

// Connect starts a server from an interactive command.
func (m *Manager) Connect(ctx context.Context, name, command string, args []string) string {
	m.mu.Lock()
	if _, exists := m.clients[name]; exists {
		m.mu.Unlock()
		return fmt.Sprintf("MCP server %q already connected", name)
	}
	cfg := ServerConfig{Name: name, Command: command, Args: args, Env: os.Environ()}
	m.mu.Unlock()
	m.mu.Lock()
	m.startServer(ctx, cfg)
	m.mu.Unlock()
	return fmt.Sprintf("MCP server %q connected", name)
}

// Disconnect stops an active server.
func (m *Manager) Disconnect(name string) string {
	m.mu.Lock()
	client, ok := m.clients[name]
	if ok {
		delete(m.clients, name)
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Sprintf("MCP server %q not connected", name)
	}
	_ = client.Stop()
	return fmt.Sprintf("MCP server %q disconnected", name)
}

// ServerInstructions returns the combined instructions from all active MCP servers.
func (m *Manager) ServerInstructions() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var parts []string
	for _, c := range m.clients {
		if c.instructions != "" {
			parts = append(parts, c.instructions)
		}
	}
	return strings.Join(parts, "\n")
}

// ListPending returns names of servers awaiting approval.
func (m *Manager) ListPending() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var names []string
	for n := range m.pendingServers {
		names = append(names, n)
	}
	return names
}

// Shutdown stops all MCP server subprocesses.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, client := range m.clients {
		if err := client.Stop(); err != nil {
			log.Printf("[MCP] Error stopping %s: %v", name, err)
		}
	}
	m.clients = make(map[string]*Client)
}

// CallTool invokes an MCP tool by its fully qualified name (mcp__server__tool).
func (m *Manager) CallTool(ctx context.Context, fullName string, args map[string]any) (string, error) {
	// Parse: mcp__<server>__<tool>
	parts := strings.SplitN(fullName, "__", 3)
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid MCP tool name: %s", fullName)
	}
	serverName := parts[1]
	toolName := parts[2]

	m.mu.Lock()
	client, ok := m.clients[serverName]
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("MCP server %s not found", serverName)
	}
	return client.CallTool(ctx, toolName, args)
}

// IsMCPTool reports whether a tool name belongs to an MCP server.
func IsMCPTool(name string) bool {
	return strings.HasPrefix(name, "mcp__")
}

func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.clients)
}

func (m *Manager) List() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.clients) == 0 {
		return "No MCP servers configured."
	}
	var names []string
	for _, c := range m.clients {
		names = append(names, c.config.Name)
	}
	return strings.Join(names, ", ")
}

// parseMCPConfigFile reads .mcp.json from the working directory.
func parseMCPConfigFile(workdir string) []ServerConfig {
	path := filepath.Join(workdir, ".mcp.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	type rawServer struct {
		Name    string            `json:"name"`
		Command string            `json:"command"`
		Args    []string          `json:"args,omitempty"`
		Env     map[string]string `json:"env,omitempty"`
	}
	var named map[string]rawServer
	if json.Unmarshal(data, &named) == nil && named != nil {
		if servers, ok := named["servers"]; ok {
			// The README-compatible object format has a nested servers object.
			_ = servers
		}
	}
	var envelope struct {
		Servers map[string]rawServer `json:"servers"`
	}
	var rawList []rawServer
	if json.Unmarshal(data, &rawList) == nil {
		// array format: name is part of each entry
	} else if json.Unmarshal(data, &envelope) == nil && envelope.Servers != nil {
		for name, server := range envelope.Servers {
			server.Name = name
			rawList = append(rawList, server)
		}
	} else {
		log.Printf("[MCP] Failed to parse %s", path)
		return nil
	}
	var configs []ServerConfig
	for _, r := range rawList {
		if r.Name == "" || r.Command == "" {
			continue
		}
		var envVars []string
		for k, v := range r.Env {
			envVars = append(envVars, k+"="+v)
		}
		configs = append(configs, ServerConfig{Name: r.Name, Command: r.Command, Args: r.Args, Env: append(os.Environ(), envVars...)})
	}
	return configs
}

// parseMCPConfigEnv parses the MCP_SERVERS environment variable.
// Format: JSON array of {name, command, args?, env?} objects.
func parseMCPConfigEnv() []ServerConfig {
	data := os.Getenv("MCP_SERVERS")
	if data == "" {
		return nil
	}
	var raw []struct {
		Name    string            `json:"name"`
		Command string            `json:"command"`
		Args    []string          `json:"args,omitempty"`
		Env     map[string]string `json:"env,omitempty"`
	}
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		log.Printf("[MCP] Failed to parse MCP_SERVERS: %v", err)
		return nil
	}

	var configs []ServerConfig
	for _, r := range raw {
		if r.Name == "" || r.Command == "" {
			continue
		}
		var envVars []string
		for k, v := range r.Env {
			envVars = append(envVars, k+"="+v)
		}
		configs = append(configs, ServerConfig{
			Name:    r.Name,
			Command: r.Command,
			Args:    r.Args,
			Env:     append(os.Environ(), envVars...),
		})
	}
	return configs
}

// --- ToolCatalogAdapter implements ToolRegistry to register MCP tools ---

// ToolCatalogAdapter bridges MCP tool discovery with the ToolCatalog.
type ToolCatalogAdapter struct {
	catalog *tool.ToolCatalog
	mcpMgr  *Manager
}

func NewToolCatalogAdapter(catalog *tool.ToolCatalog, mcpMgr *Manager) *ToolCatalogAdapter {
	return &ToolCatalogAdapter{catalog: catalog, mcpMgr: mcpMgr}
}

// RegisterMCPTools converts discovered MCP tools into ToolDefinitions and
// atomically adds them to the ToolCatalog.
func (a *ToolCatalogAdapter) RegisterMCPTools(serverName string, tools []ToolInfo) {
	var defs []tool.ToolDefinition
	for _, t := range tools {
		fullName := fmt.Sprintf("mcp__%s__%s", serverName, t.Name)
		schema := tool.MustMarshalJSON(t.Schema)

		// Infer Effects from schema/description for capability gating
		effects := inferMCPEffects(t.Name, t.Description, t.Schema)

		defs = append(defs, tool.ToolDefinition{
			Name:        fullName,
			Description: fmt.Sprintf("[MCP:%s] %s", serverName, t.Description),
			RiskLevel:   tool.RiskDanger, // MCP tools default to dangerous; override via config
			Effects:     effects,
			Schema:      schema,
			Timeout:     30, // 30s default for MCP tools
			Handler: func(scope *tool.ToolScope, args json.RawMessage) tool.Result {
				ctx := context.Background()
				var argMap map[string]any
				json.Unmarshal(args, &argMap)
				result, err := a.mcpMgr.CallTool(ctx, fullName, argMap)
				if err != nil {
					return tool.Failed(fmt.Sprintf("MCP error: %v", err))
				}
				return tool.Succeeded(result)
			},
		})
	}
	a.catalog.Register(defs)
}

// inferMCPEffects guesses tool effects from name and description keywords.
// MCP tools default to NetworkAccess because they are remote calls.
func inferMCPEffects(name, desc string, schema map[string]any) tool.EffectSet {
	combined := strings.ToLower(name + " " + desc)
	var effects []tool.Effect

	effects = append(effects, tool.EffectNetworkAccess) // all MCP tools are network calls

	if strings.Contains(combined, "write") || strings.Contains(combined, "create") ||
		strings.Contains(combined, "update") || strings.Contains(combined, "delete") ||
		strings.Contains(combined, "insert") || strings.Contains(combined, "remove") {
		effects = append(effects, tool.EffectWriteFile)
	}
	if strings.Contains(combined, "read") || strings.Contains(combined, "query") ||
		strings.Contains(combined, "search") || strings.Contains(combined, "get") ||
		strings.Contains(combined, "list") || strings.Contains(combined, "fetch") {
		effects = append(effects, tool.EffectReadFile)
	}
	if strings.Contains(combined, "exec") || strings.Contains(combined, "run") ||
		strings.Contains(combined, "command") || strings.Contains(combined, "shell") {
		effects = append(effects, tool.EffectExecuteProcess)
	}
	return tool.Effects(effects...)
}
