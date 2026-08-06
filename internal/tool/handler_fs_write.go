package tool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go-code-agent/internal/security"
	"go-code-agent/internal/store"
)

func approvedMutationFor(scope *ToolScope, path, resolvedPath string, deleting bool) (*MutationPlan, error) {
	if scope == nil || scope.approvedMutation == nil {
		return nil, nil
	}
	mutation := scope.approvedMutation
	if filepath.Clean(mutation.Path) != filepath.Clean(path) || mutation.Delete != deleting {
		return nil, fmt.Errorf("approved mutation does not match %s", path)
	}

	current, err := os.ReadFile(resolvedPath)
	if mutation.Existed {
		if err != nil {
			return nil, fmt.Errorf("file changed since plan; review again: %w", err)
		}
		if !bytes.Equal(current, mutation.OriginalContent) {
			return nil, fmt.Errorf("file changed since plan; review again")
		}
	} else {
		if err == nil {
			return nil, fmt.Errorf("file was created after plan; review again")
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("verify file since plan: %w", err)
		}
	}
	return mutation, nil
}

func filesystemWriteTools(d builtinDeps) []ToolDefinition {
	var defs []ToolDefinition

	defs = append(defs, ToolDefinition{
		Name:         "write_file",
		Description:  "Write content to file.",
		RiskLevel:    RiskDanger,
		Effects:      Effects(EffectWriteFile),
		PlanMutation: planWriteMutation,
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
			mutation, err := approvedMutationFor(scope, a.Path, fp, false)
			if err != nil {
				return Failed(err.Error())
			}
			content := []byte(a.Content)
			if mutation != nil {
				content = mutation.Content
			}
			if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
				return Failed(fmt.Sprintf("%v", err))
			}
			if err := store.AtomicWrite(fp, content); err != nil {
				return Failed(fmt.Sprintf("%v", err))
			}
			return Succeeded(fmt.Sprintf("Wrote %d bytes to %s", len(content), a.Path))
		},
	})

	defs = append(defs, ToolDefinition{
		Name:         "edit_file",
		Description:  "Replace exact text in file.",
		RiskLevel:    RiskDanger,
		Effects:      Effects(EffectWriteFile),
		PlanMutation: planEditMutation,
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
			mutation, err := approvedMutationFor(scope, a.Path, fp, false)
			if err != nil {
				return Failed(err.Error())
			}
			if mutation != nil {
				if err := store.AtomicWrite(fp, mutation.Content); err != nil {
					return Failed(fmt.Sprintf("write: %v", err))
				}
				return Succeeded(fmt.Sprintf("Edited %s", a.Path))
			}
			data, err := os.ReadFile(fp)
			if err != nil {
				return Failed(fmt.Sprintf("%v", err))
			}
			newContent, err := replaceFileContent(string(data), a.OldText, a.NewText, a.ReplaceAll)
			if err != nil {
				if errors.Is(err, errMutationTextNotFound) {
					return Failed("Text not found in file (tried exact and whitespace-tolerant match)")
				}
				return Failed(fmt.Sprintf("edit: %v", err))
			}
			if err := store.AtomicWrite(fp, []byte(newContent)); err != nil {
				return Failed(fmt.Sprintf("write: %v", err))
			}
			return Succeeded(fmt.Sprintf("Edited %s", a.Path))
		},
	})

	defs = append(defs, ToolDefinition{
		Name:         "delete_file",
		Description:  "Delete a file.",
		RiskLevel:    RiskDanger,
		Effects:      Effects(EffectDeleteFile),
		PlanMutation: planDeleteMutation,
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
			if _, err := approvedMutationFor(scope, a.Path, fp, true); err != nil {
				return Failed(err.Error())
			}
			if err := os.Remove(fp); err != nil {
				return Failed(fmt.Sprintf("%v", err))
			}
			return Succeeded(fmt.Sprintf("Deleted %s", a.Path))
		},
	})

	defs = append(defs, ToolDefinition{
		Name:         "insert_file",
		Description:  "Insert text at a specific line in a file. Returns the updated content.",
		RiskLevel:    RiskDanger,
		Effects:      Effects(EffectWriteFile),
		PlanMutation: planInsertMutation,
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
			mutation, err := approvedMutationFor(scope, a.Path, fp, false)
			if err != nil {
				return Failed(err.Error())
			}
			if mutation != nil {
				if err := store.AtomicWrite(fp, mutation.Content); err != nil {
					return Failed(fmt.Sprintf("write: %v", err))
				}
				return Succeeded(fmt.Sprintf("Inserted at line %d in %s", a.InsertAt, a.Path))
			}
			data, err := os.ReadFile(fp)
			if err != nil {
				return Failed(fmt.Sprintf("read: %v", err))
			}
			newContent := insertFileContent(string(data), a.InsertAt, a.Content)
			if err := os.WriteFile(fp, []byte(newContent), 0o644); err != nil {
				return Failed(fmt.Sprintf("write: %v", err))
			}
			return Succeeded(fmt.Sprintf("Inserted at line %d in %s", a.InsertAt, a.Path))
		},
	})

	return defs
}
