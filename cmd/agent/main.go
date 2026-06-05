// Package main — go-code-agent entry point.
//
// Architecture:
//
//	┌──────────────────────────────────────────────────────────────┐
//	│                      USER INPUT (REPL)                       │
//	│   slash commands → repl_commands.go (short-circuit)          │
//	│   user message  → memoryRecall → buildSystemPrompt           │
//	└──────────────────────────┬───────────────────────────────────┘
//	                           │
//	                           ▼
//	┌──────────────────────────────────────────────────────────────┐
//	│                 ORCHESTRATION (agent_loop.go)                │
//	│                                                              │
//	│  Pre-round:  microCompact → tokenCheck → drain bg/inbox     │
//	│  Gates:      think-gate → planning-gate → write-gate        │
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
//	│  TodoList + think    │  │  subagent         │  │  Protocols      │
//	└─────────────────────┘  └───────────────────┘  └─────────────────┘
//	                                            │
//	                                            ▼
//	┌──────────────────────────────────────────────────────────────┐
//	│                     STORAGE / STATE                          │
//	│  MemoryStore (evergreen + daily, BM25+vector hybrid search) │
//	│  TaskStore (file-persisted JSON) + DAG edges                │
//	│  MessageBus (JSONL inbox) + ProtocolStore (TTL)             │
//	│  HistoryStore (append-only JSONL + checkpoint compaction)   │
//	│  Skills / Prompts (workspace dir) + MCP servers             │
//	└──────────────────────────────────────────────────────────────┘
//
// Single-round execution flow:
//
//	user msg → [preRound] → LLM stream → tool_calls?
//	  yes → for each call: security → HITL → timeout → handler
//	       → [planningGate] → [judge] → [reflect] → next round
//	  no  → [auto-lesson?] → done
//
// This file: parse env/flags, init workdir-global subsystems, pick LLM
// provider, bootstrap session, run REPL loop, handle SIGINT cleanup.
package main

import (
	"context"
	"flag"
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/history"
	"go-code-agent/internal/hitl_audit"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/log"
	"go-code-agent/internal/mcp"
	"go-code-agent/internal/memory"
	"go-code-agent/internal/prompt"
	"go-code-agent/internal/session"
	"go-code-agent/internal/skill"
	"go-code-agent/internal/usage"
	"go-code-agent/utils"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/chzyer/readline"
)

// Process-wide globals (unchanged across session switches).
// Per-session subsystems live on the active Session; access via app.XXX().
var (
	model   string
	workdir string
	system  string

	skills   *skill.SkillLoader
	memStore *memory.MemoryStore
	mcpMgr   *mcp.MCPManager

	toolDefs     []llm.ToolDef
	toolHandlers map[string]ToolHandler
)

func main() {
	// CLI flags
	sessionFlag := flag.String("session", "", "activate a specific session id")
	newSessionFlag := flag.Bool("new-session", false, "start a brand-new session instead of resuming the latest")
	humanFlag := flag.Bool("human", false, "enable Human-in-the-loop approval for high-risk tool calls")
	humanModeFlag := flag.String("human-mode", infra.HitlDefaultMode, "hitl mode: interactive | auto-approve | auto-reject | notify-only")

	flag.Parse()

	model = os.Getenv("MODEL_ID")
	if model == "" {
		model = "claude-opus-4.7"
	}
	workdir, _ = os.Getwd()

	// ---- Workdir-global subsystems (shared across sessions) ----
	skills = skill.NewSkillLoader(utils.JoinWorkdir(workdir, "skills"))
	if skills.Len() == 0 {
		log.PrintSystem("Warning: no skills loaded from " + utils.JoinWorkdir(workdir, "skills"))
	}
	memStore = memory.NewMemoryStore(workdir)
	mcpMgr = mcp.NewMCPManager(workdir)
	mcpMgr.LoadConfig(workdir)

	hitl_audit.InitHITLAudit(workdir)
	usage.InitUsageRecorder(workdir)
	llm.SetUsageRecorder(usage.Record)
	usage.SetSessionIDFunc(func() string {
		if app != nil && app.SessionManager != nil && app.SessionManager.Active() != nil {
			return app.SessionManager.Active().ID()
		}
		return ""
	})
	defer mcpMgr.DisconnectAll()

	// Saga snapshot/rollback (opt-in via env).
	if os.Getenv("SNAPSHOT_ENABLED") == "1" {
		globalSnapshot.Enable()
		log.PrintSystem("[snapshot] enabled (write tools wrapped with git-stash rollback)")
	}

	// ---- LLM provider ----
	prov, err := llm.PickProvider(model)
	if err != nil {
		log.PrintError(fmt.Sprintf("could not select LLM provider: %v", err))
		os.Exit(1)
	}
	llm.SetProvider(prov)
	log.PrintSystem(fmt.Sprintf("[llm-throttle] %s", llm.DescribeLimiter()))

	// ---- Session ----
	promptsDir := utils.JoinWorkdir(workdir, "prompts")
	promptLoader := prompt.NewLoader(promptsDir)
	hitl_audit.SetPromptLoader(promptLoader)
	sessionManager := session.NewSessionManager(workdir, model, promptLoader, memStore, DefaultBashPolicy.Validate)

	explicitID := *sessionFlag
	if *newSessionFlag && explicitID == "" {
		newSession, err := sessionManager.NewSession("New session")
		if err != nil {
			log.PrintError(fmt.Sprintf("could not create session: %v", err))
			os.Exit(1)
		}
		explicitID = newSession.ID()
	}

	session, err := sessionManager.BootstrapSession(explicitID)
	if err != nil {
		log.PrintError(fmt.Sprintf("could not open session: %v", err))
		os.Exit(1)
	}

	// ---- AppContext (must precede buildSystemPrompt) ----
	app = &AppContext{
		Model: model, Workdir: workdir,
		Skills: skills, MemStore: memStore, MCPMgr: mcpMgr,
		PromptLoader:   promptLoader,
		SessionManager: sessionManager,
	}
	app.SessionManager.Activate(session)

	app.SetTeamMgr(NewTeamMgr(
		session.TeamDir(), session.Bus, session.TaskMgr, session.DagSched,
		session.TasksDir(), session.Protocols,
	))

	system = buildSystemPrompt("")
	app.System = system

	initTools()

	// Persist autonomous-decision events to the active session's
	// decisions.jsonl for after-the-fact replay via /decisions.
	initDecisionLog()

	// Persist all Print* calls (agent, tool, system, error, decision…)
	// to the active session's session.log for after-the-fact review.
	initFileLog()

	// ---- Judge + HITL ----
	// The judge is configured entirely via JUDGE_* env vars so its model,
	// endpoint and credentials live in one place (see judgeConfigFromEnv
	// + llm.JudgeProvider) instead of a mix of flags and env vars.
	if enabled, judgeModel, minScore := judgeConfigFromEnv(); enabled {
		globalJudge = NewJudge(true, judgeModel, minScore)
		log.PrintSystem(fmt.Sprintf("[judge] enabled (model=%q, min_score=%d)",
			firstNonEmpty(judgeModel, model), minScore))
	}
	if *humanFlag {
		hitl_audit.HitlManager.SetEnabled(true)
		switch strings.ToLower(*humanModeFlag) {
		case "interactive":
			hitl_audit.HitlManager.SetMode(hitl_audit.HITLModeInteractive)
		case "auto-approve":
			hitl_audit.HitlManager.SetMode(hitl_audit.HITLModeAutoApprove)
		case "auto-reject":
			hitl_audit.HitlManager.SetMode(hitl_audit.HITLModeAutoReject)
		case "notify-only":
			hitl_audit.HitlManager.SetMode(hitl_audit.HITLModeNotifyOnly)
		default:
			log.PrintSystem(fmt.Sprintf("[hitl] unknown mode %q, defaulting to interactive", *humanModeFlag))
			hitl_audit.HitlManager.SetMode(hitl_audit.HITLModeInteractive)
		}
		log.PrintSystem(fmt.Sprintf("[hitl] enabled (mode=%s)", hitl_audit.HitlManager.Mode()))
	}

	// Signal handling.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n[cleaning up...]")
		shutdownFileLog()
		shutdownTeammates()
		if app != nil && app.SessionManager.Active() != nil {
			app.SessionManager.Deactivate(app.SessionManager.Active())
		}
		mcpMgr.DisconnectAll()
		os.Exit(0)
	}()

	// ---- Welcome banner ----
	ec, df, de := memStore.GetStats()
	fmt.Println("============================================================")
	fmt.Printf("  go-code-agent\n")
	fmt.Printf("  Model: %s  |  Provider: %s  |  Workspace: %s\n", model, llm.GetProvider().Name(), workdir)
	fmt.Printf("  Session: %s - %s\n", session.ID(), session.Title())
	fmt.Printf("  Skills: %s\n", skills.Descriptions())
	fmt.Printf("  Memory: evergreen %d chars, %d daily files, %d entries\n", ec, df, de)
	fmt.Printf("  MCP: %d servers, %d tools\n", mcpMgr.ServerCount(), mcpMgr.ToolCount())
	if session.History != nil {
		fmt.Printf("  History: %s (%d entries)\n", session.History.Path(), session.History.WrittenCount())
	}
	fmt.Println("  Commands: /session /compact /tasks /dag /decisions /team /inbox /memory /search <q> /mcp /approve /security /usage")
	fmt.Println("  Type 'q' or 'exit' to quit.")
	fmt.Printf("  Security: bash allowlist ON, path sandbox ON, secrets sanitizer ON\n")
	fmt.Printf("  Judge: %s  |  HITL: %s  |  Preview: %s\n",
		enabledLabel(globalJudge.IsEnabled()),
		enabledLabelWithMode(hitl_audit.HitlManager.IsEnabled(), hitl_audit.HitlManager.Mode().String()),
		enabledLabel(true)) // Preview is always enabled
	fmt.Println("============================================================")
	// Print resume notice if there are unfinished tasks.
	if rc := session.DagSched.ResumeContext(); rc != "" {
		fmt.Println("\n  ⚠ Unfinished tasks detected from previous session.")
		fmt.Println("  Use /dag to view execution plan, or ask the agent to resume.")
	}
	fmt.Println()

	ctx := context.Background()

	conv := bootConversation(session, system)

	rl, err := readline.NewEx(&readline.Config{
		Prompt:          "\033[34m> \033[0m", // Blue ">" prompt
		HistoryFile:     utils.JoinWorkdir(workdir, ".rl-history"),
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
		HistoryLimit:    1000,
	})
	if err != nil {
		log.PrintError(fmt.Sprintf("failed to initialize readline: %v", err))
		os.Exit(1)
	}
	defer rl.Close()

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

		memoryContext := memoryRecall(query)
		system = buildSystemPrompt(memoryContext)
		app.System = system
		conv[0] = llm.SystemMessage(system)

		conv = append(conv, llm.UserMessage(query))
		hs := app.History()
		if hs != nil {
			if err := hs.AppendUser(query); err != nil {
				log.PrintSystem(fmt.Sprintf("[history] write failed: %v", err))
			}
		}

		before := len(conv)
		if err := agentLoop(ctx, &conv); err != nil {
			log.PrintError(err.Error())
			continue
		}
		if hs != nil {
			persistNewMessages(hs, conv[before:])
		}
		if app.SessionManager != nil && app.SessionManager.Active() != nil {
			app.SessionManager.Touch(app.SessionManager.Active().ID())
		}
		fmt.Println()
	}

	shutdownTeammates()
	if app != nil && app.SessionManager.Active() != nil {
		app.SessionManager.Deactivate(app.SessionManager.Active())
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

// bootConversation replays history or returns a fresh [system] slice.
func bootConversation(session *session.Session, systemPrompt string) []llm.Message {
	hs := session.History
	if hs == nil {
		return []llm.Message{llm.SystemMessage(systemPrompt)}
	}
	restored, restoredCount, err := hs.LoadRuntime(systemPrompt)
	if err != nil {
		log.PrintSystem(fmt.Sprintf("[history] load failed: %v - starting fresh", err))
		return []llm.Message{llm.SystemMessage(systemPrompt)}
	}
	if restoredCount > 0 {
		log.PrintSystem(fmt.Sprintf("[history] resumed %d messages from previous session", restoredCount))
		return restored
	}
	if err := hs.AppendSystem(systemPrompt); err != nil {
		log.PrintSystem(fmt.Sprintf("[history] write failed: %v", err))
	}
	return restored
}

// persistNewMessages appends new messages to history. Skips autoCompact's
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
				log.PrintSystem(fmt.Sprintf("[history] write failed: %v", err))
			}
		case llm.RoleTool:
			if err := hs.AppendTool(m.ToolCallID, m.Content); err != nil {
				log.PrintSystem(fmt.Sprintf("[history] write failed: %v", err))
			}
		case llm.RoleUser:
			text := m.Content
			if strings.HasPrefix(text, compactedMarker) {
				skipNextAssistantAck = true
				continue
			}
			if err := hs.AppendUser(text); err != nil {
				log.PrintSystem(fmt.Sprintf("[history] write failed: %v", err))
			}
		case llm.RoleSystem:
			continue
		}
	}
}
