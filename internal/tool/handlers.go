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

	"go-code-agent-refactor/internal/config"
	"go-code-agent-refactor/internal/security"
	"go-code-agent-refactor/internal/store"
)

func BuiltinTools(
	taskSvc TaskService,
	todoSvc TodoService,
	memorySvc MemoryService,
	skillLoader SkillLoader,
	bgSvc BackgroundService,
	bus MessageBus,
	subagentSvc SubagentService,
	teamSvc TeamService,
	protocolSvc TeamProtocolService,
	webSvc WebService,
) []ToolDefinition {
	var defs []ToolDefinition

	defs = append(defs, ToolDefinition{
		Name:        "bash",
		Description: "Execute a shell command. Runs with a hard 120s timeout.",
		RiskLevel:   RiskDanger,
		Effects:     Effects(EffectExecuteProcess),
		Timeout:     config.BashTimeout,
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"command"},
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "Shell command to execute."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Command string `json:"command"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			bashPolicy := security.NewDefaultBashPolicy()
			allowed, needConfirm, reason := bashPolicy.Validate(a.Command)
			if !allowed {
				return Denied("bash blocked: " + reason)
			}
			// Confirmation is handled centrally by Executor's HITL adapter.
			// BashPolicy only blocks commands that are never permitted.
			_ = needConfirm
			_ = reason
			execCtx, cancel := context.WithTimeout(context.Background(), config.BashTimeout)
			defer cancel()
			cmd := exec.CommandContext(execCtx, "sh", "-c", a.Command)
			cmd.Dir = scope.Workdir
			output, err := cmd.CombinedOutput()
			if execCtx.Err() == context.DeadlineExceeded {
				return Failed("command timed out after " + config.BashTimeout.String())
			}
			out := strings.TrimSpace(string(output))
			if err != nil && out == "" {
				out = fmt.Sprintf("Error: %v", err)
			}
			if out == "" {
				out = "(no output)"
			}
			return Succeeded(out)
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "read_file",
		Description: "Read file contents.",
		RiskLevel:   RiskAuto,
		Effects:     Effects(EffectReadFile),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"path"},
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "Absolute or relative file path."},
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

	defs = append(defs, ToolDefinition{
		Name:        "list_dir",
		Description: "List directory contents.",
		RiskLevel:   RiskAuto,
		Effects:     Effects(EffectReadFile),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "Directory path relative to workspace root (default: workspace root)."},
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
				"pattern": map[string]any{"type": "string", "description": "File name pattern with wildcards (e.g. \"*.go\")."},
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
			result := strings.Join(matches, "\n")
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
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	defs = append(defs, ToolDefinition{
		Name:        "TodoWrite",
		Description: "Update task tracking list.",
		RiskLevel:   RiskSafe,
		Effects:     Effects(EffectSessionMutation),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"items"},
			"properties": map[string]any{
				"items": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object", "required": []string{"content", "status"},
						"properties": map[string]any{
							"content": map[string]any{"type": "string", "description": "Task description."},
							"status":  map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}, "description": "Task status."},
						},
					},
				},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Items []map[string]string `json:"items"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			if todoSvc == nil {
				return Failed("todo service unavailable")
			}
			output, err := todoSvc.Update(a.Items)
			if err != nil {
				return Failed(err.Error())
			}
			return Succeeded(output)
		},
	})

	defs = append(defs,
		makeTaskTool("task_create", taskSvc),
		makeTaskTool("task_list", taskSvc),
		makeTaskTool("task_update", taskSvc),
		makeTaskTool("task_get", taskSvc),
		makeTaskTool("task_add_dep", taskSvc),
		makeTaskTool("task_remove_dep", taskSvc),
		makeTaskTool("task_ready", taskSvc),
		makeTaskTool("task_dag", taskSvc),
		makeTaskTool("claim_task", taskSvc),
	)

	defs = append(defs,
		makeMemoryTool("memory_write", memorySvc),
		makeMemoryTool("memory_search", memorySvc),
		makeMemoryTool("memory_delete", memorySvc),
		makeMemoryTool("memory_stats", memorySvc),
		makeMemoryTool("session_save_memory", memorySvc),
	)

	defs = append(defs, ToolDefinition{
		Name:        "background_run",
		Description: "Run a shell command in the background.",
		RiskLevel:   RiskDanger,
		Effects:     Effects(EffectExecuteProcess),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"command"},
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "Shell command to run in background."},
				"timeout": map[string]any{"type": "integer", "description": "Timeout in seconds (0 = no timeout)."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			if bgSvc != nil {
				return Succeeded(bgSvc.Run(scope.SessionID, a.Command, a.Timeout))
			}
			return Failed("background supervisor unavailable")
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "check_background",
		Description: "Check background task status.",
		RiskLevel:   RiskAuto,
		Effects:     Effects(),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"task_id"},
			"properties": map[string]any{
				"task_id": map[string]any{"type": "string", "description": "Background task ID returned by background_run."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				TaskID string `json:"task_id"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			if bgSvc != nil {
				return Succeeded(bgSvc.Check(a.TaskID))
			}
			return Failed("background supervisor unavailable")
		},
	})

	// --- Team tools (using closures to capture bus + callbacks) ---
	defs = append(defs, ToolDefinition{
		Name:        "spawn_teammate",
		Description: "Spawn a persistent autonomous teammate that runs in its own worktree. For code changes that need isolation. For read-only investigation, use explore instead.",
		RiskLevel:   RiskSafe,
		Effects:     Effects(EffectTeamMutation),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"name", "prompt"},
			"properties": map[string]any{
				"name":   map[string]any{"type": "string", "description": "Unique name for this teammate."},
				"role":   map[string]any{"type": "string", "description": "Optional role hint (e.g. 'researcher', 'coder')."},
				"prompt": map[string]any{"type": "string", "description": "Task description for the teammate."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Name   string `json:"name"`
				Role   string `json:"role"`
				Prompt string `json:"prompt"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			if a.Name == "" || a.Prompt == "" {
				return Failed("name and prompt are required")
			}
			if teamSvc != nil {
				return Succeeded(teamSvc.Spawn(context.Background(), a.Name, a.Role, a.Prompt))
			}
			return Failed("team spawn unavailable")
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "list_teammates",
		Description: "List all teammates and their statuses.",
		RiskLevel:   RiskAuto,
		Effects:     Effects(),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			if teamSvc != nil {
				return Succeeded(teamSvc.ListAll())
			}
			return Failed("team list unavailable")
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "send_message",
		Description: "Send a message to another agent via their inbox.",
		RiskLevel:   RiskAuto,
		Effects:     Effects(EffectTeamMutation),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"to", "content"},
			"properties": map[string]any{
				"to":      map[string]any{"type": "string", "description": "Recipient agent name."},
				"content": map[string]any{"type": "string", "description": "Message body."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				To      string `json:"to"`
				Content string `json:"content"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			if a.To == "" || a.Content == "" {
				return Failed("to and content are required")
			}
			if bus != nil {
				return Succeeded(bus.Send(scope.AgentID, a.To, a.Content, "message", nil))
			}
			return Failed("message bus unavailable")
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "read_inbox",
		Description: "Read and drain all messages from your inbox.",
		RiskLevel:   RiskAuto,
		Effects:     Effects(),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			if bus == nil {
				return Failed("message bus unavailable")
			}
			msgs := bus.ReadInbox(scope.AgentID)
			if len(msgs) == 0 {
				return Succeeded("[]")
			}
			data, _ := json.Marshal(msgs)
			return Succeeded(string(data))
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "broadcast",
		Description: "Send a message to all active teammates.",
		RiskLevel:   RiskAuto,
		Effects:     Effects(EffectTeamMutation),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"content"},
			"properties": map[string]any{
				"content": map[string]any{"type": "string", "description": "Message to broadcast."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Content string `json:"content"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			if a.Content == "" {
				return Failed("content is required")
			}
			if bus == nil || teamSvc == nil {
				return Failed("broadcast unavailable")
			}
			// Parse member names from list output
			recipients := parseTeamMemberNames(teamSvc.ListAll())
			if len(recipients) == 0 {
				return Succeeded("No teammates to broadcast to.")
			}
			return Succeeded(bus.Broadcast(scope.AgentID, a.Content, recipients))
		},
	})

	// Multi-agent protocol tools. These operate on the durable ProtocolStore,
	// not merely on the in-memory teammate list.
	defs = append(defs, ToolDefinition{
		Name:        "shutdown_request",
		Description: "Request a teammate to stop its autonomous work loop.",
		RiskLevel:   RiskSafe,
		Effects:     Effects(EffectTeamMutation),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"teammate"},
			"properties": map[string]any{
				"teammate": map[string]any{"type": "string", "description": "Teammate name to shut down."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Teammate string `json:"teammate"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			if a.Teammate == "" {
				return Failed("teammate is required")
			}
			if protocolSvc == nil {
				return Failed("team protocol unavailable")
			}
			return Succeeded(protocolSvc.ShutdownRequest(a.Teammate))
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "plan_approval",
		Description: "Approve or reject a teammate plan by request ID.",
		RiskLevel:   RiskAuto,
		Effects:     Effects(EffectTeamMutation),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"request_id", "approve"},
			"properties": map[string]any{
				"request_id": map[string]any{"type": "string", "description": "Plan request ID from inbox."},
				"approve":    map[string]any{"type": "boolean", "description": "true = approve, false = reject."},
				"feedback":   map[string]any{"type": "string", "description": "Optional feedback for the teammate."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				RequestID string `json:"request_id"`
				Approve   bool   `json:"approve"`
				Feedback  string `json:"feedback"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			if a.RequestID == "" {
				return Failed("request_id is required")
			}
			if scope == nil || scope.Role != "lead" {
				return Denied("only the lead may approve plans")
			}
			if protocolSvc == nil {
				return Failed("team protocol unavailable")
			}
			return Succeeded(protocolSvc.ReviewPlan(a.RequestID, a.Approve, a.Feedback))
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "submit_plan",
		Description: "Submit a teammate plan for lead approval before mutations.",
		RiskLevel:   RiskSafe,
		Effects:     Effects(EffectTeamMutation),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"plan"},
			"properties": map[string]any{
				"plan": map[string]any{"type": "string", "description": "Plan description for lead review."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Plan string `json:"plan"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			if a.Plan == "" {
				return Failed("plan is required")
			}
			if protocolSvc == nil {
				return Failed("team protocol unavailable")
			}
			return Succeeded(protocolSvc.SubmitPlan(scope.AgentID, a.Plan))
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "compress",
		Description: "Manually compress conversation context.",
		RiskLevel:   RiskAuto,
		Effects:     Effects(),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			return Succeeded("Compressing...")
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "load_skill",
		Description: "Load specialized knowledge by name.",
		RiskLevel:   RiskAuto,
		Effects:     Effects(),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"name"},
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "Skill name to load."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Name string `json:"name"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			if skillLoader != nil {
				return Succeeded(skillLoader.Load(a.Name))
			}
			return Failed("no skills loaded")
		},
	})

	// web_fetch — delegates to a read-only subagent that fetches the page
	// in its own isolated context and returns only a concise summary.
	// Raw page content never enters the lead agent's context.
	// When called from within a subagent (role != lead), it performs the
	// actual network call directly to avoid infinite delegation.
	defs = append(defs, ToolDefinition{
		Name:        "web_fetch",
		Description: "Fetch and analyze a web page, returning a concise summary (NOT the raw page). A read-only subagent reads the page in its own isolated context and distills the findings — raw page content never enters your context window.",
		RiskLevel:   RiskSafe,
		Effects:     Effects(EffectNetworkAccess),
		Timeout:     config.WebFetchTimeout,
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"url"},
			"properties": map[string]any{
				"url":    map[string]any{"type": "string", "description": "Full URL to fetch (https://...)."},
				"prompt": map[string]any{"type": "string", "description": "Optional: what specific information to extract from the page."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				URL    string `json:"url"`
				Prompt string `json:"prompt"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			// If this is a subagent itself, do the raw fetch directly.
			if scope != nil && scope.Role != "lead" {
				if webSvc == nil {
					return Failed("web_fetch unavailable")
				}
				ctx, cancel := context.WithTimeout(context.Background(), config.WebFetchTimeout)
				defer cancel()
				output, err := webSvc.Fetch(ctx, a.URL)
				if err != nil {
					return Failed(err.Error())
				}
				return Succeeded(output)
			}
			// Lead agent: delegate to a read-only subagent.
			if subagentSvc == nil {
				return Failed("subagent unavailable")
			}
			subPrompt := fmt.Sprintf(
				"Fetch and analyze the page at %s.\n\n%s\n\n"+
					"Read the full page content. Provide a concise, well-structured summary. "+
					"If a specific question is present in the prompt, answer it directly using the page content. "+
					"Keep your response focused and relevant.",
				a.URL, a.Prompt,
			)
			ctx, cancel := context.WithTimeout(context.Background(), config.WebFetchTimeout*3)
			defer cancel()
			output := subagentSvc.Run(ctx, subPrompt, "web_fetch", scope.Workdir)
			return Succeeded(output)
		},
	})

	// web_search
	defs = append(defs, ToolDefinition{
		Name:        "web_search",
		Description: "Search the web via DDG/SearXNG/Tavily/Brave (zero-config).",
		RiskLevel:   RiskSafe,
		Effects:     Effects(EffectNetworkAccess),
		Timeout:     config.WebSearchTimeout,
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"query"},
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Search query string."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Query string `json:"query"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			if webSvc != nil {
				ctx, cancel := context.WithTimeout(context.Background(), config.WebSearchTimeout)
				defer cancel()
				output, err := webSvc.Search(ctx, a.Query)
				if err != nil {
					return Failed(err.Error())
				}
				return Succeeded(output)
			}
			return Failed("web_search unavailable")
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "explore",
		Description: "Delegate investigation to a read-only subagent. Returns a summary of findings.",
		RiskLevel:   RiskSafe,
		Effects:     Effects(),
		Timeout:     config.SubagentTimeout,
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"prompt"},
			"properties": map[string]any{
				"prompt":     map[string]any{"type": "string", "description": "What to investigate and report back."},
				"agent_type": map[string]any{"type": "string", "description": "Optional type hint (e.g. 'explore', 'web_fetch')."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Prompt    string `json:"prompt"`
				AgentType string `json:"agent_type"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			if subagentSvc != nil {
				parent := scope.Context
				if parent == nil {
					parent = context.Background()
				}
				ctx, cancel := context.WithTimeout(parent, config.SubagentTimeout)
				defer cancel()
				return Succeeded(subagentSvc.Run(ctx, a.Prompt, a.AgentType, scope.Workdir))
			}
			return Failed("subagent runner unavailable")
		},
	})

	return defs
}

var _ = context.Background

// parseTeamMemberNames extracts member names from the TeammateManager list output.
// The list format is: "Team: <name>\n  Alice (role): working\n  Bob (role): idle"
func parseTeamMemberNames(listOutput string) []string {
	lines := strings.Split(strings.TrimSpace(listOutput), "\n")
	var names []string
	for _, line := range lines {
		// Skip header line starting with "Team:"
		if strings.HasPrefix(strings.TrimSpace(line), "Team:") {
			continue
		}
		// Extract name: "  Alice (role): working" → "Alice"
		trimmed := strings.TrimSpace(line)
		if idx := strings.Index(trimmed, " ("); idx > 0 {
			names = append(names, trimmed[:idx])
		}
	}
	return names
}
