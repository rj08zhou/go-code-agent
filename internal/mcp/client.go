package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// Client manages a single MCP server subprocess over stdio JSON-RPC,
// with circuit breaker and health monitoring.
type Client struct {
	config  ServerConfig
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	stderr  io.ReadCloser
	reader  *bufio.Reader
	mu      sync.Mutex
	rpcMu   sync.Mutex
	running bool
	reqID   int

	// Circuit breaker
	consecFails  int
	maxFails     int
	breakerState string // closed, open, half-open
	instructions string // server instructions from initialize response
	lastFailTime time.Time

	// Health
	lastPing  time.Time
	healthCtx context.CancelFunc
}

const (
	breakerClosed   = "closed"
	breakerOpen     = "open"
	breakerHalfOpen = "half-open"

	defaultMaxFails    = 3
	healthPingInterval = 30 * time.Second
	breakerCooldown    = 30 * time.Second
)

// ServerConfig describes how to launch an MCP server.
type ServerConfig struct {
	Name    string
	Command string
	Args    []string
	Env     []string
}

// ToolInfo describes an MCP tool.
type ToolInfo struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Schema      map[string]any `json:"inputSchema"`
}

// NewClient creates an MCP client. The server is not started yet.
func NewClient(cfg ServerConfig) *Client {
	return &Client{
		config:       cfg,
		breakerState: breakerClosed,
		maxFails:     defaultMaxFails,
	}
}

// Start launches the MCP server subprocess and initializes the JSON-RPC session.
func (c *Client) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
		return nil
	}

	c.cmd = exec.CommandContext(ctx, c.config.Command, c.config.Args...)
	c.cmd.Env = c.config.Env

	var err error
	c.stdin, err = c.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	c.stdout, err = c.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	c.stderr, err = c.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := c.cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", c.config.Name, err)
	}

	c.reader = bufio.NewReader(c.stdout)
	c.running = true

	// Handshake: initialize
	if err := c.initialize(ctx); err != nil {
		c.abortRPC()
		return err
	}
	c.lastPing = time.Now()
	return nil
}

// Stop terminates the MCP server and health loop.
func (c *Client) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running {
		return nil
	}
	c.running = false
	if c.healthCtx != nil {
		c.healthCtx()
	}
	if c.stdin != nil {
		c.stdin.Close()
	}
	if c.stdout != nil {
		c.stdout.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		c.cmd.Process.Kill()
	}
	return nil
}

// Health reports whether the MCP server is healthy.
func (c *Client) Health() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running {
		return "stopped"
	}
	since := time.Since(c.lastPing)
	if since > healthPingInterval*2 {
		return "unreachable"
	}
	if c.breakerState != breakerClosed {
		return c.breakerState
	}
	return "healthy"
}

// recordFailure tracks a failed operation for the circuit breaker.
func (c *Client) recordFailure() {
	c.consecFails++
	c.lastFailTime = time.Now()
	if c.consecFails >= c.maxFails {
		c.breakerState = breakerOpen
	}
}

// recordSuccess resets the circuit breaker on a successful call.
func (c *Client) recordSuccess() {
	c.consecFails = 0
	if c.breakerState == breakerHalfOpen {
		c.breakerState = breakerClosed
	}
}

// checkBreaker returns true if the circuit allows a call.
func (c *Client) checkBreaker() bool {
	if c.breakerState == breakerClosed {
		return true
	}
	if c.breakerState == breakerOpen {
		if time.Since(c.lastFailTime) > breakerCooldown {
			c.breakerState = breakerHalfOpen
			return true
		}
		return false
	}
	return true // half-open: allow one probe
}

// BreakerState returns the current circuit breaker state.
func (c *Client) BreakerState() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.breakerState
}

// startHealthLoop begins periodic health pings. Must be called after Start.
func (c *Client) startHealthLoop(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		cancel()
		return
	}
	c.healthCtx = cancel
	c.mu.Unlock()
	go func() {
		ticker := time.NewTicker(healthPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.mu.Lock()
				if !c.running {
					c.mu.Unlock()
					return
				}
				_, err := c.listToolsRaw(ctx)
				if err != nil {
					c.recordFailure()
				} else {
					c.recordSuccess()
				}
				c.lastPing = time.Now()
				c.mu.Unlock()
			}
		}
	}()
}

// DiscoverTools sends tools/list and returns the available MCP tools.
func (c *Client) DiscoverTools(ctx context.Context) ([]ToolInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running {
		return nil, fmt.Errorf("MCP server %s is not running", c.config.Name)
	}
	tools, err := c.listTools(ctx)
	if err != nil {
		c.recordFailure()
	} else {
		c.recordSuccess()
	}
	return tools, err
}

// CallTool invokes an MCP tool and returns the result.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running {
		return "", fmt.Errorf("MCP server %s is not running", c.config.Name)
	}
	if !c.checkBreaker() {
		return "", fmt.Errorf("MCP %s circuit breaker open", c.config.Name)
	}
	result, err := c.callTool(ctx, name, args)
	if err != nil {
		c.recordFailure()
	} else {
		c.recordSuccess()
	}
	return result, err
}

// listToolsRaw performs a health-check tools/list call. The caller must hold c.mu.
func (c *Client) listToolsRaw(ctx context.Context) ([]ToolInfo, error) {
	result, err := c.sendRequest(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	var wrapper struct {
		Tools []ToolInfo `json:"tools"`
	}
	if err := json.Unmarshal(result, &wrapper); err != nil {
		return nil, fmt.Errorf("parse tools: %w", err)
	}
	return wrapper.Tools, nil
}

// --- JSON-RPC internals ---

type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

// sendRequest performs one serialized stdio JSON-RPC exchange.
// The caller must hold c.mu. On cancellation the subprocess and pipes are
// closed to unblock any in-flight read/write; the client is then stopped.
func (c *Client) sendRequest(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	done := make(chan rpcResult, 1)
	go func() {
		result, err := c.sendRequestSync(method, params)
		done <- rpcResult{result: result, err: err}
	}()

	select {
	case res := <-done:
		return res.result, res.err
	case <-ctx.Done():
		c.abortRPC()
		// Wait for the serialized exchange to observe the closed pipe so no
		// reader remains behind to consume a future response.
		<-done
		return nil, ctx.Err()
	}
}

func (c *Client) sendRequestSync(method string, params interface{}) (json.RawMessage, error) {
	c.rpcMu.Lock()
	defer c.rpcMu.Unlock()
	c.reqID++
	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      c.reqID,
		Method:  method,
		Params:  params,
	}
	data, _ := json.Marshal(req)
	data = append(data, '\n')

	if _, err := c.stdin.Write(data); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	line, err := c.reader.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}

	var resp rpcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("RPC error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return resp.Result, nil
}

// abortRPC stops the client after a cancelled stdio exchange.
// The caller must hold c.mu.
func (c *Client) abortRPC() {
	c.running = false
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.stdout != nil {
		_ = c.stdout.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}

func (c *Client) initialize(ctx context.Context) error {
	result, err := c.sendRequest(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "go-code-agent",
			"version": "1.0.0",
		},
	})
	if err != nil {
		return fmt.Errorf("init: %w", err)
	}
	// Extract server instructions
	var initResp struct {
		Instructions string `json:"instructions"`
	}
	if json.Unmarshal(result, &initResp) == nil {
		c.instructions = initResp.Instructions
	}

	// Send initialized notification
	c.reqID++
	notif := rpcRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	}
	data, _ := json.Marshal(notif)
	data = append(data, '\n')
	c.stdin.Write(data)

	time.Sleep(100 * time.Millisecond)
	return nil
}

func (c *Client) listTools(ctx context.Context) ([]ToolInfo, error) {
	result, err := c.sendRequest(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}

	var wrapper struct {
		Tools []ToolInfo `json:"tools"`
	}
	if err := json.Unmarshal(result, &wrapper); err != nil {
		return nil, fmt.Errorf("parse tools: %w", err)
	}
	return wrapper.Tools, nil
}

func (c *Client) callTool(ctx context.Context, name string, args map[string]any) (string, error) {
	result, err := c.sendRequest(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return "", err
	}
	var wrapper struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result, &wrapper); err != nil {
		return string(result), nil
	}

	var texts []string
	for _, c := range wrapper.Content {
		if c.Type == "text" {
			texts = append(texts, c.Text)
		}
	}
	out := fmt.Sprintf("%v", texts)
	if wrapper.IsError {
		return fmt.Sprintf("[MCP Error] %v", texts), nil
	}
	return out, nil
}
