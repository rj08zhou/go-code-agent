package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go-code-agent/internal/config"
	"go-code-agent/internal/security"
)

func filesystemReadTools(d builtinDeps) []ToolDefinition {
	var defs []ToolDefinition

	defs = append(defs, ToolDefinition{
		Name:        "read_file",
		Description: "Read file contents.",
		RiskLevel:   RiskAuto,
		Effects:     Effects(EffectReadFile),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"path"},
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "File path relative to the workspace root (preferred), or an absolute path inside the workspace."},
				"offset": map[string]any{"type": "integer", "minimum": 0, "description": "Line number to start reading from (0-indexed)."},
				"limit":  map[string]any{"type": "integer", "minimum": 1, "description": "Maximum number of lines to read."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			if a.Offset == 0 && a.Limit == 0 {
				a.Limit = config.ReadFileDefaultLimit
			}
			fp, err := security.SecurePath(scope.Workdir, a.Path, false)
			if err != nil {
				return Failed(fmt.Sprintf("%v", err))
			}
			f, err := os.Open(fp)
			if err != nil {
				return Failed(fmt.Sprintf("%v", err))
			}
			defer f.Close()
			var buf strings.Builder
			scanner := bufio.NewScanner(f)
			scanner.Buffer(make([]byte, 64*1024), config.MaxOutputLen)
			lineCount := 0
			truncated := false
			for scanner.Scan() {
				if a.Offset > 0 && lineCount < a.Offset {
					lineCount++
					continue
				}
				if a.Limit > 0 && lineCount-a.Offset >= a.Limit {
					truncated = true
					break
				}
				line := scanner.Text()
				separator := 0
				if buf.Len() > 0 {
					separator = 1
				}
				if buf.Len()+separator+len(line) > config.MaxOutputLen {
					truncated = true
					break
				}
				if separator == 1 {
					buf.WriteByte('\n')
				}
				buf.WriteString(line)
				lineCount++
			}
			if scanner.Err() != nil {
				truncated = true
			}
			if truncated {
				buf.WriteString(fmt.Sprintf("\n[output truncated at %d bytes; continue with offset=%d]\n", config.MaxOutputLen, lineCount))
			}
			return Succeeded(buf.String())
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "list_dir",
		Description: "List directory contents.",
		RiskLevel:   RiskAuto,
		Effects:     Effects(EffectReadFile),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "Directory relative to workspace root (preferred; default '.'), or an absolute path inside the workspace."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Path string `json:"path"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			dir := scope.Workdir
			if a.Path != "" {
				resolved, err := security.SecurePath(dir, a.Path, false)
				if err != nil {
					return Failed(fmt.Sprintf("%v", err))
				}
				dir = resolved
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				return Failed(fmt.Sprintf("%v", err))
			}
			var lines []string
			truncated := false
			for _, e := range entries {
				info, infoErr := e.Info()
				if infoErr != nil {
					return Failed(fmt.Sprintf("stat %s: %v", e.Name(), infoErr))
				}
				marker := "  "
				if e.IsDir() {
					marker = "d "
				}
				line := fmt.Sprintf("%s%8d  %s", marker, info.Size(), e.Name())
				candidate := strings.Join(append(lines, line), "\n")
				if len(candidate) > config.MaxOutputLen {
					truncated = true
					break
				}
				lines = append(lines, line)
			}
			result := strings.Join(lines, "\n")
			if truncated {
				result += fmt.Sprintf("\n[output truncated at %d bytes; narrow the path or use a more specific query]\n", config.MaxOutputLen)
			}
			return Succeeded(result)
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "search_file",
		Description: "Find files by name pattern (glob).",
		RiskLevel:   RiskAuto,
		Effects:     Effects(EffectReadFile),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"pattern"},
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "File name pattern with wildcards (e.g. \"*_test.go\"). Prefer narrow patterns; broad globs like \"*.go\" are capped."},
				"path":    map[string]any{"type": "string", "description": "Directory to search in (default: workspace root)."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			root := scope.Workdir
			if a.Path != "" {
				resolved, err := security.SecurePath(root, a.Path, false)
				if err != nil {
					return Failed(fmt.Sprintf("%v", err))
				}
				root = resolved
			}
			var matches []string
			var walkErr error
			skipDirs := map[string]bool{".git": true, "node_modules": true, ".go-code-agent": true}
			walkErr = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
				if err != nil {
					walkErr = err
					return nil
				}
				if d.IsDir() && skipDirs[d.Name()] {
					return filepath.SkipDir
				}
				if !d.IsDir() {
					if ok, _ := filepath.Match(a.Pattern, d.Name()); ok {
						rel, relErr := filepath.Rel(scope.Workdir, p)
						if relErr == nil {
							matches = append(matches, rel)
						}
					}
				}
				return nil
			})
			if walkErr != nil {
				return Failed(fmt.Sprintf("search: %v", walkErr))
			}
			if len(matches) == 0 {
				return Succeeded("No files matched.")
			}
			total := len(matches)
			if total > config.SearchFileMaxMatches {
				matches = matches[:config.SearchFileMaxMatches]
			}
			result := strings.Join(matches, "\n")
			if total > config.SearchFileMaxMatches {
				result += fmt.Sprintf(
					"\n[truncated: showing %d/%d matches; narrow the path or pattern]\n",
					config.SearchFileMaxMatches, total)
			}
			if len(result) > config.MaxOutputLen {
				result = result[:config.MaxOutputLen]
				result += fmt.Sprintf("\n[output truncated at %d bytes; narrow the path or pattern]\n", config.MaxOutputLen)
			}
			return Succeeded(result)
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "search_content",
		Description: "Search file contents by regex (like grep -rn).",
		RiskLevel:   RiskAuto,
		Effects:     Effects(EffectReadFile),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"pattern"},
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "Regular expression to search for."},
				"path":    map[string]any{"type": "string", "description": "Directory or file to search in (default: workspace root)."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			if a.Pattern == "" {
				return Failed("pattern is required")
			}
			root := scope.Workdir
			if a.Path != "" {
				resolved, err := security.SecurePath(root, a.Path, false)
				if err != nil {
					return Failed(fmt.Sprintf("%v", err))
				}
				root = resolved
			}
			ctx, cancel := context.WithTimeout(scopeParentContext(scope), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "grep", "-rnI",
				"--exclude-dir=.git", "--exclude-dir=node_modules", "--exclude-dir=.go-code-agent",
				"-E", a.Pattern, root)
			out, err := cmd.CombinedOutput()
			result := strings.TrimSpace(string(out))
			if err != nil && result == "" {
				return Succeeded("No matches found.")
			}
			return Succeeded(result)
		},
	})

	return defs
}
