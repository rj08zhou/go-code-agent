package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/chzyer/readline"

	"go-code-agent/internal/agent"
	"go-code-agent/internal/application"
	"go-code-agent/internal/history"
	"go-code-agent/internal/hitlaudit"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/model"
	"go-code-agent/internal/security"
	"go-code-agent/internal/utils"
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

// turnCanceller tracks the active agent turn so Ctrl-C can interrupt it
// without tearing down the whole REPL session.
type turnCanceller struct {
	mu     sync.Mutex
	cancel context.CancelFunc
}

func (t *turnCanceller) arm(cancel context.CancelFunc) {
	t.mu.Lock()
	t.cancel = cancel
	t.mu.Unlock()
}

func (t *turnCanceller) disarm() {
	t.mu.Lock()
	t.cancel = nil
	t.mu.Unlock()
}

func (t *turnCanceller) interrupt() bool {
	t.mu.Lock()
	cancel := t.cancel
	t.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

func (r *repl) run() {
	r.next = nil

	messages, restored, err := r.built.Session.HistStore.LoadRuntime(r.built.Session.SysPrompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load history: %v\n", err)
		return
	}
	r.printBanner(restored)

	sessionCtx, cancelSession := context.WithCancel(r.rtCtx)
	defer cancelSession()

	var turns turnCanceller
	defer r.watchSignals(sessionCtx, cancelSession, &turns)()

	for sessionCtx.Err() == nil {
		line, action := r.readPrompt()
		switch action {
		case promptContinue:
			continue
		case promptExit:
			return
		}

		if strings.HasPrefix(line, "/") {
			if r.handleCommand(sessionCtx, line, &messages) {
				return
			}
			continue
		}

		messages = r.runUserTurn(sessionCtx, &turns, messages, line)
		if sessionCtx.Err() != nil {
			return
		}
		fmt.Println()
	}
}

type promptAction int

const (
	promptRun promptAction = iota
	promptContinue
	promptExit
)

func (r *repl) readPrompt() (string, promptAction) {
	line, err := r.readFn()
	if err != nil {
		switch {
		case errors.Is(err, readline.ErrInterrupt):
			return "", promptContinue
		case errors.Is(err, io.EOF):
			fmt.Println("Goodbye!")
		default:
			fmt.Fprintf(os.Stderr, "Input error: %v\n", err)
		}
		return "", promptExit
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", promptContinue
	}
	return line, promptRun
}

func (r *repl) watchSignals(sessionCtx context.Context, cancelSession context.CancelFunc, turns *turnCanceller) func() {
	sigCh := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-sessionCtx.Done():
				return
			case sig := <-sigCh:
				if sig == syscall.SIGTERM {
					fmt.Fprintln(os.Stderr, "\nShutting down...")
					cancelSession()
					return
				}
				if turns.interrupt() {
					fmt.Fprintln(os.Stderr, "\nInterrupting current turn...")
				}
			}
		}
	}()
	return func() {
		signal.Stop(sigCh)
		close(done)
	}
}

func (r *repl) runUserTurn(sessionCtx context.Context, turns *turnCanceller, messages []llm.Message, line string) []llm.Message {
	histStore := r.built.Session.HistStore
	r.drainBackground()
	r.checkInbox(&messages)

	userHistorySaved := true
	if err := persistUserHistory(histStore, line); err != nil {
		fmt.Fprintln(os.Stderr, formatHistorySaveWarning(err))
		userHistorySaved = false
	}
	messages = append(messages, llm.UserMessage(line))

	before := len(messages)
	if !userHistorySaved {
		// Compaction must not treat this unsaved user message as covered.
		before--
	}

	turnCtx, cancelTurn := context.WithCancel(sessionCtx)
	turns.arm(cancelTurn)
	outcome := r.built.Runtime.Runner.Run(
		agent.WithPersistedBoundary(turnCtx, &before),
		messages,
		model.NewTraceID(),
	)
	turns.disarm()
	cancelTurn()

	// Incomplete streamed text is not part of outcome.Messages, so a
	// cancelled partial response is not persisted.
	messages = outcome.Messages
	if before > len(messages) {
		before = len(messages)
	}
	if userHistorySaved {
		if err := persistAgentHistory(histStore, messages[before:]); err != nil {
			fmt.Fprintln(os.Stderr, formatHistorySaveWarning(err))
		}
	}
	if notice := formatTurnOutcomeError(outcome.Error, sessionCtx.Err()); notice != "" {
		fmt.Fprintln(os.Stderr, notice)
	}
	return messages
}

func formatTurnOutcomeError(turnErr, sessionErr error) string {
	if turnErr == nil {
		return ""
	}
	if errors.Is(turnErr, context.Canceled) {
		if sessionErr == nil {
			return utils.Dim + "[interrupted] current turn stopped" + utils.Reset
		}
		return ""
	}
	return fmt.Sprintf("%s[error]%s %v", utils.Red, utils.Reset, turnErr)
}

func persistUserHistory(histStore *history.Store, content string) error {
	if err := histStore.AppendUser(content); err != nil {
		return fmt.Errorf("append user message: %w", err)
	}
	if err := histStore.Sync(); err != nil {
		return fmt.Errorf("sync user message: %w", err)
	}
	return nil
}

func persistAgentHistory(histStore *history.Store, messages []llm.Message) error {
	if err := appendHistoryMessages(histStore, messages); err != nil {
		return err
	}
	if err := histStore.Sync(); err != nil {
		return fmt.Errorf("sync agent output: %w", err)
	}
	return nil
}

func formatHistorySaveWarning(err error) string {
	detail := strings.Join(strings.Fields(err.Error()), " ")
	return fmt.Sprintf("%s[warn]%s history save failed; continuing in memory (this turn may not be restorable): %s",
		utils.Yellow, utils.Reset, utils.Truncate(detail, 300))
}

func appendHistoryMessages(histStore *history.Store, messages []llm.Message) error {
	for _, message := range messages {
		switch message.Role {
		case llm.RoleAssistant:
			if err := histStore.AppendAssistant(message.Content, message.ToolCalls); err != nil {
				return fmt.Errorf("append assistant message: %w", err)
			}
		case llm.RoleTool:
			if err := histStore.AppendTool(message.ToolCallID, message.Content); err != nil {
				return fmt.Errorf("append tool result %q: %w", message.ToolCallID, err)
			}
		}
	}
	return nil
}

func (r *repl) printBanner(restored int) {
	fmt.Print(renderBanner(r.built, restored))
}

func renderBanner(b *application.BuiltRunner, restored int) string {
	judgeStatus := "off"
	if b.Runtime.JudgeEnabled {
		judgeStatus = "on"
	}

	providerName := strings.TrimSpace(b.Runtime.ProviderName)
	if providerName == "" {
		providerName = "unknown"
	}
	endpointHost := strings.TrimSpace(b.Runtime.EndpointHost)
	if endpointHost == "" {
		endpointHost = "unknown"
	}

	var out strings.Builder
	divider := strings.Repeat("=", 60)
	fmt.Fprintf(&out, "%s%s%s\n", utils.Bold+utils.Cyan, divider, utils.Reset)
	fmt.Fprintf(&out, "%s  go-code-agent%s\n", utils.Bold+utils.Cyan, utils.Reset)
	fmt.Fprintf(&out, "  Provider: %s  |  Model: %s\n", providerName, b.Session.ModelID)
	fmt.Fprintf(&out, "  Endpoint: %s  |  Reasoning: %s\n", endpointHost, reasoningStatus(b.Runtime))
	fmt.Fprintf(&out, "  Workspace: %s\n", b.Session.Workdir)
	fmt.Fprintf(&out, "  Session: %s", b.Session.ID)
	if b.Session.Title != "" {
		fmt.Fprintf(&out, " - %s", b.Session.Title)
	}
	if restored > 0 {
		fmt.Fprintf(&out, "  |  Restored: %d conversation entries", restored)
	}
	out.WriteByte('\n')
	fmt.Fprintf(&out, "  Approval: %s  |  Judge: %s", effectiveApprovalMode(b), judgeStatus)
	if perms := b.Security.Permissions; perms != nil && perms.Count() > 0 {
		fmt.Fprintf(&out, "  |  Permissions: %d rules", perms.Count())
	}
	out.WriteByte('\n')
	if b.Team.MCP != nil {
		active := b.Team.MCP.Count()
		pending := len(b.Team.MCP.ListPending())
		failed := b.Team.MCP.FailedCount()
		fmt.Fprintf(&out, "  MCP: %d active  |  %d pending  |  %d failed\n", active, pending, failed)
	}
	fmt.Fprintf(&out, "%s%s%s\n\n", utils.Bold+utils.Cyan, divider, utils.Reset)
	out.WriteString("Type a message; /help for commands; Ctrl-C clears input or interrupts a turn; Ctrl-D exits.\n")
	return out.String()
}

func reasoningStatus(runtime application.RuntimeFacade) string {
	if !runtime.ReasoningRequested {
		return "off"
	}
	effort := strings.ToLower(strings.TrimSpace(runtime.ReasoningEffort))
	if effort == "" {
		effort = "provider-default"
	}
	if !runtime.ReasoningAvailable {
		return fmt.Sprintf("degraded (unsupported; effort=%s)", effort)
	}
	return fmt.Sprintf("on (effort=%s)", effort)
}

func (r *repl) drainBackground() {
	for _, n := range r.built.Team.BG.Notifications() {
		fmt.Fprintf(os.Stderr, "[bg] job %s: %s\n", n["id"], n["status"])
	}
	for _, j := range r.built.Team.BG.Drain() {
		fmt.Fprintln(os.Stderr, "[bg] completed:", j)
	}
}

func (r *repl) checkInbox(messages *[]llm.Message) {
	mb := r.built.Team.Bus
	if mb == nil {
		return
	}
	msgs := mb.ReadInbox(r.built.Session.AgentID)
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

func (r *repl) readInbox(agentID string) string {
	if r.built.Team.Bus == nil {
		return "Inbox is unavailable."
	}
	return formatInboxMessages(r.built.Team.Bus.ReadInbox(agentID))
}

func formatInboxMessages(messages []map[string]any) string {
	if len(messages) == 0 {
		return "Inbox is empty."
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Inbox (%d):", len(messages))
	for i, message := range messages {
		from, _ := message["from"].(string)
		msgType, _ := message["type"].(string)
		content, _ := message["content"].(string)
		if from == "" {
			from = "unknown"
		}
		if msgType == "" {
			msgType = "message"
		}
		fmt.Fprintf(&out, "\n  %d. [%s] from %s", i+1, inboxText(msgType, 80), inboxText(from, 80))
		if requestID, ok := message["request_id"].(string); ok && requestID != "" {
			fmt.Fprintf(&out, " request_id=%s", inboxText(requestID, 120))
		}
		if content != "" {
			fmt.Fprintf(&out, ": %s", inboxText(content, 500))
		}
	}
	return out.String()
}

func inboxText(text string, limit int) string {
	text = utils.Truncate(strings.TrimSpace(text), limit)
	quoted := strconv.QuoteToGraphic(text)
	return strings.TrimSuffix(strings.TrimPrefix(quoted, `"`), `"`)
}

func (r *repl) runSearch(ctx context.Context, query string) string {
	if r.built.Runtime.Web == nil {
		return "Web search is unavailable."
	}
	output, err := r.built.Runtime.Web.Search(ctx, query)
	if err != nil {
		return fmt.Sprintf("Search failed: %v", err)
	}
	output = strings.TrimSpace(output)
	if output == "" {
		return "No search results."
	}
	return output
}

func renderHelp() string {
	return strings.TrimSpace(`
Commands:
  Tasks and memory
    /tasks                              Show short-term todos
    /dag                                Show the persistent task DAG and progress
    /task clear                         Hide completed persistent tasks
    /task reset                         Remove all persistent tasks
    /memory                             Show memory statistics

  Sessions and context
    /session                            Show the active session
    /session list                       List sessions
    /session switch <id>                Switch session and rebuild the runtime
    /session new                        Start a fresh session
    /session rename <title>             Rename the active session
    /session archive                    Archive the active session and start a new one
    /compact                            Compact the current conversation now

  Security and approval
    /approval                           Show the effective approval mode
    /approval manual                    Prompt for review-required operations
    /approval safe-auto                 Auto-approve lower-risk reviews; prompt for high risk
    /approval all-auto confirm          Skip approval prompts and diff previews (unsafe)
    /approval reject                    Auto-reject review-required operations
    /approval notify-only               Report reviews without blocking execution
    /approve ...                        Compatibility alias for legacy presets
    /hitl ...                           Compatibility alias for legacy HITL modes
    /permissions                        Show loaded permission rules
    /permissions reload                 Reload permissions.json
    /security                           Show security controls
    /security test-bash <command>       Dry-run Bash policy without executing the command
    /decisions                          Show recent approval decisions

  MCP and web
    /mcp                                Show active, pending, and failed MCP servers
    /mcp pending                        List MCP servers awaiting approval
    /mcp approve <name>                 Approve and start a pending MCP server
    /mcp connect <name> <cmd> [args...] Connect an MCP server for this runtime
    /mcp disconnect <name>              Disconnect an MCP server
    /search <query>                     Run a one-shot web search

  Team
    /team                               List teammates
    /team spawn <name> <role> <prompt>  Spawn a teammate
    /team shutdown <name>               Shut down a teammate
    /team message <name> <content>      Send a message to a teammate
    /team inbox                         Read the lead inbox
    /inbox                              Read the current agent inbox

  Runtime
    /judge                              Toggle LLM-as-Judge
    /usage                              Show token usage
    /help                               Show this help
    /exit, /quit                        Exit

Notes:
  Text without a leading slash is sent to the current agent.
  Approval starts in safe-auto mode; use /approval to inspect the effective mode.
  all-auto requires an explicit "confirm"; hard Bash deny rules and permissions.json still apply.
`)
}

// newCompleter builds Tab completion for slash commands.
// Keep in sync with renderHelp and handleCommand when commands change.
func newCompleter() readline.AutoCompleter {
	return readline.NewPrefixCompleter(
		readline.PcItem("/tasks"),
		readline.PcItem("/dag"),
		readline.PcItem("/task", readline.PcItem("clear"), readline.PcItem("reset")),
		readline.PcItem("/memory"),
		readline.PcItem("/session",
			readline.PcItem("list"),
			readline.PcItem("switch"),
			readline.PcItem("new"),
			readline.PcItem("rename"),
			readline.PcItem("archive"),
		),
		readline.PcItem("/compact"),
		readline.PcItem("/approval",
			readline.PcItem("manual"),
			readline.PcItem("safe-auto"),
			readline.PcItem("all-auto", readline.PcItem("confirm")),
			readline.PcItem("reject"),
			readline.PcItem("notify-only"),
		),
		readline.PcItem("/approve"),
		readline.PcItem("/hitl"),
		readline.PcItem("/permissions", readline.PcItem("reload")),
		readline.PcItem("/security", readline.PcItem("test-bash")),
		readline.PcItem("/decisions"),
		readline.PcItem("/mcp",
			readline.PcItem("pending"),
			readline.PcItem("approve"),
			readline.PcItem("connect"),
			readline.PcItem("disconnect"),
		),
		readline.PcItem("/search"),
		readline.PcItem("/team",
			readline.PcItem("spawn"),
			readline.PcItem("shutdown"),
			readline.PcItem("message"),
			readline.PcItem("inbox"),
		),
		readline.PcItem("/inbox"),
		readline.PcItem("/judge"),
		readline.PcItem("/usage"),
		readline.PcItem("/help"),
		readline.PcItem("/exit"),
		readline.PcItem("/quit"),
	)
}

func (r *repl) handleCommand(ctx context.Context, cmd string, messages *[]llm.Message) bool {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return false
	}
	switch parts[0] {
	case "/help":
		fmt.Println(renderHelp())
	case "/task":
		if len(parts) < 2 {
			fmt.Println("Usage: /task clear|reset")
			fmt.Println("  clear — mark all completed tasks as deleted (hide from /dag)")
			fmt.Println("  reset — remove all tasks and start fresh")
		} else {
			switch parts[1] {
			case "clear":
				fmt.Println(r.built.Tasks.Service.ClearCompleted())
			case "reset":
				r.built.Tasks.Service.Reset()
				fmt.Println("All tasks cleared.")
			default:
				fmt.Printf("Unknown: %s\n", parts[1])
			}
		}
	case "/tasks":
		if r.built.Tasks.Todos != nil {
			fmt.Println(r.built.Tasks.Todos.Render())
		} else {
			fmt.Println("No todos.")
		}
		fmt.Println("(persistent / dependency-tracked tasks are shown via /dag)")
	case "/dag":
		fmt.Println(r.built.Tasks.Service.TopoView())
		if progress := r.built.Tasks.Service.ProgressSummary(); progress != "" {
			fmt.Println(progress)
		}
	case "/memory":
		fmt.Println(r.built.Tasks.Memory.Stats())
	case "/mcp":
		if r.built.Team.MCP == nil {
			fmt.Println("MCP is unavailable.")
			break
		}
		switch {
		case len(parts) == 1:
			fmt.Println(r.built.Team.MCP.Status())
		case parts[1] == "pending" && len(parts) == 2:
			pending := r.built.Team.MCP.ListPending()
			if len(pending) == 0 {
				fmt.Println("No pending MCP servers. Run /mcp to view failures.")
			} else {
				fmt.Printf("Pending MCP servers: %v\n", pending)
				fmt.Println("Use /mcp approve <name> to start a server.")
			}
		case parts[1] == "approve" && len(parts) == 3:
			toolCount, err := r.built.Team.MCP.Approve(ctx, parts[2])
			if err != nil {
				fmt.Printf("Failed to approve MCP server %q: %v\n", parts[2], err)
			} else {
				fmt.Printf("Approved and started MCP server %q (%d tools).\n", parts[2], toolCount)
			}
		case parts[1] == "connect" && len(parts) >= 4:
			toolCount, err := r.built.Team.MCP.Connect(ctx, parts[2], parts[3], parts[4:])
			if err != nil {
				fmt.Printf("Failed to connect MCP server %q: %v\n", parts[2], err)
			} else {
				fmt.Printf("MCP server %q connected (%d tools).\n", parts[2], toolCount)
			}
		case parts[1] == "disconnect" && len(parts) == 3:
			if err := r.built.Team.MCP.Disconnect(parts[2]); err != nil {
				fmt.Printf("Failed to disconnect MCP server %q: %v\n", parts[2], err)
			} else {
				fmt.Printf("MCP server %q disconnected.\n", parts[2])
			}
		default:
			fmt.Println("Usage: /mcp [pending|approve <name>|connect <name> <cmd> [args...]|disconnect <name>]")
		}
	case "/team":
		if len(parts) == 1 {
			fmt.Println(r.built.Team.Mgr.ListAll())
			break
		}
		switch parts[1] {
		case "spawn":
			if len(parts) < 5 {
				fmt.Println("Usage: /team spawn <name> <role> <prompt>")
			} else {
				fmt.Println(r.built.Team.Mgr.Spawn(context.Background(), parts[2], parts[3], strings.Join(parts[4:], " ")))
			}
		case "shutdown":
			if len(parts) < 3 {
				fmt.Println("Usage: /team shutdown <name>")
			} else {
				fmt.Println(r.built.Team.Mgr.ShutdownByName(parts[2]))
			}
		case "message":
			if len(parts) < 4 {
				fmt.Println("Usage: /team message <name> <content>")
			} else {
				fmt.Println(r.built.Team.Bus.Send("lead", parts[2], strings.Join(parts[3:], " "), "message", nil))
			}
		case "inbox":
			fmt.Println(r.readInbox("lead"))
		default:
			fmt.Println(r.built.Team.Mgr.ListAll())
		}
	case "/session":
		if len(parts) < 2 {
			fmt.Printf("Session: %s (%s)\n", r.built.Session.ID, r.built.Session.Title)
			fmt.Println("Usage: /session [list|switch <id>|new|rename <title>|archive]")
		} else {
			switch parts[1] {
			case "list":
				fmt.Println(r.built.Session.Repo.ListSessions())
			case "switch":
				if len(parts) < 3 {
					fmt.Println("Usage: /session switch <id>")
				} else if _, err := r.built.Session.Repo.LoadSessionMeta(parts[2]); err != nil {
					fmt.Printf("Failed to switch session: %v\n", err)
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
					fmt.Println(r.built.Session.Repo.RenameSession(r.built.Session.ID, strings.Join(parts[2:], " ")))
				}
			case "archive":
				if r.built.Tasks.Memory != nil {
					r.built.Tasks.Memory.SaveSessionMemory(r.built.Session.ID, summarizeMessages(*messages))
				}
				if err := r.built.Session.Repo.ArchiveSession(r.built.Session.ID); err != "" {
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
		if r.built.Runtime.Judge.IsEnabled() {
			r.built.Runtime.Judge.SetEnabled(false)
		} else {
			r.built.Runtime.Judge.SetEnabled(true)
		}
		fmt.Printf("Judge: %v\n", r.built.Runtime.Judge.IsEnabled())
	case "/hitl":
		fmt.Println(r.handleApproval(parts))
	case "/inbox":
		fmt.Println(r.readInbox(r.built.Session.AgentID))
	case "/search":
		if len(parts) < 2 {
			fmt.Println("Usage: /search <query>")
		} else {
			fmt.Println(r.runSearch(ctx, strings.Join(parts[1:], " ")))
		}
	case "/permissions":
		if len(parts) > 1 && parts[1] == "reload" && r.built.Security.ReloadPermissions != nil {
			if err := r.built.Security.ReloadPermissions(); err != nil {
				fmt.Println(err)
			} else {
				fmt.Println("Permissions reloaded.")
			}
		} else {
			fmt.Println(r.built.Security.Permissions.Describe())
		}
	case "/usage":
		if r.built.Session.Usage != nil {
			fmt.Println(r.built.Session.Usage.Render())
		} else {
			fmt.Println("Usage tracking not available.")
		}
	case "/security":
		fmt.Println(handleSecurityCommand(cmd, parts, r.built.Security.Permissions))
	case "/approval", "/approve":
		fmt.Println(r.handleApproval(parts))
	case "/decisions":
		if r.built.Security.DecisionLog != nil {
			fmt.Println(r.built.Security.DecisionLog.Render())
		} else {
			fmt.Println("Decision log not available.")
		}
	case "/compact":
		if r.built.Session.Compact == nil {
			fmt.Println("Compaction unavailable.")
		} else {
			before := len(*messages)
			*messages = r.built.Session.Compact(ctx, *messages)
			if len(*messages) < before {
				fmt.Printf("Compacted conversation: %d -> %d messages.\n", before, len(*messages))
			} else {
				fmt.Println("Compaction skipped.")
			}
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

func effectiveApprovalMode(b *application.BuiltRunner) string {
	if b == nil || b.Security.HITL == nil {
		return "unavailable"
	}
	if !b.Security.HITL.IsEnabled() {
		return "all-auto (legacy HITL off)"
	}
	switch b.Security.HITL.Mode() {
	case hitlaudit.HITLModeInteractive:
		return "manual"
	case hitlaudit.HITLModeSafeOnly:
		return "safe-auto"
	case hitlaudit.HITLModeAutoApprove:
		return "all-auto"
	case hitlaudit.HITLModeAutoReject:
		return "reject"
	case hitlaudit.HITLModeNotifyOnly:
		return "notify-only"
	default:
		return "unknown"
	}
}

func (r *repl) handleApproval(parts []string) string {
	if r.built.Security.HITL == nil || r.built.Security.Approval == nil {
		return "Approval controls are unavailable."
	}
	if len(parts) == 1 {
		mode := effectiveApprovalMode(r.built)
		preview := "skipped"
		if (mode == "manual" || mode == "safe-auto") && r.built.Security.Approval.ShouldPreviewDiff() {
			preview = "enabled"
		}
		return fmt.Sprintf("Approval mode: %s\nDiff preview: %s", mode, preview)
	}

	mode := strings.ToLower(parts[1])
	legacy := parts[0] != "/approval"
	switch parts[0] {
	case "/approve":
		switch mode {
		case "off", "reset":
			mode = "manual"
		case "safe":
			mode = "safe-auto"
		case "danger", "all":
			mode = "all-auto"
		default:
			return "Usage: /approve off|safe|danger [confirm] (compatibility alias for /approval)"
		}
	case "/hitl":
		switch mode {
		case "on", "safe-only", "safeonly":
			mode = "safe-auto"
		case "off", "auto-approve", "approve":
			mode = "all-auto"
		case "interactive":
			mode = "manual"
		case "auto-reject":
			mode = "reject"
		case "notify-only", "notify":
			mode = "notify-only"
		default:
			return "Usage: /hitl on|off|interactive|safe-only|auto-approve|auto-reject|notify-only [confirm] (compatibility alias for /approval)"
		}
	}

	if mode == "all-auto" {
		if len(parts) != 3 || strings.ToLower(parts[2]) != "confirm" {
			return "WARNING: all-auto disables approval prompts and skips diff previews.\nHard Bash deny rules and permissions.json remain enforced.\nConfirm with: /approval all-auto confirm"
		}
	} else if len(parts) != 2 {
		return "Usage: /approval manual|safe-auto|all-auto|reject|notify-only"
	}

	r.built.Security.HITL.SetEnabled(true)
	switch mode {
	case "manual":
		r.built.Security.HITL.SetMode(hitlaudit.HITLModeInteractive)
		r.built.Security.Approval.ApplyPreset("manual")
	case "safe-auto":
		r.built.Security.HITL.SetMode(hitlaudit.HITLModeSafeOnly)
		r.built.Security.Approval.ApplyPreset("safe-auto")
	case "all-auto":
		r.built.Security.HITL.SetMode(hitlaudit.HITLModeAutoApprove)
		r.built.Security.Approval.ApplyPreset("all-auto")
	case "reject":
		r.built.Security.HITL.SetMode(hitlaudit.HITLModeAutoReject)
		r.built.Security.Approval.ApplyPreset("manual")
	case "notify-only":
		r.built.Security.HITL.SetMode(hitlaudit.HITLModeNotifyOnly)
		r.built.Security.Approval.ApplyPreset("manual")
	default:
		return "Usage: /approval manual|safe-auto|all-auto|reject|notify-only"
	}

	prefix := ""
	if legacy {
		canonical := "/approval " + mode
		if mode == "all-auto" {
			canonical += " confirm"
		}
		prefix = fmt.Sprintf("Compatibility alias: use %s.\n", canonical)
	}
	if mode == "all-auto" {
		return prefix + "Approval mode: all-auto — prompts disabled and diff previews skipped; hard deny rules still apply."
	}
	return fmt.Sprintf("%sApproval mode: %s", prefix, mode)
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

func handleSecurityCommand(raw string, parts []string, perms *security.Permissions) string {
	if len(parts) == 1 {
		return securityDesc()
	}
	if parts[1] != "test-bash" {
		return fmt.Sprintf("Unknown security command: %s\nUsage: /security test-bash <command>", parts[1])
	}

	rest := strings.TrimSpace(strings.TrimPrefix(raw, "/security"))
	command := strings.TrimSpace(strings.TrimPrefix(rest, "test-bash"))
	if command == "" {
		return "Usage: /security test-bash <command>"
	}

	classification := security.ClassifyCommand(command)
	allowed, needConfirm, policyReason := security.NewDefaultBashPolicy().Validate(command, perms)
	decision := "allow"
	if !allowed {
		decision = "deny"
	} else if needConfirm {
		decision = "confirm"
	}
	if policyReason == "" {
		policyReason = classification.Reason
	}

	return fmt.Sprintf(
		"Command: %s\nRisk: %s\nDecision: %s\nReason: %s",
		command,
		classification.Verdict,
		decision,
		policyReason,
	)
}

func securityDesc() string {
	return `Security Status:
  Bash: whitelist (85 commands) + deny/confirm regexps
  Permissions: rules loaded from permissions.json
  Secrets: output sanitizer active
  Diff: preview available for file writes
  Dry-run: /security test-bash <command>`
}
