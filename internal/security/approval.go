// Package security holds pure, cmd/agent-agnostic security logic:
// the tool-approval state machine, bash command policy, path sandbox,
// secrets sanitizer and diff-preview helpers.
//
// It deliberately does NOT know about the concrete tool registry
// (toolDefs/toolHandlers/toolSecurityMap) or any workdir-aware I/O
// wrappers (secureReadFile, secureWriteFile, ...) - those stay in
// cmd/agent because they're tied to that package's global state
// (workdir, log.Print*, the tool registry). This package only
// provides the reusable, independently-testable primitives that
// cmd/agent composes on top of.
package security

import (
	"fmt"
	"sync"
)

// ApprovalLevel classifies how risky a tool call is, and therefore
// how it should be gated before execution.
type ApprovalLevel int

const (
	ApproveAuto    ApprovalLevel = iota // auto-approved (read-only tools)
	ApproveSafe                         // safe operations (create new files, non-destructive writes)
	ApproveDanger                       // destructive operations (delete, force-push, etc.)
	ApproveBlocked                      // explicitly forbidden
)

func (l ApprovalLevel) String() string {
	switch l {
	case ApproveAuto:
		return "auto"
	case ApproveSafe:
		return "safe"
	case ApproveDanger:
		return "danger"
	case ApproveBlocked:
		return "blocked"
	default:
		return "unknown"
	}
}

// MCPToolPrefix marks tools dynamically registered from an MCP server
// (see internal/mcp: names are "mcp__<server>__<tool>").
const MCPToolPrefix = "mcp__"

// MCPDefaultLevel is the approval level a caller should apply to any
// MCP tool that has no explicit per-tool override in its own registry.
//
// MCP servers are opted into explicitly by the user via .mcp.json, but
// callers have no static knowledge of what any individual tool
// actually does. Defaulting to ApproveSafe gives MCP tools the same UX
// as write_file/edit_file: confirm once, or bulk-approve via
// `/approve safe`, while `/approve danger`/off still gates it - instead
// of falling into an "unknown tool" branch that never approves
// regardless of settings.
var MCPDefaultLevel = ApproveSafe

// ApprovalState tracks the current session's auto-approval settings
// (toggled via the /approve REPL command). It is safe for concurrent use.
type ApprovalState struct {
	autoApproveSafe bool // auto-approve safe-level tools without prompting
	autoApproveAll  bool // auto-approve ALL tools (including danger) - use with caution!
	mu              sync.RWMutex
}

// NewApprovalState returns a fresh ApprovalState with both auto-approve
// flags off (the safe default).
func NewApprovalState() *ApprovalState {
	return &ApprovalState{}
}

// GlobalApproval is the process-wide approval state. Kept as a package
// singleton (mirroring the pre-extraction global) since this is a
// single-process, single-session CLI agent; callers needing isolation
// (e.g. tests) should use NewApprovalState instead.
var GlobalApproval = NewApprovalState()

// SetAutoApproveSafe toggles auto-approval for safe-level tools.
func (a *ApprovalState) SetAutoApproveSafe(v bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.autoApproveSafe = v
}

// SetAutoApproveAll toggles auto-approval for ALL tools (including danger).
func (a *ApprovalState) SetAutoApproveAll(v bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.autoApproveAll = v
}

// IsAutoApproveSafe returns current auto-approve-safe setting.
func (a *ApprovalState) IsAutoApproveSafe() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.autoApproveSafe
}

// IsAutoApproveAll returns current auto-approve-all setting.
func (a *ApprovalState) IsAutoApproveAll() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.autoApproveAll
}

// Decide answers "is a tool call at this Level allowed right now?"
// given the current approval state. This is the state-vs-level
// decision logic extracted from cmd/agent's old checkToolApproval: it
// only needs the resolved Level + a human description, not the tool
// name or the tool registry that resolves a name to a Level - that
// resolution stays in cmd/agent (toolSecurityMap + MCP-prefix
// fallback), which then calls Decide with the result.
func (a *ApprovalState) Decide(level ApprovalLevel, description string) (bool, string) {
	switch level {
	case ApproveAuto:
		return true, ""
	case ApproveSafe:
		if a.IsAutoApproveAll() || a.IsAutoApproveSafe() {
			return true, ""
		}
		return false, fmt.Sprintf("[safe] %s requires approval. Use /approve safe to auto-approve.", description)
	case ApproveDanger:
		if a.IsAutoApproveAll() {
			return true, ""
		}
		return false, fmt.Sprintf("[DANGER] %s requires confirmation. Use /approve danger to auto-approve (risky!).", description)
	case ApproveBlocked:
		return false, fmt.Sprintf("BLOCKED: %s is not permitted", description)
	default:
		return false, fmt.Sprintf("unknown approval level for %q", description)
	}
}
