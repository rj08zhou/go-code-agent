package tool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go-code-agent/internal/security"
	"go-code-agent/internal/store"
)

func filesystemWriteTools(d builtinDeps) []ToolDefinition {
	var defs []ToolDefinition

	defs = append(defs, ToolDefinition{
		Name:        "write_file",
		Description: "Write content to file.",
		RiskLevel:   RiskDanger,
		Effects:     Effects(EffectWriteFile),
		Preview:     previewWriteFile,
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"path", "content"},
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "Absolute or relative file path."},
				"content": map[string]any{"type": "string", "description": "Content to write to the file."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			if a.Path == "" {
				return Failed("path is required")
			}
			fp, err := security.SecurePath(scope.Workdir, a.Path, true)
			if err != nil {
				return Failed(fmt.Sprintf("%v", err))
			}
			if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
				return Failed(fmt.Sprintf("%v", err))
			}
			previewText := ""
			if scope.DiffPreview != nil {
				if preview, previewErr := scope.DiffPreview.Preview(a.Path, []byte(a.Content)); previewErr == nil && preview != "(no changes)" {
					previewText = "\nDiff preview:\n" + preview
				}
			}
			if err := store.AtomicWrite(fp, []byte(a.Content)); err != nil {
				return Failed(fmt.Sprintf("%v", err))
			}
			return Succeeded(fmt.Sprintf("Wrote %d bytes to %s%s", len(a.Content), a.Path, previewText))
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "edit_file",
		Description: "Replace exact text in file.",
		RiskLevel:   RiskDanger,
		Effects:     Effects(EffectWriteFile),
		Preview:     previewEditFile,
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"path", "old_text", "new_text"},
			"properties": map[string]any{
				"path":        map[string]any{"type": "string", "description": "Absolute or relative file path."},
				"old_text":    map[string]any{"type": "string", "description": "Exact text to find and replace."},
				"new_text":    map[string]any{"type": "string", "description": "Replacement text."},
				"replace_all": map[string]any{"type": "boolean", "description": "Replace all occurrences (default: false, first match only)."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Path       string `json:"path"`
				OldText    string `json:"old_text"`
				NewText    string `json:"new_text"`
				ReplaceAll bool   `json:"replace_all"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			fp, err := security.SecurePath(scope.Workdir, a.Path, true)
			if err != nil {
				return Failed(fmt.Sprintf("%v", err))
			}
			data, err := os.ReadFile(fp)
			if err != nil {
				return Failed(fmt.Sprintf("%v", err))
			}
			content := string(data)
			oldText := a.OldText
			if !strings.Contains(content, oldText) {
				// Try whitespace-tolerant fuzzy match as fallback
				for _, line := range strings.Split(content, "\n") {
					normed := security.WhitespaceNormalize(line)
					searchNormed := security.WhitespaceNormalize(oldText)
					if normed == searchNormed {
						oldText = line
						break
					}
				}
				if !strings.Contains(content, oldText) {
					return Failed("Text not found in file (tried exact and whitespace-tolerant match)")
				}
			}
			var newContent string
			if a.ReplaceAll {
				newContent = strings.ReplaceAll(content, oldText, a.NewText)
			} else {
				newContent = strings.Replace(content, oldText, a.NewText, 1)
			}
			if err := store.AtomicWrite(fp, []byte(newContent)); err != nil {
				return Failed(fmt.Sprintf("write: %v", err))
			}
			return Succeeded(fmt.Sprintf("Edited %s", a.Path))
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "delete_file",
		Description: "Delete a file.",
		RiskLevel:   RiskDanger,
		Effects:     Effects(EffectDeleteFile),
		Preview:     previewDeleteFile,
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"path"},
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "Absolute or relative file path to delete."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Path string `json:"path"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			fp, err := security.SecurePath(scope.Workdir, a.Path, true)
			if err != nil {
				return Failed(fmt.Sprintf("%v", err))
			}
			if err := os.Remove(fp); err != nil {
				return Failed(fmt.Sprintf("%v", err))
			}
			return Succeeded(fmt.Sprintf("Deleted %s", a.Path))
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "insert_file",
		Description: "Insert text at a specific line in a file. Returns the updated content.",
		RiskLevel:   RiskDanger,
		Effects:     Effects(EffectWriteFile),
		Preview:     previewInsertFile,
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"path", "insert_at", "content"},
			"properties": map[string]any{
				"path":      map[string]any{"type": "string", "description": "Absolute or relative file path."},
				"insert_at": map[string]any{"type": "integer", "minimum": 1, "description": "Line number to insert before (1-based)."},
				"content":   map[string]any{"type": "string", "description": "Text to insert."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Path     string `json:"path"`
				InsertAt int    `json:"insert_at"`
				Content  string `json:"content"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			fp, err := security.SecurePath(scope.Workdir, a.Path, true)
			if err != nil {
				return Failed(fmt.Sprintf("%v", err))
			}
			data, err := os.ReadFile(fp)
			if err != nil {
				return Failed(fmt.Sprintf("read: %v", err))
			}
			lines := strings.Split(string(data), "\n")
			idx := a.InsertAt - 1 // 1-based to 0-based
			if idx < 0 {
				idx = 0
			}
			if idx > len(lines) {
				idx = len(lines)
			}
			newLines := append([]string{}, lines[:idx]...)
			insertLines := strings.Split(a.Content, "\n")
			newLines = append(newLines, insertLines...)
			newLines = append(newLines, lines[idx:]...)
			newContent := strings.Join(newLines, "\n")
			if err := os.WriteFile(fp, []byte(newContent), 0o644); err != nil {
				return Failed(fmt.Sprintf("write: %v", err))
			}
			return Succeeded(fmt.Sprintf("Inserted at line %d in %s", a.InsertAt, a.Path))
		},
	})

	return defs
}
