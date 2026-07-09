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
//
// The agent's engine (loop/judge/tools/team/security/...) lives in
// internal/agent - this file is intentionally just the composition
// root: wire dependencies, then hand off to agent.Run per turn.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
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
	"strings"
	"syscall"

	"github.com/chzyer/readline"
)

func main() {
	// CLI flags
	sessionFlag := flag.String("session", "", "activate a specific session id")
	newSessionFlag := flag.Bool("new-session", false, "start a brand-new session instead of resuming the latest")
	humanFlag := flag.Bool("human", false, "enable Human-in-the-loop approval for high-risk tool calls")
	humanModeFlag := flag.String("human-mode", infra.HitlDefaultMode, "hitl mode: interactive | auto-approve | auto-reject | notify-only")

	flag.Parse()

	// All environment-derived settings come from the single infra.Cfg
	// snapshot (parsed once at init); surface any suspicious config up
	// front rather than failing opaquely on the first LLM call.
	for _, w := range infra.Cfg.Validate() {
		logging.PrintSystem("[config] " + w)
	}

	model := infra.Cfg.ModelID
	workdir, _ := os.Getwd()

	// ---- Root object: builds all workdir-global subsystems ----
	// (Skills/MemStore/MCPMgr/PromptLoader/SessionManager) internally.
	agent.App = agent.NewApp(model, workdir, security.DefaultBashPolicy.Validate)

	hitlaudit.InitHITLAudit(workdir)
	hitlaudit.SetPromptLoader(agent.App.PromptLoader)
	usage.InitUsageRecorder(workdir)
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
	// BootstrapOrCreate owns the full "which session do we start with"
	// policy (--new-session override + explicit id > most recent >
	// fresh); main just passes the CLI flags through and exits on error.
	sess, err := agent.App.SessionManager.BootstrapOrCreate(*newSessionFlag, *sessionFlag)
	if err != nil {
		logging.PrintError(fmt.Sprintf("could not open session: %v", err))
		os.Exit(1)
	}

	agent.App.ActivateSession(sess)

	agent.InitTools()

	// Persist autonomous-decision events to the active session's
	// decisions.jsonl for after-the-fact replay via /decisions.
	agent.InitDecisionLog()

	// Persist all Print* calls (agent, tool, system, error, decision…)
	// to the active session's session.log for after-the-fact review.
	agent.InitFileLog()

	// ---- Judge + HITL ----
	// The judge is configured entirely via JUDGE_* env vars (surfaced
	// through infra.Cfg) so its model, endpoint and credentials live in
	// one place (see infra.Config + llm.JudgeProvider) instead of a mix
	// of flags and env vars.
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

	// Signal handling.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n[cleaning up...]")
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
	fmt.Println("  Commands: /session /compact /tasks /dag /decisions /team /inbox /memory /search <q> /mcp /approve /security /permissions /usage")
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
	// When we restored a prior (possibly interrupted) transcript, the
	// model would otherwise read the trailing unfinished task + a small
	// new message like "hello" as a cue to silently plow ahead with the
	// old work. Inject a one-shot boundary note before the FIRST real
	// user message of this process so the model answers the new message
	// on its own terms and *asks* before resuming anything. Deliberately
	// not persisted (see below) so it never lingers into later restarts.
	resumeBoundaryPending := resumed

	const replPrompt = "\033[34m> \033[0m" // Blue ">" prompt
	rl, err := readline.NewEx(&readline.Config{
		Prompt:          replPrompt,
		HistoryFile:     utils.JoinWorkdir(agent.App.Workdir, ".rl-history"),
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
		HistoryLimit:    1000,
	})
	if err != nil {
		logging.PrintError(fmt.Sprintf("failed to initialize readline: %v", err))
		os.Exit(1)
	}
	defer rl.Close()

	// Route every interactive confirmation prompt (diff preview, bash
	// confirm, HITL approval - see internal/security.ReadLine) through a
	// plain cooked-mode line read on os.Stdin.
	//
	// Key fact about chzyer/readline: it does NOT hold the terminal in
	// raw mode for the whole process. Operation.Runes() (what
	// rl.Readline() calls) enters raw mode on entry and defer-exits it
	// on return, so BETWEEN rl.Readline() calls - which is exactly when
	// these confirmation prompts run, during tool execution - the
	// terminal is already back in cooked mode. A normal
	// bufio.Reader.ReadString('\n') therefore behaves correctly: kernel
	// line buffering, echo and CR->LF translation are all active.
	//
	// We still call ExitRawMode() once, defensively, in case some path
	// left it raw. What we must NOT do is re-enter raw mode after the
	// read: that was the "^M on Enter" regression. Leaving the terminal
	// raw between prompts means the next rl.Readline()'s own
	// EnterRawMode (rm.Enter -> MakeRaw) captures the RAW termios as its
	// "cooked" baseline, and its matching defer ExitRawMode then
	// "restores" the terminal to raw - permanently. From then on Enter
	// echoes as ^M and no line is ever assembled. The next rl.Readline()
	// re-enters raw on its own regardless, so leaving cooked here is both
	// correct and self-healing.
	//
	// readline's background ioloop only reads stdin when its own
	// Readline() kicks it (see Terminal.kickChan), so it is idle and not
	// competing for bytes while we read here.
	security.ReadLine = func() (string, error) {
		_ = rl.Terminal.ExitRawMode()
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		return strings.TrimSpace(line), err
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

		// One-shot resume boundary (see resumeBoundaryPending above).
		// Appended to the live conv only - NOT persisted via AppendUser -
		// so it steers just this process and never replays on the next
		// restart. Placed before the user's message so the model reads
		// the guidance first, then the actual query.
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
		if err := agent.Run(ctx, &conv); err != nil {
			logging.PrintError(err.Error())
			continue
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

// resumeBoundaryNote is injected once, before the first user message of
// a process that restored prior-session history, so the model treats
// that first message as a fresh turn instead of a cue to auto-continue
// whatever unfinished task the restored transcript ends on.
const resumeBoundaryNote = "<system>The conversation above was restored from a previous session that may have been interrupted with unfinished work. " +
	"Respond to the user's new message below on its own terms first. " +
	"Do NOT silently pick up or continue any earlier unfinished task: if relevant unfinished work exists, briefly tell the user what it was and ask whether they want to resume it, then wait for their answer.</system>"

// bootConversation replays history or returns a fresh [system] slice.
// The returned bool reports whether prior-session messages were actually
// restored (used to arm the one-shot resume boundary note).
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
