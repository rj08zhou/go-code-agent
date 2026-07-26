// Package tool defines the unified tool architecture:
// ToolDefinition, ToolScope, ToolResult, ToolCatalog, and ToolExecutor.
package tool

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"go-code-agent/internal/llm"
)

// Status is the machine-readable result of one tool invocation.
type Status string

const (
	StatusSucceeded   Status = "succeeded"
	StatusFailed      Status = "failed"
	StatusDenied      Status = "denied"
	StatusRejected    Status = "rejected"
	StatusModified    Status = "modified"
	StatusTimeout     Status = "timeout"
	StatusCancelled   Status = "cancelled"
	StatusInvalidArgs Status = "invalid_arguments"
	StatusUnavailable Status = "unavailable"
)

// Result is the structured output of a tool execution pipeline.
// Handlers MUST return this; string-based status expression is forbidden.
type Result struct {
	Status   Status        `json:"status"`
	Output   string        `json:"output"`
	Reason   string        `json:"reason,omitempty"`
	Feedback string        `json:"feedback,omitempty"`
	Duration time.Duration `json:"duration"`
}

func Succeeded(output string) Result {
	return Result{Status: StatusSucceeded, Output: output}
}

func Failed(message string) Result {
	return Result{Status: StatusFailed, Output: "[ERROR] " + message, Reason: message}
}

func Denied(reason string) Result {
	return Result{Status: StatusDenied, Output: "[SECURITY] " + reason, Reason: reason}
}

func Rejected(reason string) Result {
	return Result{Status: StatusRejected, Output: "[HITL-REJECTED] " + reason, Reason: reason}
}

func Modified(feedback string) Result {
	return Result{Status: StatusModified, Output: "[HITL-MODIFIED] " + feedback, Feedback: feedback}
}

func Timeout(toolName string, d time.Duration) Result {
	return Result{Status: StatusTimeout, Output: "[TIMEOUT] tool '" + toolName + "' exceeded " + d.String()}
}

func Cancelled(err error) Result {
	return Result{Status: StatusCancelled, Output: "[CANCELLED] " + err.Error()}
}

func InvalidArgs(msg string) Result {
	return Result{Status: StatusInvalidArgs, Output: "[SKIPPED] " + msg}
}

func Unavailable(msg string) Result {
	return Result{Status: StatusUnavailable, Output: "[ERROR] " + msg}
}

func (r Result) Succeeded() bool {
	return r.Status == StatusSucceeded
}

func (r Result) ToToolMessage() string {
	if r.Output != "" {
		return r.Output
	}
	return "[" + string(r.Status) + "]"
}

// ---------- Tool Definition ----------

// RiskLevel classifies a tool's inherent risk.
type RiskLevel int

const (
	RiskAuto        RiskLevel = iota // no risk (read-only, introspection)
	RiskSafe                         // safe writes, user-visible mutations
	RiskInteractive                  // requires user confirmation
	RiskDanger                       // potentially destructive
)

// Effect describes what kind of side-effect a tool has.
type Effect int

const (
	EffectReadFile Effect = 1 << iota
	EffectWriteFile
	EffectDeleteFile
	EffectExecuteProcess
	EffectNetworkAccess
	EffectSessionMutation
	EffectMemoryMutation
	EffectTeamMutation
)

// SnapshotPolicy controls git snapshot creation.
type SnapshotPolicy int

const (
	SnapshotNone          SnapshotPolicy = iota
	SnapshotBefore                       // stash before tool runs
	SnapshotTransactional                // stash before, rollback on failure
)

// ToolDefinition is a complete, self-contained tool spec.
type ToolDefinition struct {
	Name           string
	Description    string
	Schema         json.RawMessage
	Handler        ToolHandler
	Preview        ToolPreview
	RiskLevel      RiskLevel
	Effects        EffectSet
	Timeout        time.Duration
	SnapshotPolicy SnapshotPolicy
}

// PreviewRequest describes a proposed filesystem mutation before it is applied.
type PreviewRequest struct {
	Path    string
	Content []byte
	Delete  bool
}

// ToolPreview computes a mutation preview without changing the filesystem.
type ToolPreview func(scope *ToolScope, args json.RawMessage) (PreviewRequest, error)

// HasEffect checks if this tool definition includes the given effect.
func (td ToolDefinition) HasEffect(e Effect) bool {
	return td.Effects.Has(e)
}

type EffectSet struct {
	bitmask int
}

func Effects(es ...Effect) EffectSet {
	var s EffectSet
	for _, e := range es {
		s.bitmask |= int(e)
	}
	return s
}

func (es EffectSet) Has(e Effect) bool {
	return es.bitmask&int(e) != 0
}

// ToolHandler is the unified signature: receives ToolScope, returns structured Result.
type ToolHandler func(scope *ToolScope, args json.RawMessage) Result

// ToolScope carries the per-invocation execution context.
// Handlers read from this instead of reaching for global App.
type ToolScope struct {
	Context      context.Context
	ProjectID    string
	SessionID    string
	AgentID      string
	Role         string // "lead", "explore", "teammate"
	Workdir      string
	AllowedRoots []string

	// Capability gates
	CanRead    bool
	CanWrite   bool
	CanExecute bool
	CanNetwork bool
	CanTeam    bool
	CanMemory  bool

	// Approval & network policies set by the runtime
	ApprovalPolicy ApprovalChecker
	NetworkPolicy  NetworkChecker
	DiffPreview    DiffPreview

	// Audit context for logging
	AuditID string
}

// ApprovalChecker is the interface for tool approval decisions.
type ApprovalChecker interface {
	AllowTool(toolName string, args json.RawMessage) (allowed bool, reason string)
}

// NetworkChecker validates outbound network requests.
type NetworkChecker interface {
	AllowHost(host string) bool
	AllowURL(rawURL string) bool
}

// ---------- Tool Catalog ----------

// ToolCatalogSnapshot is an immutable snapshot of all registered tools.
type ToolCatalogSnapshot struct {
	Version     int
	Definitions map[string]ToolDefinition
	Handlers    map[string]ToolHandler
	// Order preserves registration order so LLM-facing tool lists are
	// deterministic across calls (Go map iteration order is randomized,
	// which would otherwise defeat OpenAI/Anthropic prompt-prefix caching
	// since tool schemas sit at the very front of the request).
	Order []string
}

// ToolCatalog manages the tool registry with atomic snapshot replacement.
// Uses RWMutex to guarantee thread-safe reads without data races while allowing
// concurrent readers and atomic writer replacement.
type ToolCatalog struct {
	mu       sync.RWMutex
	snapshot *ToolCatalogSnapshot
}

func NewToolCatalog() *ToolCatalog {
	c := &ToolCatalog{}
	c.snapshot = &ToolCatalogSnapshot{
		Version:     0,
		Definitions: make(map[string]ToolDefinition),
		Handlers:    make(map[string]ToolHandler),
	}
	return c
}

func (c *ToolCatalog) Load() *ToolCatalogSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snapshot
}

// RegisterAll atomically replaces the entire catalog with a new snapshot.
// This eliminates the data race from incremental mutation. The order of
// defs is preserved in snapshot.Order so tool lists sent to the LLM are
// stable across calls (see ToolCatalogSnapshot.Order).
func (c *ToolCatalog) RegisterAll(defs []ToolDefinition) {
	c.mu.Lock()
	defer c.mu.Unlock()

	newSnap := &ToolCatalogSnapshot{
		Version:     c.snapshot.Version + 1,
		Definitions: make(map[string]ToolDefinition, len(defs)),
		Handlers:    make(map[string]ToolHandler, len(defs)),
		Order:       make([]string, 0, len(defs)),
	}
	seen := make(map[string]bool, len(defs))
	for _, d := range defs {
		newSnap.Definitions[d.Name] = d
		newSnap.Handlers[d.Name] = d.Handler
		if !seen[d.Name] {
			newSnap.Order = append(newSnap.Order, d.Name)
			seen[d.Name] = true
		}
	}
	c.snapshot = newSnap
}

// Register additively merges defs into the current snapshot without
// disturbing already-registered tools (unlike RegisterAll, which replaces
// the entire snapshot). Existing entries whose names collide with defs are
// updated in place, preserving their original position in Order; genuinely
// new tools are appended at the end. Used for MCP tools discovered after
// startup (LoadAndStart, /mcp approve, /mcp connect) so they don't wipe out
// the builtin tool set — and so tool order stays stable across calls
// within a session, which OpenAI/Anthropic prompt-prefix caching depends on.
func (c *ToolCatalog) Register(defs []ToolDefinition) {
	c.mu.Lock()
	defer c.mu.Unlock()

	old := c.snapshot
	newSnap := &ToolCatalogSnapshot{
		Version:     old.Version + 1,
		Definitions: make(map[string]ToolDefinition, len(old.Definitions)+len(defs)),
		Handlers:    make(map[string]ToolHandler, len(old.Handlers)+len(defs)),
		Order:       append([]string(nil), old.orderedNames()...),
	}
	for name, d := range old.Definitions {
		newSnap.Definitions[name] = d
		newSnap.Handlers[name] = old.Handlers[name]
	}
	existing := make(map[string]bool, len(newSnap.Order))
	for _, name := range newSnap.Order {
		existing[name] = true
	}
	for _, d := range defs {
		newSnap.Definitions[d.Name] = d
		newSnap.Handlers[d.Name] = d.Handler
		if !existing[d.Name] {
			newSnap.Order = append(newSnap.Order, d.Name)
			existing[d.Name] = true
		}
	}
	c.snapshot = newSnap
}

// Subset returns a new catalog containing only the named tools that exist
// in c, preserving their relative registration order. Unknown names are
// skipped. Used to build read-oriented catalogs for explore subagents.
func (c *ToolCatalog) Subset(names ...string) *ToolCatalog {
	snap := c.Load()
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	defs := make([]ToolDefinition, 0, len(names))
	for _, name := range snap.orderedNames() {
		if !want[name] {
			continue
		}
		d, ok := snap.Definitions[name]
		if !ok {
			continue
		}
		if d.Handler == nil {
			d.Handler = snap.Handlers[name]
		}
		defs = append(defs, d)
	}
	out := NewToolCatalog()
	out.RegisterAll(defs)
	return out
}

func (c *ToolCatalog) Resolve(name string) (ToolHandler, bool) {
	snap := c.Load()
	h, ok := snap.Handlers[name]
	return h, ok
}

// IsKnown reports whether a tool name exists in the current snapshot.
func (c *ToolCatalog) IsKnown(name string) bool {
	_, ok := c.Load().Handlers[name]
	return ok
}

// orderedNames returns snap.Order if populated, else a sorted fallback.
// Sorting (rather than raw map iteration) keeps output deterministic
// across calls even for snapshots built without RegisterAll.
func (snap *ToolCatalogSnapshot) orderedNames() []string {
	if len(snap.Order) == len(snap.Definitions) {
		return snap.Order
	}
	names := make([]string, 0, len(snap.Definitions))
	for name := range snap.Definitions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ToolDefs returns the LLM-facing tool schemas from the current snapshot,
// in stable registration order (see ToolCatalogSnapshot.Order).
func (c *ToolCatalog) ToolDefs() []map[string]any {
	snap := c.Load()
	names := snap.orderedNames()
	defs := make([]map[string]any, 0, len(names))
	for _, name := range names {
		d := snap.Definitions[name]
		params := json.RawMessage{}
		if len(d.Schema) > 0 {
			params = d.Schema
		}
		defs = append(defs, map[string]any{
			"name":        d.Name,
			"description": d.Description,
			"parameters":  json.RawMessage(params),
		})
	}
	return defs
}

// LLMToolDefs returns tool definitions in the format expected by the LLM
// gateway, in stable registration order (see ToolCatalogSnapshot.Order).
// A stable, repeatable order is required for OpenAI/Anthropic prompt-prefix
// caching to work: tool schemas sit at the very front of every request, so
// if their order changes between calls the entire cached prefix is voided.
func (c *ToolCatalog) LLMToolDefs() []llm.ToolDef {
	snap := c.Load()
	names := snap.orderedNames()
	defs := make([]llm.ToolDef, 0, len(names))
	for _, name := range names {
		d := snap.Definitions[name]
		params := make(map[string]any)
		if len(d.Schema) > 0 {
			_ = json.Unmarshal(d.Schema, &params)
		}
		if _, ok := params["type"]; !ok {
			params["type"] = "object"
		}
		if _, ok := params["properties"]; !ok {
			params["properties"] = map[string]any{}
		}
		defs = append(defs, llm.ToolDef{
			Name:        d.Name,
			Description: d.Description,
			Parameters:  params,
		})
	}
	return defs
}

// MustMarshalJSON serializes v to json.RawMessage, panicking on error.
// Use only for static schema data that is known-valid at compile time.
func MustMarshalJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic("tool.MustMarshalJSON: " + err.Error())
	}
	return data
}
