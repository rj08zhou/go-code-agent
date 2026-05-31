package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/internal/llm"
)

// ToolResult is an alias for llm.ToolResult.
type ToolResult = llm.ToolResult

// ToolHandler is the signature for every registered tool.
type ToolHandler = func(ctx context.Context, args json.RawMessage) ToolResult

func mkOk(output string) ToolResult { return llm.MkOk(output) }
func mkErr(msg string) ToolResult   { return llm.MkErr(msg) }

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

// Base tools: path sandbox + bash / read / write / edit / delete.
// Security: see security.go for BashPolicy, securePath, ApprovalLayer, SecretsSanitizer.

func runBash(command string) string {
	// Delegate to secureRunBash with interactive=true (REPL context).
	return secureRunBash(command, true)
}

func runRead(path string, limit int) string {
	return secureReadFile(path, limit)
}

func runWrite(path, content string) string {
	return secureWriteFile(path, content)
}

func runEdit(path, oldText, newText string) string {
	return secureEditFile(path, oldText, newText)
}

// runDelete provides safe file deletion through the security layer.
func runDelete(path string) string {
	return secureDeleteFile(path)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// parseArgs unmarshals JSON tool arguments into the target struct.
func parseArgs(raw json.RawMessage, target any) string {
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Sprintf("Error: invalid arguments: %v", err)
	}
	return ""
}

// coreToolDefs returns base tool definitions shared across all agent types.
// Set writeTools=true to include write_file, edit_file, delete_file.
func coreToolDefs(writeTools bool) []llm.ToolDef {
	defs := []llm.ToolDef{
		toolDef("bash", "Run a shell command (allowlisted commands only).", map[string]any{"command": strProp()}, []string{"command"}),
		toolDef("read_file", "Read file contents.", map[string]any{"path": strProp(), "limit": intProp()}, []string{"path"}),
	}
	if writeTools {
		defs = append(defs,
			toolDef("write_file", "Write content to file.", map[string]any{"path": strProp(), "content": strProp()}, []string{"path", "content"}),
			toolDef("edit_file", "Replace exact text in file.", map[string]any{"path": strProp(), "old_text": strProp(), "new_text": strProp()}, []string{"path", "old_text", "new_text"}),
			toolDef("delete_file", "Delete a file (requires confirmation).", map[string]any{"path": strProp()}, []string{"path"}),
		)
	}
	return defs
}

// coreToolHandlers returns base tool handlers shared across all agent types.
func coreToolHandlers() map[string]ToolHandler {
	h := make(map[string]ToolHandler)

	h["bash"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			Command string `json:"command"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		return mkOk(runBash(a.Command))
	}
	h["read_file"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			Path  string `json:"path"`
			Limit int    `json:"limit"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		return mkOk(runRead(a.Path, a.Limit))
	}
	h["write_file"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		if a.Path == "" {
			return mkErr("Error: 'path' parameter is required and cannot be empty. Please provide the target file path.")
		}
		return mkOk(runWrite(a.Path, a.Content))
	}
	h["edit_file"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			Path    string `json:"path"`
			OldText string `json:"old_text"`
			NewText string `json:"new_text"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		if a.Path == "" {
			return mkErr("Error: 'path' parameter is required and cannot be empty. Please provide the target file path.")
		}
		if a.OldText == "" {
			return mkErr("Error: 'old_text' parameter is required and cannot be empty. Provide the exact text to replace.")
		}
		return mkOk(runEdit(a.Path, a.OldText, a.NewText))
	}
	h["delete_file"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			Path string `json:"path"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		return mkOk(runDelete(a.Path))
	}
	return h
}
