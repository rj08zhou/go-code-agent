// Package main — go-code-agent entry point.
//
// Architecture:
//
//	┌──────────────────────────────────────────────────────────────┐
//	│                      USER INPUT (REPL)                       │
//	│   slash commands → repl_commands.go (short-circuit)          │
//	│   user message  → agent.Run (memory via memory_search tool)  │
//	└──────────────────────────┬───────────────────────────────────┘
//	                           │
//	                           ▼
//	┌──────────────────────────────────────────────────────────────┐
//	│           ORCHESTRATION (internal/agent, loop.go)            │
//	│                                                              │
//	│  Pre-round:  microCompact → tokenCheck → drain bg/inbox     │
//	│  Gates:      think-gate → planning-gate → DAG-dep-gate      │
//	│  Reflection: mini-reflect → strategy-change → stuck         │
//	│  Termination: maxRounds → auto-lesson → judge verify         │
//	└────────────┬─────────────────────────────┬───────────────────┘
//	             │                             │
//	             ▼                             ▼
//	┌────────────────────────┐   ┌─────────────────────────────────┐
//	│  LLM PROVIDERS         │   │  TOOL DISPATCH                  │
//	│  (provider_*.go)       │   │  (tool_registry + MCP)          │
//	│                        │   │                                 │
//	│  openai / anthropic /  │   │  30+ built-in + mcp__* tools    │
//	│  gemini (stub)         │   │  security gate → HITL gate →    │
//	│  + retry (exp backoff) │   │  timeout → snapshot/rollback    │
//	└────────────────────────┘   └──────────────┬──────────────────┘
//	                                            │
//	                     ┌──────────────────────┼──────────────────┐
//	                     ▼                      ▼                  ▼
//	┌─────────────────────┐  ┌───────────────────┐  ┌─────────────────┐
//	│  Planning            │  │  Execution        │  │  Multi-agent    │
//	│  DAGScheduler +      │  │  bash/files/bg    │  │  TeamMgr + Bus  │
//	│  TodoList + think    │  │  subagent         │  │  Worktree isol. │
//	└─────────────────────┘  └───────────────────┘  └─────────────────┘
//	                                            │
//	                                            ▼
//	┌──────────────────────────────────────────────────────────────┐
//	│                     STORAGE / STATE                          │
//	│  workdir  = project root (agent edits, skills, MCP config)   │
//	│  dataDir  = ~/.config/go-code-agent/projects/<hash>/         │
//	│    ├── MemoryStore (evergreen + daily, BM25+vector hybrid)  │
//	│    ├── TaskStore (JSON) + DAG edges                         │
//	│    ├── MessageBus (JSONL inbox) + ProtocolStore (TTL)       │
//	│    ├── HistoryStore (append-only JSONL + checkpoint)        │
//	│    └── BackfillMemory (async session summary on startup)    │
//	└──────────────────────────────────────────────────────────────┘
//
// Single-round execution flow:
//
//	user msg → [preRound] → LLM stream → tool_calls?
//	  yes → for each call: security → HITL → timeout → handler
//	       → [planningGate] → [judge] → [reflect] → next round
//	  no  → [auto-lesson?] → done
//
// This file is the composition root: parse env/flags, resolve workdir/
// dataDir, init subsystems, bootstrap session, run REPL loop.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"go-code-agent/embedded"
	"go-code-agent/infra"
	"go-code-agent/internal/agent"
	"go-code-agent/internal/history"
	"go-code-agent/internal/hitlaudit"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/logging"
	"go-code-agent/internal/security"
	"go-code-agent/internal/session"
	"go-code-agent/internal/usage"
	"go-code-agent/utils"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/chzyer/readline"
)

// resolveWorkdir returns the project working directory (--workdir flag or CWD).
func resolveWorkdir(flagValue string) (string, error) {
	if flagValue == "" {
		return os.Getwd()
	}
	abs, err := filepath.Abs(flagValue)
	if err != nil {
		return "", fmt.Errorf("resolve --workdir %q: %w", flagValue, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("--workdir %q does not exist: %w", flagValue, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("--workdir %q is not a directory", flagValue)
	}
	return abs, nil
}

func main() {
	// CLI flags
	sessionFlag := flag.String("session", "", "activate a specific session id")
	newSessionFlag := flag.Bool("new-session", false, "start a brand-new session instead of resuming the latest")
	humanFlag := flag.Bool("human", false, "enable Human-in-the-loop approval for high-risk tool calls")
	humanModeFlag := flag.String("human-mode", infra.HitlDefaultMode, "hitl mode: interactive | auto-approve | auto-reject | notify-only")
	workdirFlag := flag.String("workdir", "", "project working directory the agent edits and runs commands in (default: current directory)")
	dataDirFlag := flag.String("data-dir", "", "override the persistent state directory (sessions, memory, etc.); default: <user-config>/go-code-agent/projects/<hash>")

	flag.Parse()

	// Surface suspicious config early.
	for _, w := range infra.Cfg.Validate() {
		logging.PrintSystem("[config] " + w)
	}

	model := infra.Cfg.ModelID
	workdir, err := resolveWorkdir(*workdirFlag)
	if err != nil {
		logging.PrintSystem("Error resolving workdir: " + err.Error())
		os.Exit(1)
	}
	dataDir := infra.ResolveDataDir(workdir, *dataDirFlag)

	// ---- Root object: builds all workdir-global subsystems internally.
	logging.PrintSystem("=== go-code-agent ===")
	logging.PrintSystem("Model: " + model)
	logging.PrintSystem("Workdir: " + workdir)
	logging.PrintSystem("DataDir: " + dataDir)
	agent.App = agent.NewApp(model, workdir, dataDir, security.DefaultBashPolicy.Validate)

	// Embedded README for /readme (works from any directory).
	agent.App.Embedded = embedded.Content

	hitlaudit.InitHITLAudit(dataDir)
	hitlaudit.SetPromptLoader(agent.App.PromptLoader)
	usage.InitUsageRecorder(dataDir)
	llm.SetUsageRecorder(usage.Record)
	usage.SetSessionIDFunc(agent.App.ActiveSessionID)
	defer agent.App.MCPMgr.DisconnectAll()

	// Saga snapshot/rollback (opt-in via env).
	if infra.Cfg.SnapshotEnabled {
		agent.App.Snapshot.Enable()
		logging.PrintSystem("[snapshot] enabled (write tools wrapped with git-stash rollback)")
	}

	// ---- LLM provider ----
	prov, err := llm.PickProvider(model)
	if err != nil {
		logging.PrintError(fmt.Sprintf("could not select LLM provider: %v", err))
		os.Exit(1)
	}
	llm.SetProvider(prov)
	logging.PrintSystem(fmt.Sprintf("[llm-throttle] %s", llm.DescribeLimiter()))

	// ---- Session ----
	sess, err := agent.App.SessionManager.BootstrapOrCreate(*newSessionFlag, *sessionFlag)
	if err != nil {
		logging.PrintError(fmt.Sprintf("could not open session: %v", err))
		os.Exit(1)
	}

	agent.App.ActivateSession(sess)

	// Background memory backfill for sessions that were never summarized.
	agent.App.SessionManager.BackfillMemory(sess.ID())

	agent.InitTools()

	// Persist decisions and all Print* output to session files.
	agent.InitDecisionLog()
	agent.InitFileLog()

	// ---- Judge + HITL (both configured via JUDGE_*/--human) ----
	if infra.Cfg.JudgeEnabled {
		agent.App.Judge = agent.NewJudge(true, infra.Cfg.JudgeModel, infra.Cfg.JudgeMinScore, agent.App.PromptLoader)
		logging.PrintSystem(fmt.Sprintf("[judge] enabled (model=%q, min_score=%d)",
			firstNonEmpty(infra.Cfg.JudgeModel, model), infra.Cfg.JudgeMinScore))
	}
	if *humanFlag {
		hitlaudit.HitlManager.SetEnabled(true)
		switch strings.ToLower(*humanModeFlag) {
		case "interactive":
			hitlaudit.HitlManager.SetMode(hitlaudit.HITLModeInteractive)
		case "auto-approve":
			hitlaudit.HitlManager.SetMode(hitlaudit.HITLModeAutoApprove)
		case "auto-reject":
			hitlaudit.HitlManager.SetMode(hitlaudit.HITLModeAutoReject)
		case "notify-only":
			hitlaudit.HitlManager.SetMode(hitlaudit.HITLModeNotifyOnly)
		default:
			logging.PrintSystem(fmt.Sprintf("[hitl] unknown mode %q, defaulting to interactive", *humanModeFlag))
			hitlaudit.HitlManager.SetMode(hitlaudit.HITLModeInteractive)
		}
		logging.PrintSystem(fmt.Sprintf("[hitl] enabled (mode=%s)", hitlaudit.HitlManager.Mode()))
	}

	// Signal handling: first Ctrl+C does lightweight cleanup; second exits.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n[exiting... (Ctrl+C again to force quit)]")
		agent.ShutdownFileLog()
		if agent.App != nil {
			agent.App.DeactivateActiveSession()
			agent.App.MCPMgr.DisconnectAll()
		}
		os.Exit(0)
	}()

	// ---- Welcome banner ----
	ec, df, de := agent.App.MemStore.GetStats()
	fmt.Println("============================================================")
	fmt.Printf("  go-code-agent\n")
	fmt.Printf("  Model: %s  |  Provider: %s  |  Workspace: %s\n", agent.App.Model, llm.GetProvider().Name(), agent.App.Workdir)
	fmt.Printf("  Session: %s - %s\n", sess.ID(), sess.Title())
	fmt.Printf("  Skills: %s\n", agent.App.Skills.Descriptions())
	fmt.Printf("  Memory: evergreen %d chars, %d daily files, %d entries\n", ec, df, de)
	fmt.Printf("  MCP: %d servers, %d tools\n", agent.App.MCPMgr.ServerCount(), agent.App.MCPMgr.ToolCount())
	if sess.History != nil {
		fmt.Printf("  History: %s (%d entries)\n", sess.History.Path(), sess.History.WrittenCount())
	}
	fmt.Println("  Commands: /readme /session /compact /tasks /dag /decisions /team /inbox /memory /search <q> /mcp /approve /security /permissions /usage")
	fmt.Println("  Type 'q' or 'exit' to quit.")
	fmt.Printf("  Security: bash allowlist ON, path sandbox ON, secrets sanitizer ON\n")
	fmt.Printf("  Judge: %s  |  HITL: %s  |  Preview: %s\n",
		enabledLabel(agent.App.Judge.IsEnabled()),
		enabledLabelWithMode(hitlaudit.HitlManager.IsEnabled(), hitlaudit.HitlManager.Mode().String()),
		enabledLabel(true)) // Preview is always enabled
	fmt.Println("============================================================")
	// Print resume notice if there are unfinished tasks.
	if rc := sess.DagSched.ResumeContext(); rc != "" {
		fmt.Println("\n  ⚠ Unfinished tasks detected from previous session.")
		fmt.Println("  Use /dag to view execution plan, or ask the agent to resume.")
	}
	fmt.Println()

	ctx := context.Background()

	conv, resumed := bootConversation(sess, agent.App.System)
	// One-shot boundary note: if we restored prior history, inject a
	// note before the first user message so the model doesn't auto-
	// continue unfinished work from the previous session.
	resumeBoundaryPending := resumed

	const replPrompt = "\033[34m> \033[0m" // Blue ">" prompt
	rl, err := readline.NewEx(&readline.Config{
		Prompt:          replPrompt,
		HistoryFile:     utils.JoinWorkdir(agent.App.DataDir, ".rl-history"),
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
		HistoryLimit:    1000,
	})
	if err != nil {
		logging.PrintError(fmt.Sprintf("failed to initialize readline: %v", err))
		os.Exit(1)
	}
	defer rl.Close()

	// Route interactive confirmation prompts (diff preview, bash confirm,
	// HITL approval) through a cooked-mode read on os.Stdin. Between
	// rl.Readline() calls the terminal is in cooked mode, so a plain
	// ReadString works. We exit raw mode defensively and drain stdin to
	// clear stray \r/\n left by the raw→cooked transition.
	security.ReadLine = func() (string, error) {
		_ = rl.Terminal.ExitRawMode()

		// 1. 启动一个 goroutine 来读取一行
		lineChan := make(chan string, 1)
		errChan := make(chan error, 1)
		go func() {
			reader := bufio.NewReader(os.Stdin)
			line, err := reader.ReadString('\n')
			if err != nil {
				errChan <- err
				return
			}
			lineChan <- strings.TrimSpace(line)
		}()

		// 2. 使用 select 等待结果，并设置超时防止永久阻塞
		select {
		case line := <-lineChan:
			return line, nil
		case err := <-errChan:
			return "", err
		case <-time.After(100 * time.Millisecond): // 等待100ms，可根据需要调整
			// 超时了，没有输入。这里返回空字符串和 nil 错误
			// 你的逻辑可能需要在这里处理“无输入”的情况
			return "", nil
		}
	}

	for {
		query, err := rl.Readline()
		if err != nil {
			// io.EOF or Ctrl+D
			break
		}
		query = strings.TrimSpace(query)
		if query == "" {
			continue
		}
		if strings.ToLower(query) == "q" || strings.ToLower(query) == "exit" {
			break
		}

		if handled, newConv := handleReplCommand(ctx, query, conv); handled {
			conv = newConv
			continue
		}

		// One-shot resume boundary: not persisted, placed before the
		// user's message so the model reads guidance first.
		if resumeBoundaryPending {
			conv = append(conv, llm.UserMessage(resumeBoundaryNote))
			resumeBoundaryPending = false
		}

		conv = append(conv, llm.UserMessage(query))
		hs := agent.App.History()
		if hs != nil {
			if err := hs.AppendUser(query); err != nil {
				logging.PrintSystem(fmt.Sprintf("[history] write failed: %v", err))
			}
		}

		before := len(conv)
		// AutoCompact may replace conv mid-turn; WithPersistedBoundary
		// keeps `before` in sync.
		runCtx := agent.WithPersistedBoundary(ctx, &before)
		if err := agent.Run(runCtx, &conv); err != nil {
			logging.PrintError(err.Error())
			continue
		}
		// Defensive clamp in case any path missed the remap above.
		if before > len(conv) {
			before = len(conv)
		}
		if hs != nil {
			persistNewMessages(hs, conv[before:])
		}
		if id := agent.App.ActiveSessionID(); id != "" {
			agent.App.SessionManager.Touch(id)
		}
		fmt.Println()
	}

	if agent.App != nil {
		agent.App.DeactivateActiveSession()
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func enabledLabel(on bool) string {
	if on {
		return "ON"
	}
	return "OFF"
}

func enabledLabelWithMode(on bool, mode string) string {
	if on {
		return "ON (" + mode + ")"
	}
	return "OFF"
}

// resumeBoundaryNote: injected before the first user message of a restored
// session so the model doesn't auto-continue unfinished work.
const resumeBoundaryNote = "<system>The conversation above was restored from a previous session that may have been interrupted with unfinished work. " +
	"Respond to the user's new message below on its own terms first. " +
	"Do NOT silently pick up or continue any earlier unfinished task: if relevant unfinished work exists, briefly tell the user what it was and ask whether they want to resume it, then wait for their answer.</system>"

// bootConversation replays history or returns a fresh [system] slice.
// The bool reports whether prior messages were restored.
func bootConversation(sess *session.Session, systemPrompt string) ([]llm.Message, bool) {
	hs := sess.History
	if hs == nil {
		return []llm.Message{llm.SystemMessage(systemPrompt)}, false
	}
	restored, restoredCount, err := hs.LoadRuntime(systemPrompt)
	if err != nil {
		logging.PrintSystem(fmt.Sprintf("[history] load failed: %v - starting fresh", err))
		return []llm.Message{llm.SystemMessage(systemPrompt)}, false
	}
	if restoredCount > 0 {
		logging.PrintSystem(fmt.Sprintf("[history] resumed %d messages from previous session", restoredCount))
		return restored, true
	}
	if err := hs.AppendSystem(systemPrompt); err != nil {
		logging.PrintSystem(fmt.Sprintf("[history] write failed: %v", err))
	}
	return restored, false
}

// persistNewMessages appends new messages to history, skipping autoCompact's
// synthetic messages (already recorded as checkpoint).
func persistNewMessages(hs *history.HistoryStore, msgs []llm.Message) {
	skipNextAssistantAck := false
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleAssistant:
			if skipNextAssistantAck {
				skipNextAssistantAck = false
				continue
			}
			if err := hs.AppendAssistant(m.Content, m.ToolCalls); err != nil {
				logging.PrintSystem(fmt.Sprintf("[history] write failed: %v", err))
			}
		case llm.RoleTool:
			if err := hs.AppendTool(m.ToolCallID, m.Content); err != nil {
				logging.PrintSystem(fmt.Sprintf("[history] write failed: %v", err))
			}
		case llm.RoleUser:
			text := m.Content
			if strings.HasPrefix(text, agent.CompactedMarker) {
				skipNextAssistantAck = true
				continue
			}
			if err := hs.AppendUser(text); err != nil {
				logging.PrintSystem(fmt.Sprintf("[history] write failed: %v", err))
			}
		case llm.RoleSystem:
			continue
		}
	}
}
