package agent

import (
	"context"
	"encoding/json"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/security"
)

// ToolResult is an alias for llm.ToolResult.
type ToolResult = llm.ToolResult

// ToolHandler is the signature for every registered tool.
type ToolHandler = func(ctx context.Context, args json.RawMessage) ToolResult

// ToolDefs / ToolHandlers are the process-wide tool registry, populated
// once by InitTools() (see tool_registry.go) and consulted by the main
// loop (Run) plus REPL commands that need the raw defs (e.g. /mcp).
var (
	ToolDefs     []llm.ToolDef
	ToolHandlers map[string]ToolHandler
)

// toolDef builds a neutral ToolDef with a standard JSON-schema body.
func toolDef(name, desc string, props map[string]any, required []string) llm.ToolDef {
	params := map[string]any{"type": "object"}
	if len(props) > 0 {
		params["properties"] = props
	} else {
		params["properties"] = map[string]any{}
	}
	if len(required) > 0 {
		params["required"] = required
	}
	return llm.ToolDef{Name: name, Description: desc, Parameters: params}
}

func strProp() map[string]any                { return map[string]any{"type": "string"} }
func intProp() map[string]any                { return map[string]any{"type": "integer"} }
func boolProp() map[string]any               { return map[string]any{"type": "boolean"} }
func enumProp(vals ...string) map[string]any { return map[string]any{"type": "string", "enum": vals} }
func intArrayProp() map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}
}

// ----------------------------------------------------------------------------
// ToolSpec - single source of truth for a tool's schema + handler + security
// ----------------------------------------------------------------------------
//
// Previously a tool's LLM-facing definition (ToolDefs), its execution
// logic (ToolHandlers) and its approval level (ToolSecurityMap) were
// three independently-maintained call sites. It was possible - and it
// happened in practice - to register a tool's Def+Handler in
// tool_registry.go and simply forget to add the matching entry to
// security.go's ToolSecurityMap. checkToolApproval treats anything
// missing from that map as "unknown tool", which is *always* denied
// (see security.go) - so the tool would silently never be callable,
// with no compile-time or obvious runtime signal that anything was
// wrong.
//
// ToolSpec + registerToolSpec fix this by construction: a tool cannot
// be added to ToolDefs/ToolHandlers without also supplying its Level
// in the same call, and registerToolSpec is the only path that
// populates ToolSecurityMap (see security.go - the map starts empty).
type ToolSpec struct {
	Def     llm.ToolDef
	Handler ToolHandler
	Level   security.ApprovalLevel
}

// spec builds a ToolSpec from the same (name, desc, props, required)
// shape toolDef already takes, plus the approval Level and Handler.
func spec(name, desc string, props map[string]any, required []string, level security.ApprovalLevel, h ToolHandler) ToolSpec {
	return ToolSpec{Def: toolDef(name, desc, props, required), Handler: h, Level: level}
}

// registerToolSpec adds s to the three global registries atomically:
// ToolDefs (schema advertised to the LLM), ToolHandlers (execution),
// and ToolSecurityMap (approval gate). Called from InitTools().
func registerToolSpec(s ToolSpec) {
	ToolDefs = append(ToolDefs, s.Def)
	ToolHandlers[s.Def.Name] = s.Handler
	ToolSecurityMap[s.Def.Name] = ToolSecurityMeta{Name: s.Def.Name, Level: s.Level, Description: s.Def.Description}
}

// registerToolSpecs registers every spec in specs (see registerToolSpec).
func registerToolSpecs(specs ...ToolSpec) {
	for _, s := range specs {
		registerToolSpec(s)
	}
}

// specsToDefsAndHandlers projects a []ToolSpec down to the (defs,
// handlers) pair that restricted contexts need - e.g. the read-only
// subagent or a teammate's local tool set - without touching the
// global registries (those contexts never call registerToolSpec；
// their approval checks fall back to the levels the main registry
// already established via coreToolSpecs).
func specsToDefsAndHandlers(specs []ToolSpec) ([]llm.ToolDef, map[string]ToolHandler) {
	defs := make([]llm.ToolDef, 0, len(specs))
	handlers := make(map[string]ToolHandler, len(specs))
	for _, s := range specs {
		defs = append(defs, s.Def)
		handlers[s.Def.Name] = s.Handler
	}
	return defs, handlers
}

// Base tools: path sandbox + bash / read / write / edit / delete.
// Security: see security.go for BashPolicy, securePath, ApprovalLayer, SecretsSanitizer.

func runBash(ctx context.Context, command string) string {
	// Delegate to secureRunBash with interactive=true (REPL context).
	return secureRunBash(ctx, command, true)
}

func runRead(ctx context.Context, path string, limit int) string {
	return secureReadFile(ctx, path, limit)
}

func runWrite(ctx context.Context, path, content string) string {
	return secureWriteFile(ctx, path, content)
}

func runEdit(ctx context.Context, path, oldText, newText string) string {
	return secureEditFile(ctx, path, oldText, newText)
}

// runDelete provides safe file deletion through the security layer.
func runDelete(ctx context.Context, path string) string {
	return secureDeleteFile(ctx, path)
}

// coreToolSpecs returns the base tool specs shared across all agent
// types (main loop, subagent, teammate): bash/read_file always,
// write_file/edit_file/delete_file when writeTools=true. This is the
// single canonical definition of "what bash/read_file/... are" - its
// Level values are what every context's checkToolApproval call
// ultimately consults, regardless of whether that context reaches
// them via the global registry (registerToolSpecs, in InitTools) or a
// local subset (specsToDefsAndHandlers, in subagent.go/team.go).
func coreToolSpecs(writeTools bool) []ToolSpec {
	specs := []ToolSpec{
		spec("bash", "Execute a shell command (e.g., ls, pwd, go build, git status). ONLY for shell commands - use read_file to read files, NOT bash. "+
			"Runs with a hard 120s timeout, after which the command AND all its child processes are killed. "+
			"For long-running or blocking processes (dev servers like 'npm run dev', watch-mode builds, 'tail -f', etc.) use the background_run tool instead - under bash they will hit the 120s timeout and be killed. "+
			"Do NOT wrap commands in a 'timeout' shell command: it is not reliably available (absent on macOS by default) and this tool already enforces its own timeout.",
			map[string]any{"command": strProp()}, []string{"command"}, security.ApproveDanger,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					Command string `json:"command"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				return llm.MkOk(runBash(ctx, a.Command))
			}),
		spec("read_file", "Read file contents. When you only need a specific fact, symbol, constant value, or a few lines (NOT the whole file), first locate it with `bash` grep/rg (e.g. rg -n \"TokenThreshold\" .) and read only what you need - pass `limit` to cap the number of lines returned for large files. Reading entire large files just to check one value wastes context and forces compaction.",
			map[string]any{"path": strProp(), "limit": intProp()}, []string{"path"}, security.ApproveAuto,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					Path  string `json:"path"`
					Limit int    `json:"limit"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				return llm.MkOk(runRead(ctx, a.Path, a.Limit))
			}),
	}
	if !writeTools {
		return specs
	}
	return append(specs,
		spec("write_file", "Write content to file.",
			map[string]any{"path": strProp(), "content": strProp()}, []string{"path", "content"}, security.ApproveSafe,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					Path    string `json:"path"`
					Content string `json:"content"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				if a.Path == "" {
					return llm.MkErr("Error: 'path' parameter is required and cannot be empty. Please provide the target file path.")
				}
				return llm.MkOk(runWrite(ctx, a.Path, a.Content))
			}),
		spec("edit_file", "Replace exact text in file.",
			map[string]any{"path": strProp(), "old_text": strProp(), "new_text": strProp()}, []string{"path", "old_text", "new_text"}, security.ApproveSafe,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					Path    string `json:"path"`
					OldText string `json:"old_text"`
					NewText string `json:"new_text"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				if a.Path == "" {
					return llm.MkErr("Error: 'path' parameter is required and cannot be empty. Please provide the target file path.")
				}
				if a.OldText == "" {
					return llm.MkErr("Error: 'old_text' parameter is required and cannot be empty. Provide the exact text to replace.")
				}
				return llm.MkOk(runEdit(ctx, a.Path, a.OldText, a.NewText))
			}),
		spec("delete_file", "Delete a file (requires confirmation).",
			map[string]any{"path": strProp()}, []string{"path"}, security.ApproveDanger,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					Path string `json:"path"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				return llm.MkOk(runDelete(ctx, a.Path))
			}),
	)
}

// coreToolDefs / coreToolHandlers are thin projections of coreToolSpecs
// for callers (subagent.go, team.go) that only need the (defs,
// handlers) pair, not the Level (those contexts share the global
// ToolSecurityMap that InitTools populated via coreToolSpecs).
//
// writeTools must match between the Defs a caller advertises to the
// LLM and the Handlers it accepts: previously coreToolHandlers()
// always returned all 5 handlers regardless of which Defs were
// exposed, so e.g. the read-only subagent (coreToolDefs(false)) still
// had a live write_file/edit_file/delete_file handler sitting in its
// dispatch map - reachable if the model ever emitted a tool_call for a
// tool it was never told about. Threading writeTools through here too
// closes that gap.
func coreToolDefs(writeTools bool) []llm.ToolDef {
	defs, _ := specsToDefsAndHandlers(coreToolSpecs(writeTools))
	return defs
}

func coreToolHandlers(writeTools bool) map[string]ToolHandler {
	_, handlers := specsToDefsAndHandlers(coreToolSpecs(writeTools))
	return handlers
}
