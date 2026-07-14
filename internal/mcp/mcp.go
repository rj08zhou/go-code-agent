package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/logging"
	"go-code-agent/utils"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// MCPManager - manage multiple MCP Server connections via stdio
//
// Protocol: JSON-RPC 2.0 over stdin/stdout (see mcp_jsonrpc.go)
// Flow: connect -> initialize -> tools/list -> ready
// Tool naming: mcp__<server>__<tool> to avoid conflicts
//
// Resilience layer (added)
// ------------------------
//   - Per-server circuit breaker: after `mcpBreakerThreshold` consecutive
//     failures the server is marked OPEN; subsequent Call returns
//     fast (no IPC) until `mcpBreakerCooldown` elapses, after which
//     the breaker enters HALF-OPEN — the next call is a probe that
//     either CLOSES the breaker (success) or re-OPENs it (failure).
//   - Background health-check: every `mcpHealthInterval`, the manager
//     sends a `tools/list` ping to each server. Pinging while the
//     breaker is OPEN serves as the half-open probe.

const (
	mcpBreakerThreshold = 3
	mcpBreakerCooldown  = 30 * time.Second
	mcpHealthInterval   = 60 * time.Second
)

// breakerState is a tri-state circuit breaker.
type breakerState int

const (
	breakerClosed   breakerState = iota // normal
	breakerOpen                         // failing, fast-fail
	breakerHalfOpen                     // one probe allowed
)

// mcpTool represents a tool discovered from an MCP server.
type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// mcpServer represents a single connected MCP server.
type mcpServer struct {
	Name         string
	Command      string
	Args         []string
	Env          map[string]string
	Instructions string // server-provided usage guidance (from initialize response)
	conn         *jsonRPCConn
	Tools        []mcpTool

	// Breaker state. Guarded by bMu — kept separate from the
	// manager's mu so a long ping doesn't block Tool dispatch.
	bMu             sync.Mutex
	breaker         breakerState
	consecFails     int
	openedAt        time.Time
	lastHealthErr   string
	lastHealthCheck time.Time
}

// MCPManager manages multiple MCP server connections.
type MCPManager struct {
	servers map[string]*mcpServer
	mu      sync.RWMutex
	workdir string

	// Health checker control.
	healthStop chan struct{}
	healthOnce sync.Once
}

func NewMCPManager(workdir string) *MCPManager {
	return &MCPManager{
		servers:    make(map[string]*mcpServer),
		healthStop: make(chan struct{}),
		workdir:    workdir,
	}
}

// mcpConfig is the structure of .mcp.json
type mcpConfig struct {
	Servers map[string]struct {
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env"`
	} `json:"servers"`
}

// LoadConfig reads .mcp.json and connects all configured servers.
func (m *MCPManager) LoadConfig(dir string) {
	path := filepath.Join(dir, ".mcp.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var cfg mcpConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		logging.PrintSystem(fmt.Sprintf("[mcp] Warning: invalid .mcp.json: %v", err))
		return
	}
	for name, srv := range cfg.Servers {
		if err := m.Connect(name, srv.Command, srv.Args, srv.Env); err != nil {
			logging.PrintSystem(fmt.Sprintf("[mcp] Failed to connect '%s': %v", name, err))
		} else {
			logging.PrintSystem(fmt.Sprintf("[mcp] Connected '%s' (%d tools)", name, len(m.servers[name].Tools)))
		}
	}
}

// Connect starts an MCP server process, initializes, and discovers tools.
func (m *MCPManager) Connect(name, command string, args []string, env map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Close existing connection if any.
	if existing, ok := m.servers[name]; ok {
		existing.conn.close()
	}

	cmd := exec.Command(command, args...)
	cmd.Dir = m.workdir

	// Inherit current env + custom env.
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start command: %w", err)
	}

	conn := &jsonRPCConn{
		cmd:    cmd,
		stdin:  stdin,
		reader: bufio.NewReader(stdout),
	}

	srv := &mcpServer{
		Name: name, Command: command, Args: args, Env: env, conn: conn,
	}

	// Initialize the MCP session.
	if err := srv.initialize(); err != nil {
		conn.close()
		return fmt.Errorf("initialize: %w", err)
	}

	// Discover tools.
	if err := srv.discoverTools(); err != nil {
		conn.close()
		return fmt.Errorf("tools/list: %w", err)
	}

	m.servers[name] = srv
	// Spin up the health checker on first successful connect. A
	// sync.Once ensures we don't start multiple goroutines if Connect
	// is called repeatedly (e.g. via /mcp connect).
	m.healthOnce.Do(func() { go m.healthLoop() })
	return nil
}

// Disconnect closes a specific server connection.
func (m *MCPManager) Disconnect(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	srv, ok := m.servers[name]
	if !ok {
		return fmt.Sprintf("Error: server '%s' not found", name)
	}
	srv.conn.close()
	delete(m.servers, name)
	return fmt.Sprintf("Disconnected '%s'", name)
}

// DisconnectAll closes all server connections.
func (m *MCPManager) DisconnectAll() {
	// Stop the health loop first so it doesn't race with closes.
	select {
	case <-m.healthStop:
		// already closed
	default:
		close(m.healthStop)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, srv := range m.servers {
		srv.conn.close()
	}
	m.servers = make(map[string]*mcpServer)
}

// ListAll returns a summary of all connected servers and their tools.
func (m *MCPManager) ListAll() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.servers) == 0 {
		return "No MCP servers connected."
	}
	var lines []string
	for name, srv := range m.servers {
		toolNames := make([]string, len(srv.Tools))
		for i, t := range srv.Tools {
			toolNames[i] = t.Name
		}
		lines = append(lines, fmt.Sprintf("  %s (%s): %d tools [%s]",
			name, srv.Command, len(srv.Tools), strings.Join(toolNames, ", ")))
	}
	return "MCP Servers:\n" + strings.Join(lines, "\n")
}

// Call dispatches a tool call to the appropriate MCP server.
// Returns a ToolResult whose OK field is false when the MCP server
// signals an error via the isError flag.
//
// The circuit breaker short-circuits this path when the target
// server has been failing recently — keeping the agent responsive
// instead of waiting for repeated IPC timeouts.
func (m *MCPManager) Call(toolName string, argsJSON json.RawMessage) llm.ToolResult {
	parts := strings.SplitN(toolName, "__", 3)
	if len(parts) != 3 || parts[0] != "mcp" {
		return llm.MkErr(fmt.Sprintf("invalid MCP tool name: %s", toolName))
	}
	serverName, mcpToolName := parts[1], parts[2]

	m.mu.RLock()
	srv, ok := m.servers[serverName]
	m.mu.RUnlock()
	if !ok {
		return llm.MkErr(fmt.Sprintf("MCP server '%s' not connected", serverName))
	}

	// Breaker gate.
	if !srv.breakerAllow() {
		return llm.MkErr(fmt.Sprintf("MCP server '%s' circuit OPEN (last error: %s); will retry after %s",
			serverName, srv.snapshotHealthErr(), mcpBreakerCooldown))
	}

	var args map[string]any
	if len(argsJSON) > 0 {
		json.Unmarshal(argsJSON, &args)
	}

	raw, err := srv.callTool(mcpToolName, args)
	if err != nil {
		srv.breakerOnFail(err.Error())
		return llm.MkErr(fmt.Sprintf("%v", err))
	}
	if strings.HasPrefix(raw, "Error: ") {
		srv.breakerOnSuccess()
		return llm.MkErr(raw[len("Error: "):])
	}
	srv.breakerOnSuccess()
	return llm.MkOk(raw)
}

// ToolDefs returns neutral tool definitions for all MCP tools.
func (m *MCPManager) ToolDefs() []llm.ToolDef {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var defs []llm.ToolDef
	for serverName, srv := range m.servers {
		for _, t := range srv.Tools {
			qualifiedName := fmt.Sprintf("mcp__%s__%s", serverName, t.Name)
			desc := t.Description
			if desc == "" {
				desc = fmt.Sprintf("MCP tool from %s", serverName)
			}

			params := map[string]any{"type": "object"}
			if t.InputSchema != nil {
				if props, ok := t.InputSchema["properties"]; ok {
					params["properties"] = props
				}
				if req, ok := t.InputSchema["required"]; ok {
					params["required"] = req
				}
			}
			if _, ok := params["properties"]; !ok {
				params["properties"] = map[string]any{}
			}

			defs = append(defs, llm.ToolDef{
				Name:        qualifiedName,
				Description: desc,
				Parameters:  params,
			})
		}
	}
	return defs
}

// IsMCPTool returns true if the tool name is an MCP tool.
func (m *MCPManager) IsMCPTool(name string) bool {
	return strings.HasPrefix(name, "mcp__")
}

// ServerInstructions returns a formatted string of all connected servers'
// instructions for injection into the system prompt.
func (m *MCPManager) ServerInstructions() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var parts []string
	for name, srv := range m.servers {
		if srv.Instructions == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("### %s\n%s", name, srv.Instructions))
	}
	return strings.Join(parts, "\n\n")
}

// ServerCount returns the number of connected servers.
func (m *MCPManager) ServerCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.servers)
}

// ToolCount returns total number of MCP tools across all servers.
func (m *MCPManager) ToolCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, srv := range m.servers {
		n += len(srv.Tools)
	}
	return n
}

// mcpServer - MCP protocol methods (uses jsonRPCConn)

func (s *mcpServer) initialize() error {
	params := map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "go-code-agent",
			"version": "1.0.0",
		},
	}
	result, err := s.conn.sendRequest("initialize", params)
	if err != nil {
		return err
	}
	var initResult struct {
		Instructions string `json:"instructions"`
		ServerInfo   struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(result, &initResult); err == nil {
		s.Instructions = strings.TrimSpace(initResult.Instructions)
	}
	s.conn.sendNotification("notifications/initialized")
	return nil
}

func (s *mcpServer) discoverTools() error {
	result, err := s.conn.sendRequest("tools/list", map[string]any{})
	if err != nil {
		return err
	}
	var toolsResult struct {
		Tools []mcpTool `json:"tools"`
	}
	if err := json.Unmarshal(result, &toolsResult); err != nil {
		return fmt.Errorf("parse tools: %w", err)
	}
	s.Tools = toolsResult.Tools
	return nil
}

func (s *mcpServer) callTool(name string, args map[string]any) (string, error) {
	params := map[string]any{
		"name":      name,
		"arguments": args,
	}
	result, err := s.conn.sendRequest("tools/call", params)
	if err != nil {
		return "", err
	}

	var callResult struct {
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Data     string          `json:"data"`     // image base64
			MimeType string          `json:"mimeType"` // image mime
			Resource json.RawMessage `json:"resource"` // resource object
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result, &callResult); err != nil {
		return string(result), nil
	}

	var parts []string
	for _, c := range callResult.Content {
		switch c.Type {
		case "text":
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		case "image":
			label := "[image"
			if c.MimeType != "" {
				label += " " + c.MimeType
			}
			label += fmt.Sprintf(", %d bytes]", len(c.Data))
			parts = append(parts, label)
		case "resource":
			if len(c.Resource) > 0 {
				parts = append(parts, "[resource: "+string(c.Resource)+"]")
			} else {
				parts = append(parts, "[resource]")
			}
		default:
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
	}
	output := strings.Join(parts, "\n")
	if callResult.IsError {
		output = "Error: " + output
	}
	if output == "" {
		output = "(no output)"
	}
	return utils.Truncate(output, infra.MaxOutputLen), nil
}

// breakerAllow returns true if a call may proceed. Side effect: in
// HALF-OPEN state it returns true exactly once per cooldown window
// (the probe), then waits for the call's outcome to decide whether
// to close or re-open.
func (s *mcpServer) breakerAllow() bool {
	s.bMu.Lock()
	defer s.bMu.Unlock()
	switch s.breaker {
	case breakerClosed:
		return true
	case breakerOpen:
		if time.Since(s.openedAt) >= mcpBreakerCooldown {
			s.breaker = breakerHalfOpen
			return true
		}
		return false
	case breakerHalfOpen:
		// Already a probe in flight; reject parallel callers to keep
		// the half-open invariant of "one probe at a time".
		return false
	}
	return true
}

// breakerOnSuccess records a healthy outcome. Closes the breaker
// from HALF-OPEN and resets the failure counter from CLOSED.
func (s *mcpServer) breakerOnSuccess() {
	s.bMu.Lock()
	defer s.bMu.Unlock()
	s.consecFails = 0
	s.lastHealthErr = ""
	s.breaker = breakerClosed
}

// breakerOnFail records a failed outcome. Trips the breaker once
// the threshold is hit (or immediately if we were probing).
func (s *mcpServer) breakerOnFail(reason string) {
	s.bMu.Lock()
	defer s.bMu.Unlock()
	s.consecFails++
	s.lastHealthErr = reason
	if s.breaker == breakerHalfOpen || s.consecFails >= mcpBreakerThreshold {
		s.breaker = breakerOpen
		s.openedAt = time.Now()
	}
}

func (s *mcpServer) snapshotHealthErr() string {
	s.bMu.Lock()
	defer s.bMu.Unlock()
	if s.lastHealthErr == "" {
		return "unknown"
	}
	return utils.Truncate(s.lastHealthErr, 120)
}

// healthLoop periodically pings every connected server with a
// `tools/list` request. Servers in OPEN state are also pinged once
// the cooldown passes — the ping doubles as the half-open probe.
//
// We do NOT respawn a crashed server here (that would race with
// in-flight Calls and stdio buffers). The loop only updates breaker
// state; explicit reconnect via /mcp connect is the recovery path.
func (m *MCPManager) healthLoop() {
	ticker := time.NewTicker(mcpHealthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.healthStop:
			return
		case <-ticker.C:
			m.pingAll()
		}
	}
}

func (m *MCPManager) pingAll() {
	m.mu.RLock()
	servers := make([]*mcpServer, 0, len(m.servers))
	for _, s := range m.servers {
		servers = append(servers, s)
	}
	m.mu.RUnlock()

	for _, s := range servers {
		if !s.breakerAllow() {
			// Still in cooldown. Skip this round.
			continue
		}
		_, err := s.conn.sendRequest("tools/list", map[string]any{})
		s.bMu.Lock()
		s.lastHealthCheck = time.Now()
		s.bMu.Unlock()
		if err != nil {
			s.breakerOnFail("health: " + err.Error())
			logging.PrintSystem(fmt.Sprintf("[mcp] health-check failed for '%s': %v", s.Name, err))
		} else {
			s.breakerOnSuccess()
		}
	}
}
