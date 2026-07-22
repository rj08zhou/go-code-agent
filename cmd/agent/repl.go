package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/internal/agent"
	"go-code-agent/internal/application"
	"go-code-agent/internal/hitlaudit"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/model"
	"go-code-agent/internal/security"
	"go-code-agent/internal/utils"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

type repl struct {
	built  *application.BuiltRunner
	rtCtx  context.Context
	readFn func() (string, error)
	next   *application.BuildOptions
}

func newRepl(built *application.BuiltRunner, rtCtx context.Context, readFn func() (string, error)) *repl {
	return &repl{built: built, rtCtx: rtCtx, readFn: readFn}
}

func (r *repl) run() {
	r.next = nil
	runner := r.built.Runner
	histStore := r.built.HistStore

	messages, restored, err := histStore.LoadRuntime(r.built.SysPrompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load history: %v\n", err)
		return
	}
	if restored > 0 {
		fmt.Printf("[restored %d conversation entries]\n", restored)
	}

	r.printBanner()

	ctx, cancel := context.WithCancel(r.rtCtx)
	defer cancel()

	sigCount := 0
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		for range sigCh {
			sigCount++
			if sigCount >= 2 {
				fmt.Fprintln(os.Stderr, "\nForce quitting...")
				os.Exit(1)
			}
			fmt.Fprintln(os.Stderr, "\nShutting down... (Ctrl+C again to force quit)")
			cancel()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line, err := r.readFn()
		if err != nil {
			fmt.Println("Goodbye!")
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			if r.handleCommand(ctx, line, &messages) {
				return
			}
			continue
		}

		// Drain background notifications and inbox before LLM call
		r.drainBackground()
		r.checkInbox(&messages)

		userMsg := llm.UserMessage(line)
		messages = append(messages, userMsg)
		histStore.AppendUser(line)

		before := len(messages)
		runCtx := agent.WithPersistedBoundary(ctx, &before)
		outcome := runner.Run(runCtx, messages, model.NewTraceID())
		messages = outcome.Messages
		if before > len(messages) {
			before = len(messages)
		}

		for _, m := range messages[before:] {
			switch m.Role {
			case llm.RoleAssistant:
				histStore.AppendAssistant(m.Content, m.ToolCalls)
			case llm.RoleTool:
				histStore.AppendTool(m.ToolCallID, m.Content)
			}
		}
		if len(outcome.Messages) > 0 && outcome.Rounds > 0 && len(messages) > 10 {
			histStore.AppendCheckpoint(
				fmt.Sprintf("round %d", outcome.Rounds),
				histStore.WrittenCount(),
			)
		}
		if outcome.Error != nil {
			fmt.Fprintf(os.Stderr, "%s[error]%s %v\n", utils.Red, utils.Reset, outcome.Error)
		}
		histStore.Sync()
		fmt.Println()
	}
}

func (r *repl) printBanner() {
	fmt.Printf("go-code-agent | session: %s\n", r.built.SessionID)
	fmt.Printf("workdir: %s | model: %s | HITL: %s", r.built.Workdir, r.built.ModelID, hitlStatus(r.built))
	if r.built.JudgeEnabled {
		fmt.Print(" | judge: on")
	}
	perms := r.built.Permissions
	if perms != nil && perms.Count() > 0 {
		fmt.Printf(" | permissions: %d rules", perms.Count())
	}
	mcpCount := r.built.MCPMgr.Count()
	pending := r.built.MCPMgr.ListPending()
	if mcpCount > 0 {
		fmt.Printf(" | mcp: %d active", mcpCount)
	}
	if len(pending) > 0 {
		fmt.Printf(" | mcp: %d pending", len(pending))
	}
	fmt.Println()
	fmt.Println("Type a message, /help for commands, Ctrl-D to exit.")
}

func (r *repl) drainBackground() {
	for _, n := range r.built.BGSvc.Notifications() {
		fmt.Fprintf(os.Stderr, "[bg] job %s: %s\n", n["id"], n["status"])
	}
	for _, j := range r.built.BGSvc.Drain() {
		fmt.Fprintln(os.Stderr, "[bg] completed:", j)
	}
}

func (r *repl) checkInbox(messages *[]llm.Message) {
	mb := r.built.Bus
	if mb == nil {
		return
	}
	msgs := mb.ReadInbox(r.built.AgentID)
	if len(msgs) == 0 {
		return
	}
	for _, m := range msgs {
		from, _ := m["from"].(string)
		ct, _ := m["content"].(string)
		msgType, _ := m["type"].(string)
		text := fmt.Sprintf("[From %s] %s", from, ct)
		if msgType == "shutdown_request" {
			text = fmt.Sprintf("[Shutdown request from %s]", from)
		}
		*messages = append(*messages, llm.SystemMessage(text))
	}
}

func (r *repl) handleCommand(ctx context.Context, cmd string, messages *[]llm.Message) bool {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return false
	}
	switch parts[0] {
	case "/help":
		fmt.Println(strings.TrimSpace(`
Commands:
  /tasks           - List tasks
  /dag             - Show DAG topology
  /memory          - Memory stats
  /mcp             - MCP server list
  /team            - List teammates
  /session         - Session info
  /judge           - Toggle judge
  /approve [mode]  - off|safe|danger (default startup: safe)
  /hitl [mode]     - off|on|safe-only|interactive|auto-approve|...
  /compact         - Trigger manual compaction
  /exit /quit      - Exit

HITL defaults to on (safe-only): safe tools auto-approved, danger tools prompt.
  /approve off|safe|danger  — quick presets (also syncs HITL mode)
  /hitl off                 — disable approval prompts entirely
  /hitl on                  — re-enable HITL at the current mode
Bash deny-list and permissions.json still apply when HITL is off.
`))
	case "/task":
		if len(parts) < 2 {
			fmt.Println("Usage: /task clear|reset")
			fmt.Println("  clear — mark all completed tasks as deleted (hide from /dag)")
			fmt.Println("  reset — remove all tasks and start fresh")
		} else {
			switch parts[1] {
			case "clear":
				fmt.Println(r.built.TaskSvc.ClearCompleted())
			case "reset":
				r.built.TaskSvc.Reset()
				fmt.Println("All tasks cleared.")
			default:
				fmt.Printf("Unknown: %s\n", parts[1])
			}
		}
	case "/tasks":
		if r.built.TodoSvc != nil {
			fmt.Println(r.built.TodoSvc.Render())
		} else {
			fmt.Println("No todos.")
		}
		fmt.Println("(persistent / dependency-tracked tasks are shown via /dag)")
	case "/dag":
		fmt.Println(r.built.TaskSvc.TopoView())
		if progress := r.built.TaskSvc.ProgressSummary(); progress != "" {
			fmt.Println(progress)
		}
	case "/memory":
		fmt.Println(r.built.MemoryStore.Stats())
	case "/mcp":
		if len(parts) >= 2 && parts[1] == "pending" {
			pending := r.built.MCPMgr.ListPending()
			if len(pending) == 0 {
				fmt.Println("No pending MCP servers.")
			} else {
				fmt.Printf("Pending MCP servers: %v\n", pending)
				fmt.Println("Use /mcp approve <name> to start a server.")
			}
		} else if len(parts) >= 3 && parts[1] == "approve" {
			fmt.Println(r.built.MCPMgr.Approve(context.Background(), parts[2]))
		} else if len(parts) >= 4 && parts[1] == "connect" {
			fmt.Println(r.built.MCPMgr.Connect(context.Background(), parts[2], parts[3], parts[4:]))
		} else if len(parts) >= 3 && parts[1] == "disconnect" {
			fmt.Println(r.built.MCPMgr.Disconnect(parts[2]))
		} else {
			fmt.Println("MCP servers: " + r.built.MCPMgr.List())
		}
	case "/team":
		if len(parts) == 1 {
			fmt.Println(r.built.TeamMgr.ListAll())
			break
		}
		switch parts[1] {
		case "spawn":
			if len(parts) < 5 {
				fmt.Println("Usage: /team spawn <name> <role> <prompt>")
			} else {
				fmt.Println(r.built.TeamMgr.Spawn(context.Background(), parts[2], parts[3], strings.Join(parts[4:], " ")))
			}
		case "shutdown":
			if len(parts) < 3 {
				fmt.Println("Usage: /team shutdown <name>")
			} else {
				fmt.Println(r.built.TeamMgr.ShutdownByName(parts[2]))
			}
		case "message":
			if len(parts) < 4 {
				fmt.Println("Usage: /team message <name> <content>")
			} else {
				fmt.Println(r.built.Bus.Send("lead", parts[2], strings.Join(parts[3:], " "), "message", nil))
			}
		case "inbox":
			data, _ := json.Marshal(r.built.Bus.ReadInbox("lead"))
			fmt.Println(string(data))
		default:
			fmt.Println(r.built.TeamMgr.ListAll())
		}
	case "/session":
		if len(parts) < 2 {
			fmt.Printf("Session: %s (%s)\n", r.built.SessionID, r.built.SessionTitle)
			fmt.Println("Usage: /session [list|switch <id>|new|rename <title>|archive]")
		} else {
			switch parts[1] {
			case "list":
				fmt.Println(r.built.SessionRepo.ListSessions())
			case "switch":
				if len(parts) < 3 {
					fmt.Println("Usage: /session switch <id>")
				} else if err := r.built.SessionRepo.SwitchActive(parts[2]); err != "" {
					fmt.Println(err)
				} else {
					r.next = &application.BuildOptions{SessionID: parts[2]}
					return true
				}
			case "new":
				r.next = &application.BuildOptions{NewSession: true}
				return true
			case "rename":
				if len(parts) < 3 {
					fmt.Println("Usage: /session rename <title>")
				} else {
					fmt.Println(r.built.SessionRepo.RenameSession(r.built.SessionID, strings.Join(parts[2:], " ")))
				}
			case "archive":
				if r.built.MemoryStore != nil {
					r.built.MemoryStore.SaveSessionMemory(r.built.SessionID, summarizeMessages(*messages))
				}
				if err := r.built.SessionRepo.ArchiveSession(r.built.SessionID); err != "" {
					fmt.Println(err)
				} else {
					r.next = &application.BuildOptions{NewSession: true}
					return true
				}
			default:
				fmt.Printf("Unknown session command: %s\n", parts[1])
			}
		}
	case "/judge":
		if r.built.Judge.IsEnabled() {
			r.built.Judge.SetEnabled(false)
		} else {
			r.built.Judge.SetEnabled(true)
		}
		fmt.Printf("Judge: %v\n", r.built.Judge.IsEnabled())
	case "/hitl":
		if len(parts) > 1 {
			switch strings.ToLower(parts[1]) {
			case "off":
				r.built.HitlMgr.SetEnabled(false)
				fmt.Println("HITL disabled — no approval prompts (bash deny / permissions still apply).")
			case "on":
				r.built.HitlMgr.SetEnabled(true)
				fmt.Printf("HITL enabled (mode=%s).\n", r.built.HitlMgr.Mode())
			default:
				if mode, err := hitlaudit.ParseMode(parts[1]); err == nil {
					r.built.HitlMgr.SetMode(mode)
					r.built.HitlMgr.SetEnabled(true)
					r.syncApprovalWithHITL(mode)
					fmt.Printf("HITL mode: %s\n", parts[1])
				} else {
					fmt.Println(err)
					fmt.Println("Usage: /hitl [off|on|interactive|safe-only|auto-approve|auto-reject|notify-only]")
				}
			}
		} else {
			r.built.HitlMgr.SetEnabled(!r.built.HitlMgr.IsEnabled())
			if r.built.HitlMgr.IsEnabled() {
				fmt.Printf("HITL enabled (mode=%s).\n", r.built.HitlMgr.Mode())
			} else {
				fmt.Println("HITL disabled.")
			}
		}
	case "/inbox":
		data, _ := json.Marshal(r.built.Bus.ReadInbox(r.built.AgentID))
		fmt.Println(string(data))
	case "/search":
		if len(parts) < 2 {
			fmt.Println("Usage: /search <query>")
		} else if r.built.WebService != nil {
			output, err := r.built.WebService.Search(context.Background(), strings.Join(parts[1:], " "))
			if err != nil {
				fmt.Println(err)
			} else {
				fmt.Println(output)
			}
		}
	case "/permissions":
		if len(parts) > 1 && parts[1] == "reload" && r.built.ReloadPermissions != nil {
			if err := r.built.ReloadPermissions(); err != nil {
				fmt.Println(err)
			} else {
				fmt.Println("Permissions reloaded.")
			}
		} else {
			fmt.Println(r.built.Permissions.Describe())
		}
	case "/usage":
		if r.built.UsageTracker != nil {
			fmt.Println(r.built.UsageTracker.Render())
		} else {
			fmt.Println("Usage tracking not available.")
		}
	case "/security":
		fmt.Println(securityDesc())
	case "/security test-bash":
		if len(parts) < 3 {
			fmt.Println("Usage: /security test-bash <command>")
		} else {
			cmd := strings.Join(parts[2:], " ")
			fmt.Printf("Testing: %s\nResult: TODO - validate path\n", cmd)
		}
	case "/approve":
		if len(parts) < 2 {
			r.printApproveStatus()
		} else {
			switch parts[1] {
			case "off", "reset":
				r.applyApprovePreset("off", hitlaudit.HITLModeInteractive)
				fmt.Println("All tools require manual confirmation. Diff preview enabled.")
			case "safe":
				r.applyApprovePreset("safe", hitlaudit.HITLModeSafeOnly)
				fmt.Println("Safe tools auto-approved; dangerous tools require confirmation. Diff preview enabled.")
			case "danger", "all":
				r.applyApprovePreset("danger", hitlaudit.HITLModeAutoApprove)
				fmt.Println("ALL tools auto-approved, diff preview skipped — use with caution.")
			default:
				fmt.Printf("Unknown level: %s\n", parts[1])
			}
		}
	case "/decisions":
		if r.built.DecisionLog != nil {
			fmt.Println(r.built.DecisionLog.Render())
		} else {
			fmt.Println("Decision log not available.")
		}
	case "/compact":
		if r.built.Compact == nil {
			fmt.Println("Compaction unavailable.")
		} else {
			*messages = r.built.Compact(ctx, *messages)
			fmt.Printf("Compacted conversation to %d messages.\n", len(*messages))
		}
	case "/exit", "/quit":
		fmt.Println("Goodbye!")
		return true
	default:
		fmt.Printf("Unknown command: %s\n", parts[0])
	}
	return false
}

func (r *repl) nextBuild() *application.BuildOptions { return r.next }

func (r *repl) approvalState() *security.ApprovalState {
	if r.built != nil && r.built.Approval != nil {
		return r.built.Approval
	}
	return security.ActiveApproval()
}

func (r *repl) applyApprovePreset(preset string, mode hitlaudit.HITLMode) {
	r.built.HitlMgr.SetEnabled(true)
	r.built.HitlMgr.SetMode(mode)
	r.approvalState().ApplyPreset(preset)
}

func (r *repl) syncApprovalWithHITL(mode hitlaudit.HITLMode) {
	switch mode {
	case hitlaudit.HITLModeAutoApprove:
		r.approvalState().ApplyPreset("danger")
	case hitlaudit.HITLModeSafeOnly:
		r.approvalState().ApplyPreset("safe")
	default:
		r.approvalState().ApplyPreset("off")
	}
}

func (r *repl) printApproveStatus() {
	state := r.approvalState()
	mode := r.built.HitlMgr.Mode()
	fmt.Println("Approval status:")
	fmt.Printf("  HITL mode:           %s\n", mode)
	fmt.Printf("  Auto-approve safe:   %v\n", state.IsAutoApproveSafe())
	fmt.Printf("  Auto-approve all:    %v\n", state.IsAutoApproveAll())
	fmt.Printf("  Diff preview:        %v\n", state.ShouldPreviewDiff())
	fmt.Println()
	fmt.Println("Usage: /approve off|safe|danger")
	fmt.Println("  off    — manual confirmation for every tool; diff preview on")
	fmt.Println("  safe   — auto-approve safe tools; prompt for danger; diff preview on [default]")
	fmt.Println("  danger — auto-approve ALL tools; skip diff preview")
}

func summarizeMessages(messages []llm.Message) string {
	var parts []string
	for i := len(messages) - 1; i >= 0 && len(parts) < 5; i-- {
		if messages[i].Role == llm.RoleUser || messages[i].Role == llm.RoleAssistant {
			text := strings.TrimSpace(messages[i].Content)
			if text != "" {
				if len(text) > 300 {
					text = text[:300]
				}
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func securityDesc() string {
	return `Security Status:
  Bash: whitelist (85 commands) + deny/confirm regexps
  Permissions: rules loaded from permissions.json
  Secrets: output sanitizer active
  Diff: preview available for file writes`
}
