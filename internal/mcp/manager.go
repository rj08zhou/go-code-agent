package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"go-code-agent/internal/tool"
)

// Manager manages MCP server subprocesses and their tool registrations.
type Manager struct {
	workdir        string
	mu             sync.Mutex
	clients        map[string]*Client
	pendingServers map[string]ServerConfig
	failures       map[string]string
	connecting     map[string]struct{}
	toolCounts     map[string]int
	registry       ToolRegistry
	closed         bool
}

// ToolRegistry manages MCP tools in the runtime ToolCatalog.
type ToolRegistry interface {
	RegisterMCPTools(serverName string, tools []ToolInfo)
	UnregisterMCPTools(serverName string)
}

func NewManager(workdir string) *Manager {
	return &Manager{
		workdir:        workdir,
		clients:        make(map[string]*Client),
		pendingServers: make(map[string]ServerConfig),
		failures:       make(map[string]string),
		connecting:     make(map[string]struct{}),
		toolCounts:     make(map[string]int),
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
	var startupErrors []error

	// 1. Auto-start from MCP_SERVERS env var.
	for _, cfg := range parseMCPConfigEnv() {
		m.mu.Lock()
		_, active := m.clients[cfg.Name]
		_, connecting := m.connecting[cfg.Name]
		m.mu.Unlock()
		if active || connecting {
			continue
		}
		if _, err := m.startServer(ctx, cfg); err != nil {
			startupErrors = append(startupErrors, fmt.Errorf("%s: %w", cfg.Name, err))
		}
	}

	// 2. Load .mcp.json into the pending list (require /mcp approve).
	pendingCount := 0
	for _, cfg := range parseMCPConfigFile(m.workdir) {
		if err := validateServerConfig(cfg); err != nil {
			startupErrors = append(startupErrors, err)
			continue
		}
		m.mu.Lock()
		if _, active := m.clients[cfg.Name]; !active {
			m.pendingServers[cfg.Name] = cfg
			pendingCount++
		}
		m.mu.Unlock()
	}
	if pendingCount > 0 {
		log.Printf("[MCP] %d server(s) pending approval from .mcp.json", pendingCount)
	}
	return errors.Join(startupErrors...)
}

func validateServerConfig(cfg ServerConfig) error {
	if strings.TrimSpace(cfg.Name) == "" {
		return errors.New("MCP server name is required")
	}
	if cfg.Name != strings.TrimSpace(cfg.Name) || strings.Contains(cfg.Name, "__") {
		return fmt.Errorf("invalid MCP server name %q", cfg.Name)
	}
	for _, r := range cfg.Name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_' {
			continue
		}
		return fmt.Errorf("invalid MCP server name %q: only letters, digits, '.', '-' and '_' are allowed", cfg.Name)
	}
	if strings.TrimSpace(cfg.Command) == "" {
		return fmt.Errorf("MCP server %q command is required", cfg.Name)
	}
	return nil
}

// startServer publishes a server only after start, initialize, tool discovery,
// and catalog registration have all completed successfully.
func (m *Manager) startServer(ctx context.Context, cfg ServerConfig) (toolCount int, retErr error) {
	if err := validateServerConfig(cfg); err != nil {
		return 0, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return 0, errors.New("MCP manager is closed")
	}
	if _, exists := m.clients[cfg.Name]; exists {
		m.mu.Unlock()
		return 0, fmt.Errorf("MCP server %q is already connected", cfg.Name)
	}
	if _, exists := m.connecting[cfg.Name]; exists {
		m.mu.Unlock()
		return 0, fmt.Errorf("MCP server %q is already connecting", cfg.Name)
	}
	m.connecting[cfg.Name] = struct{}{}
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		delete(m.connecting, cfg.Name)
		if retErr != nil && !m.closed {
			m.failures[cfg.Name] = strings.ReplaceAll(retErr.Error(), "\n", " ")
		}
		m.mu.Unlock()
	}()

	client := NewClient(cfg)
	if err := client.Start(ctx); err != nil {
		retErr = fmt.Errorf("start MCP server %q: %w", cfg.Name, err)
		if cleanupErr := client.Stop(); cleanupErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("cleanup MCP server %q: %w", cfg.Name, cleanupErr))
		}
		return 0, retErr
	}

	tools, err := client.DiscoverTools(ctx)
	if err != nil {
		retErr = fmt.Errorf("discover tools from MCP server %q: %w", cfg.Name, err)
		if cleanupErr := client.Stop(); cleanupErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("cleanup MCP server %q: %w", cfg.Name, cleanupErr))
		}
		return 0, retErr
	}
	if err := ctx.Err(); err != nil {
		retErr = fmt.Errorf("connect MCP server %q: %w", cfg.Name, err)
		_ = client.Stop()
		return 0, retErr
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = client.Stop()
		return 0, errors.New("MCP manager closed while connecting")
	}
	// Publish the tools and client under the same manager lock. A handler that
	// becomes visible concurrently will block in CallTool until the client map
	// contains the matching server.
	if m.registry != nil {
		m.registry.RegisterMCPTools(cfg.Name, tools)
	}
	m.clients[cfg.Name] = client
	m.toolCounts[cfg.Name] = len(tools)
	delete(m.failures, cfg.Name)
	m.mu.Unlock()

	client.startHealthLoop(ctx)
	log.Printf("[MCP] Server %s: %d tools registered", cfg.Name, len(tools))
	return len(tools), nil
}

// Approve starts a pending MCP server from .mcp.json. A failed server remains
// retryable with the same command and is exposed through Status as failed.
func (m *Manager) Approve(ctx context.Context, name string) (int, error) {
	m.mu.Lock()
	cfg, ok := m.pendingServers[name]
	m.mu.Unlock()
	if !ok {
		return 0, fmt.Errorf("no pending MCP server named %q", name)
	}

	toolCount, err := m.startServer(ctx, cfg)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	delete(m.pendingServers, name)
	m.mu.Unlock()
	return toolCount, nil
}

// Connect starts a server from an interactive command.
func (m *Manager) Connect(ctx context.Context, name, command string, args []string) (int, error) {
	cfg := ServerConfig{Name: name, Command: command, Args: args, Env: os.Environ()}
	return m.startServer(ctx, cfg)
}

// Disconnect stops an active server and removes its tools from the catalog.
func (m *Manager) Disconnect(name string) error {
	m.mu.Lock()
	client, ok := m.clients[name]
	registry := m.registry
	if ok {
		delete(m.clients, name)
		delete(m.toolCounts, name)
		delete(m.failures, name)
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("MCP server %q is not connected", name)
	}
	if registry != nil {
		registry.UnregisterMCPTools(name)
	}
	if err := client.Stop(); err != nil {
		return fmt.Errorf("stop MCP server %q: %w", name, err)
	}
	return nil
}

// ServerInstructions returns the combined instructions from all active MCP servers.
func (m *Manager) ServerInstructions() string {
	m.mu.Lock()
	names := make([]string, 0, len(m.clients))
	clients := make(map[string]*Client, len(m.clients))
	for name, client := range m.clients {
		names = append(names, name)
		clients[name] = client
	}
	m.mu.Unlock()

	sort.Strings(names)
	var parts []string
	for _, name := range names {
		if instructions := clients[name].instructions; instructions != "" {
			parts = append(parts, instructions)
		}
	}
	return strings.Join(parts, "\n")
}

// ListPending returns sorted names that are awaiting first approval. Servers
// whose last start failed are shown separately by Status and remain retryable.
func (m *Manager) ListPending() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var names []string
	for name := range m.pendingServers {
		if _, failed := m.failures[name]; !failed {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// FailedCount returns the number of servers whose most recent start failed.
func (m *Manager) FailedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.failures)
}

// Shutdown stops all MCP server subprocesses and removes their tools.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	clients := m.clients
	registry := m.registry
	m.clients = make(map[string]*Client)
	m.toolCounts = make(map[string]int)
	m.mu.Unlock()

	for name, client := range clients {
		if registry != nil {
			registry.UnregisterMCPTools(name)
		}
		if err := client.Stop(); err != nil {
			log.Printf("[MCP] Error stopping %s: %v", name, err)
		}
	}
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

// List returns active server names for model context.
func (m *Manager) List() string {
	m.mu.Lock()
	names := make([]string, 0, len(m.clients))
	for name := range m.clients {
		names = append(names, name)
	}
	m.mu.Unlock()
	if len(names) == 0 {
		return "No active MCP servers."
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// Status renders active, pending, connecting, and failed servers for users.
func (m *Manager) Status() string {
	m.mu.Lock()
	active := make(map[string]*Client, len(m.clients))
	toolCounts := make(map[string]int, len(m.toolCounts))
	for name, client := range m.clients {
		active[name] = client
		toolCounts[name] = m.toolCounts[name]
	}
	var pending []string
	for name := range m.pendingServers {
		if _, failed := m.failures[name]; !failed {
			pending = append(pending, name)
		}
	}
	var connecting []string
	for name := range m.connecting {
		connecting = append(connecting, name)
	}
	failures := make(map[string]string, len(m.failures))
	retryable := make(map[string]bool, len(m.failures))
	for name, failure := range m.failures {
		failures[name] = failure
		_, ok := m.pendingServers[name]
		retryable[name] = ok
	}
	m.mu.Unlock()

	if len(active) == 0 && len(pending) == 0 && len(connecting) == 0 && len(failures) == 0 {
		return "No MCP servers configured."
	}

	var out strings.Builder
	out.WriteString("MCP servers:")
	if len(active) > 0 {
		names := make([]string, 0, len(active))
		for name := range active {
			names = append(names, name)
		}
		sort.Strings(names)
		out.WriteString("\n  active:")
		for _, name := range names {
			count := toolCounts[name]
			label := "tools"
			if count == 1 {
				label = "tool"
			}
			fmt.Fprintf(&out, "\n    - %s (%s, %d %s)", name, active[name].Health(), count, label)
		}
	}
	if len(connecting) > 0 {
		sort.Strings(connecting)
		out.WriteString("\n  connecting:")
		for _, name := range connecting {
			fmt.Fprintf(&out, "\n    - %s", name)
		}
	}
	if len(pending) > 0 {
		sort.Strings(pending)
		out.WriteString("\n  pending:")
		for _, name := range pending {
			fmt.Fprintf(&out, "\n    - %s", name)
		}
	}
	if len(failures) > 0 {
		names := make([]string, 0, len(failures))
		for name := range failures {
			names = append(names, name)
		}
		sort.Strings(names)
		out.WriteString("\n  failed:")
		for _, name := range names {
			fmt.Fprintf(&out, "\n    - %s: %s", name, failures[name])
			if retryable[name] {
				fmt.Fprintf(&out, "\n      Retry: /mcp approve %s", name)
			}
		}
	}
	return out.String()
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

// UnregisterMCPTools removes every tool owned by one MCP server.
func (a *ToolCatalogAdapter) UnregisterMCPTools(serverName string) {
	a.catalog.UnregisterPrefix("mcp__" + serverName + "__")
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
			Timeout:     30 * time.Second,
			Handler: func(scope *tool.ToolScope, args json.RawMessage) tool.Result {
				// Must use Executor-injected scope.Context so tool Timeout /
				// Ctrl-C cancel the stdio RPC (not a detached Background ctx).
				ctx := tool.ScopeContext(scope)
				var argMap map[string]any
				_ = json.Unmarshal(args, &argMap)
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
func inferMCPEffects(name, desc string, _ map[string]any) tool.EffectSet {
	combined := strings.ToLower(name + " " + desc)
	tokens := strings.FieldsFunc(combined, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	hasToken := func(words ...string) bool {
		for _, token := range tokens {
			for _, word := range words {
				if token == word {
					return true
				}
			}
		}
		return false
	}
	effects := []tool.Effect{tool.EffectNetworkAccess} // all MCP tools are network calls

	mutating := hasToken("write", "create", "update", "delete", "insert", "remove", "set", "put", "patch", "archive", "purge", "forget")
	readOnly := hasToken("read", "query", "search", "get", "list", "fetch", "find", "describe", "inspect")
	executing := hasToken("exec", "execute", "run", "command", "shell")
	if mutating {
		effects = append(effects, tool.EffectWriteFile)
	}
	if readOnly {
		effects = append(effects, tool.EffectReadFile)
	}
	if executing {
		effects = append(effects, tool.EffectExecuteProcess)
	}
	// A read keyword is not proof that a compound remote operation is read-only.
	// Mixed or unknown semantics stay unclassified and therefore fail closed
	// until a plan is established.
	if (!mutating && !readOnly && !executing) || (readOnly && (mutating || executing)) {
		effects = append(effects, tool.EffectUnclassified)
	}
	return tool.Effects(effects...)
}
