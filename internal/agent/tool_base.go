package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/security"
	"go-code-agent/utils"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ToolResult is an alias for llm.ToolResult.
type ToolResult = llm.ToolResult

// ToolHandler is the signature for every registered tool.
type ToolHandler = func(ctx context.Context, args json.RawMessage) ToolResult

// ToolDefs / ToolHandlers are the process-wide tool registry, populated
// once by InitTools() (see tool_registry.go).
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

// ToolSpec bundles a tool's Def, Handler, and approval Level so they
// are registered atomically via registerToolSpec.
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

func runRead(ctx context.Context, path string, offset, limit int) string {
	return secureReadFile(ctx, path, offset, limit)
}

func runWrite(ctx context.Context, path, content string) string {
	return secureWriteFile(ctx, path, content)
}

func runEdit(ctx context.Context, path, oldText, newText string, replaceAll bool) string {
	return secureEditFile(ctx, path, oldText, newText, replaceAll)
}

// runInsert inserts newText into a file after a 1-based line number
// (afterLine==0 prepends, afterLine>=line count appends). Delegates to
// the security layer for approval, path safety, diff preview, and the
// atomic write.
func runInsert(ctx context.Context, path string, afterLine int, newText string) string {
	return secureInsertFile(ctx, path, afterLine, newText)
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
		spec("read_file", "Read file contents. When you only need a specific fact, symbol, constant value, or a few lines (NOT the whole file), first locate it with `bash` grep/rg (e.g. rg -n \"TokenThreshold\" .) and read only what you need - pass `offset` + `limit` to narrow the range. Reading entire large files just to check one value wastes context and forces compaction.",
			map[string]any{"path": strProp(), "offset": intProp(), "limit": intProp()}, []string{"path"}, security.ApproveAuto,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					Path   string `json:"path"`
					Offset int    `json:"offset"`
					Limit  int    `json:"limit"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				return llm.MkOk(runRead(ctx, a.Path, a.Offset, a.Limit))
			}),
		spec("list_dir", "List directory contents (type, size, name). Defaults to workdir.",
			map[string]any{"path": strProp()}, nil, security.ApproveAuto,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					Path string `json:"path"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				dir := workdirFromCtx(ctx)
				if a.Path != "" {
					resolved, err := security.SecurePath(dir, a.Path, false)
					if err != nil {
						return llm.MkErr(fmt.Sprintf("%v", err))
					}
					dir = resolved
				}
				entries, err := os.ReadDir(dir)
				if err != nil {
					return llm.MkErr(fmt.Sprintf("%v", err))
				}
				var lines []string
				for _, e := range entries {
					info, _ := e.Info()
					marker := "  "
					if e.IsDir() {
						marker = "d "
					}
					lines = append(lines, fmt.Sprintf("%s%8d  %s", marker, info.Size(), e.Name()))
				}
				return llm.MkOk(strings.Join(lines, "\n"))
			}),
		spec("search_file", "Find files by name pattern (glob, e.g. '*.go'). Searches recursively, skipping .git/node_modules.",
			map[string]any{
				"pattern": strProp(),
				"path":    strProp(),
			}, []string{"pattern"}, security.ApproveAuto,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					Pattern string `json:"pattern"`
					Path    string `json:"path"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				root := workdirFromCtx(ctx)
				if a.Path != "" {
					resolved, err := security.SecurePath(root, a.Path, false)
					if err != nil {
						return llm.MkErr(fmt.Sprintf("%v", err))
					}
					root = resolved
				}
				var matches []string
				skipDirs := map[string]bool{".git": true, "node_modules": true, ".go-code-agent": true}
				filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
					if err != nil {
						return nil
					}
					if d.IsDir() {
						if skipDirs[d.Name()] {
							return filepath.SkipDir
						}
						return nil
					}
					if ok, _ := filepath.Match(a.Pattern, d.Name()); ok {
						rel, _ := filepath.Rel(workdirFromCtx(ctx), p)
						matches = append(matches, rel)
					}
					return nil
				})
				if len(matches) == 0 {
					return llm.MkOk("No files matched.")
				}
				return llm.MkOk(utils.Truncate(strings.Join(matches, "\n"), infra.MaxOutputLen))
			}),
		spec("search_content", "Search file contents by regex (like grep -rn). Returns file:line:content for each match.",
			map[string]any{
				"pattern": strProp(),
				"path":    strProp(),
			}, []string{"pattern"}, security.ApproveAuto,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					Pattern string `json:"pattern"`
					Path    string `json:"path"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				if a.Pattern == "" {
					return llm.MkErr("pattern is required")
				}
				root := workdirFromCtx(ctx)
				if a.Path != "" {
					resolved, err := security.SecurePath(root, a.Path, false)
					if err != nil {
						return llm.MkErr(fmt.Sprintf("%v", err))
					}
					root = resolved
				}
				searchCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				cmd := exec.CommandContext(searchCtx, "grep",
					"-rnI",
					"--exclude-dir=.git",
					"--exclude-dir=node_modules",
					"--exclude-dir=.go-code-agent",
					"-E", a.Pattern, root,
				)
				out, err := cmd.CombinedOutput()
				if searchCtx.Err() == context.DeadlineExceeded {
					return llm.MkErr("search timed out after 30s")
				}
				result := strings.TrimSpace(string(out))
				if err != nil && result == "" {
					return llm.MkOk("No matches found.")
				}
				return llm.MkOk(utils.Truncate(result, infra.MaxOutputLen))
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
		spec("edit_file", "Replace exact text in file. By default replaces only the first occurrence; set replace_all=true to replace every occurrence. When there are multiple non-unique matches and replace_all is false, only the first is changed and the rest are left untouched.",
			map[string]any{"path": strProp(), "old_text": strProp(), "new_text": strProp(), "replace_all": boolProp()}, []string{"path", "old_text", "new_text"}, security.ApproveSafe,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					Path       string `json:"path"`
					OldText    string `json:"old_text"`
					NewText    string `json:"new_text"`
					ReplaceAll bool   `json:"replace_all"`
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
				return llm.MkOk(runEdit(ctx, a.Path, a.OldText, a.NewText, a.ReplaceAll))
			}),
		spec("insert_file", "Insert text at a specific line in a file. after_line is 1-based: 0 prepends before the first line, N inserts after line N, and values >= line count append to the end. Newlines are added between the existing content and the inserted text automatically.",
			map[string]any{"path": strProp(), "after_line": intProp(), "content": strProp()}, []string{"path", "after_line", "content"}, security.ApproveSafe,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					Path      string `json:"path"`
					AfterLine int    `json:"after_line"`
					Content   string `json:"content"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				if a.Path == "" {
					return llm.MkErr("Error: 'path' parameter is required and cannot be empty. Please provide the target file path.")
				}
				return llm.MkOk(runInsert(ctx, a.Path, a.AfterLine, a.Content))
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
