package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/internal/agent"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/security"
	"go-code-agent/internal/session"
	"go-code-agent/internal/usage"
	"slices"
	"strings"
)

// REPL slash-commands.
//
// Commands are self-registered (see the init() at the bottom of this
// file) into replCommands, mirroring the same registry pattern already
// used for LLM providers (internal/llm/provider.go, via init()) and
// the tool registry (agent.ToolSpec, see internal/agent/tool_base.go).
//
// Before this, handleReplCommand was a single ~270-line switch
// statement: every new command required editing this one function in
// the middle of an unrelated block, and there was no way to express
// "this command's logic lives elsewhere" — everything had to be
// inlined into the switch body. Registration makes each command an
// independent, self-contained entry (a match predicate + a handler),
// so adding one is an isolated append instead of a squeeze into a
// growing switch.
//
// handleReplCommand returns (handled, newHistory). Session-aware
// commands may replace the conversation slice entirely.

// replHandler runs a matched command and returns (handled, newHistory).
type replHandler func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message)

// replCommand pairs a match predicate with its handler.
type replCommand struct {
	match   func(query string) bool
	handler replHandler
}

// replCommands holds every registered command, in registration order.
// handleReplCommand tries them in order and runs the first match —
// same first-match-wins semantics as the switch statement this
// replaced, so registration order still matters when two predicates
// could both match (e.g. an exact "/session" vs a prefix "/session ").
var replCommands []replCommand

// registerReplCommand appends a command to the registry. Called from
// this file's init() below; see the doc comment on replCommands for
// why commands self-register instead of living in one big function.
func registerReplCommand(match func(string) bool, handler replHandler) {
	replCommands = append(replCommands, replCommand{match: match, handler: handler})
}

// exact matches when query is equal to one of the given literals.
func exact(queries ...string) func(string) bool {
	return func(q string) bool { return slices.Contains(queries, q) }
}

// prefix matches when query starts with p.
func prefix(p string) func(string) bool {
	return func(q string) bool { return strings.HasPrefix(q, p) }
}

// handleReplCommand dispatches query to the first registered command
// whose match predicate returns true. Returns (false, history)
// unchanged if nothing matches, signalling the caller should treat
// query as a normal user message instead.
func handleReplCommand(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
	for _, c := range replCommands {
		if c.match(query) {
			return c.handler(ctx, query, history)
		}
	}
	return false, history
}

func init() {
	registerReplCommand(exact("/compact"), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		if len(history) > 1 {
			fmt.Println("[manual compact via /compact]")
			history = agent.AutoCompact(ctx, history, agent.App.System)
		}
		return true, history
	})

	registerReplCommand(exact("/tasks"), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		fmt.Println(agent.App.TaskMgr().ListAll())
		return true, history
	})

	registerReplCommand(exact("/dag"), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		fmt.Println(agent.App.DagSched().TopoView())
		return true, history
	})

	registerReplCommand(exact("/decisions"), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		fmt.Println(agent.RenderDecisions())
		return true, history
	})

	registerReplCommand(exact("/team"), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		fmt.Println(agent.App.TeamMgr().ListAll())
		return true, history
	})

	registerReplCommand(exact("/inbox"), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		data, _ := json.MarshalIndent(agent.App.Bus().ReadInbox("lead"), "", "  ")
		fmt.Println(string(data))
		return true, history
	})

	registerReplCommand(exact("/memory"), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		ec, df, de := agent.App.MemStore.GetStats()
		fmt.Printf("  Evergreen (MEMORY.md): %d chars\n", ec)
		fmt.Printf("  Daily files: %d\n", df)
		fmt.Printf("  Daily entries: %d\n", de)
		return true, history
	})

	registerReplCommand(exact("/usage"), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		// In-process token-usage rollups (by source + by model).
		// The full per-call log lives in memory/usage.jsonl - the
		// Render output points operators at it.
		fmt.Println(usage.Render())
		return true, history
	})

	registerReplCommand(prefix("/search "), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		q := strings.TrimSpace(strings.TrimPrefix(query, "/search "))
		if q == "" {
			fmt.Println("Usage: /search <query>")
			return true, history
		}
		results := agent.App.MemStore.HybridSearch(q, 5)
		if len(results) == 0 {
			fmt.Println("  (no results)")
		} else {
			for _, r := range results {
				fmt.Printf("  [%.4f] %s\n    %s\n", r.Score, r.Path, r.Snippet)
			}
		}
		return true, history
	})

	registerReplCommand(exact("/mcp"), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		fmt.Println(agent.App.MCPMgr.ListAll())
		return true, history
	})

	registerReplCommand(prefix("/mcp connect "), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		// /mcp connect <name> <command> [args...]
		parts := strings.Fields(strings.TrimPrefix(query, "/mcp connect "))
		if len(parts) < 2 {
			fmt.Println("Usage: /mcp connect <name> <command> [args...]")
			return true, history
		}
		name, cmd := parts[0], parts[1]
		args := parts[2:]
		if err := agent.App.MCPMgr.Connect(name, cmd, args, nil); err != nil {
			fmt.Printf("  Error: %v\n", err)
		} else {
			fmt.Printf("  Connected '%s' (%d tools)\n", name, agent.App.MCPMgr.ToolCount())
			// Rebuild ToolDefs with new MCP tools.
			agent.InitTools()
		}
		return true, history
	})

	registerReplCommand(prefix("/mcp disconnect "), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		name := strings.TrimSpace(strings.TrimPrefix(query, "/mcp disconnect "))
		fmt.Println(agent.App.MCPMgr.Disconnect(name))
		// Rebuild ToolDefs without disconnected tools.
		agent.InitTools()
		return true, history
	})

	// ---- Session commands ----

	registerReplCommand(exact("/session", "/sessions"), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		if agent.App == nil || agent.App.SessionManager == nil {
			fmt.Println("(session manager not initialized)")
			return true, history
		}
		fmt.Println(agent.App.SessionManager.Render())
		return true, history
	})

	registerReplCommand(prefix("/session new"), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		title := strings.TrimSpace(strings.TrimPrefix(query, "/session new"))
		if title == "" {
			title = "New session"
		}
		return true, sessionSwitchTo(history, "", title)
	})

	registerReplCommand(prefix("/session switch "), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		id := strings.TrimSpace(strings.TrimPrefix(query, "/session switch "))
		if id == "" {
			fmt.Println("Usage: /session switch <id>")
			return true, history
		}
		return true, sessionSwitchTo(history, id, "")
	})

	registerReplCommand(prefix("/session rename "), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		rest := strings.TrimSpace(strings.TrimPrefix(query, "/session rename "))
		// Accept "<id> <title...>" or just "<title...>" (renames active).
		var id, title string
		if sp := strings.IndexByte(rest, ' '); sp > 0 {
			first := rest[:sp]
			// If first token looks like an id that exists, treat as id.
			if agent.App != nil && agent.App.SessionManager != nil && hasSessionID(agent.App.SessionManager, first) {
				id = first
				title = strings.TrimSpace(rest[sp+1:])
			}
		}
		if id == "" {
			id = agent.App.ActiveSessionID()
			title = rest
		}
		if id == "" || title == "" {
			fmt.Println("Usage: /session rename [<id>] <new-title>")
			return true, history
		}
		if err := agent.App.SessionManager.Rename(id, title); err != nil {
			fmt.Printf("  Error: %v\n", err)
		} else {
			fmt.Printf("  Renamed %s -> %s\n", id, title)
		}
		return true, history
	})

	registerReplCommand(prefix("/session archive"), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		id := strings.TrimSpace(strings.TrimPrefix(query, "/session archive"))
		if id == "" {
			id = agent.App.ActiveSessionID()
		}
		if id == "" {
			fmt.Println("Usage: /session archive [<id>]   (default: active)")
			return true, history
		}
		wasActive := agent.App.ActiveSessionID() == id
		if err := agent.App.SessionManager.Archive(id); err != nil {
			fmt.Printf("  Error: %v\n", err)
			return true, history
		}
		fmt.Printf("  Archived session %s\n", id)
		// If we archived the active session, switch to the most recent
		// remaining one (or create a new one if none remain).
		if wasActive {
			next := agent.App.SessionManager.MostRecentActiveID()
			if next == "" {
				return true, sessionSwitchTo(history, "", "New session")
			}
			return true, sessionSwitchTo(history, next, "")
		}
		return true, history
	})

	registerReplCommand(exact("/session help", "/session ?"), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		fmt.Println("Session commands:")
		fmt.Println("  /session                        list sessions")
		fmt.Println("  /session new [title]            create and switch to a new session")
		fmt.Println("  /session switch <id>            switch to an existing session")
		fmt.Println("  /session rename [<id>] <title>  rename a session (default: active)")
		fmt.Println("  /session archive [<id>]         archive a session (default: active)")
		return true, history
	})

	// ---- Security commands ----

	registerReplCommand(exact("/approve", "/approve status"), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		autoSafe := security.GlobalApproval.IsAutoApproveSafe()
		autoAll := security.GlobalApproval.IsAutoApproveAll()
		fmt.Println("Security approval status:")
		fmt.Printf("  Auto-approve safe:   %v\n", autoSafe)
		fmt.Printf("  Auto-approve all:    %v (dangerous!)\n", autoAll)
		fmt.Println()
		fmt.Println("Usage:")
		fmt.Println("  /approve safe        enable auto-approval for safe-level tools (write_file, edit_file, etc.)")
		fmt.Println("  /approve danger      enable auto-approval for ALL tools including dangerous ones (!)")
		fmt.Println("  /approve off         reset to manual confirmation for everything")
		fmt.Println()
		fmt.Println("Tool security levels:")
		for _, meta := range agent.ToolSecurityMap {
			fmt.Printf("  %-20s %s\n", meta.Name, meta.Level)
		}
		return true, history
	})

	registerReplCommand(exact("/approve safe"), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		security.GlobalApproval.SetAutoApproveSafe(true)
		security.GlobalApproval.SetAutoApproveAll(false)
		fmt.Println("\u2705 Auto-approve ENABLED for safe-level tools (write, edit, task ops).")
		fmt.Println("   Danger-level tools (bash, delete) still require confirmation.")
		return true, history
	})

	registerReplCommand(exact("/approve danger", "/approve all"), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		security.GlobalApproval.SetAutoApproveAll(true)
		security.GlobalApproval.SetAutoApproveSafe(true)
		fmt.Println("\u26a0\ufe0f Auto-approve ALL enabled - including bash/delete/force-push!")
		fmt.Println("   The agent will execute any tool without asking. Use with caution!")
		fmt.Println("   Run '/approve off' to re-enable safety.")
		return true, history
	})

	registerReplCommand(exact("/approve off", "/approve reset"), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		security.GlobalApproval.SetAutoApproveSafe(false)
		security.GlobalApproval.SetAutoApproveAll(false)
		fmt.Println("\U0001f6e1 Security: all approvals set to manual mode.")
		fmt.Println("   Safe and dangerous tools will require explicit confirmation.")
		return true, history
	})

	registerReplCommand(exact("/security", "/security status"), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		fmt.Println("--- Security Status ---")
		autoSafe := security.GlobalApproval.IsAutoApproveSafe()
		autoAll := security.GlobalApproval.IsAutoApproveAll()
		fmt.Printf("Approval: safe=%v, all=%v\n", autoSafe, autoAll)
		fmt.Printf("Bash policy: %d allowed commands\n", len(security.DefaultBashPolicy.AllowCommands))
		fmt.Printf("Danger patterns: %d rules\n", len(security.DefaultBashPolicy.DangerPatterns))
		fmt.Printf("Require-confirm patterns: %d rules\n", len(security.DefaultBashPolicy.RequireConfirm))
		fmt.Printf("Path sandbox: symlink resolution ENABLED\n")
		fmt.Printf("Secrets sanitizer: %d patterns loaded\n", security.GlobalSecretsSanitizer.PatternCount())
		return true, history
	})

	registerReplCommand(prefix("/security test-bash "), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		testCmd := strings.TrimSpace(strings.TrimPrefix(query, "/security test-bash "))
		if testCmd == "" {
			fmt.Println("Usage: /security test-bash <command>")
			return true, history
		}
		allowed, needConfirm, reason := security.DefaultBashPolicy.Validate(testCmd)
		fmt.Printf("Command: '%s'\n", testCmd)
		fmt.Printf("Allowed:     %v\n", allowed)
		fmt.Printf("NeedConfirm: %v\n", needConfirm)
		if reason != "" {
			fmt.Printf("Reason:      %s\n", reason)
		}
		return true, history
	})

	registerReplCommand(exact("/permissions", "/permissions status"), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		fmt.Println(security.GlobalPermissions.Describe())
		return true, history
	})

	registerReplCommand(exact("/permissions reload"), func(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
		path := security.GlobalPermissions.Path()
		if path == "" {
			fmt.Println("No permissions file path known yet (agent not fully initialized).")
			return true, history
		}
		warn, err := security.GlobalPermissions.Load(path)
		if err != nil {
			fmt.Printf("Reload failed: %v (previous rules kept)\n", err)
			return true, history
		}
		if warn != "" {
			fmt.Println("[permissions] " + warn)
		}
		fmt.Printf("Reloaded %d rule(s) from %s\n", security.GlobalPermissions.Count(), path)
		return true, history
	})
}

// hasSessionID is a helper for /session rename arg disambiguation.
func hasSessionID(sm *session.SessionManager, id string) bool {
	for _, s := range sm.List() {
		if s.ID == id {
			return true
		}
	}
	return false
}

// sessionSwitchTo performs a full session swap and returns a fresh conversation.
func sessionSwitchTo(oldHistory []llm.Message, id, newTitle string) []llm.Message {
	if agent.App == nil || agent.App.SessionManager == nil {
		fmt.Println("(session manager not initialized)")
		return oldHistory
	}

	// Deactivate current, if any.
	agent.App.DeactivateActiveSession()

	var next *session.Session
	var err error
	if id == "" {
		// Create a fresh session.
		next, err = agent.App.SessionManager.NewSession(newTitle)
		if err != nil {
			fmt.Printf("  Error: %v\n", err)
			return oldHistory
		}
	} else {
		next, err = agent.App.SessionManager.LoadSession(id)
		if err != nil {
			fmt.Printf("  Error: %v\n", err)
			return oldHistory
		}
	}

	agent.App.ActivateSession(next)

	// An explicit /session switch is a deliberate user action, so we do
	// not arm the startup resume-boundary note here (that guards only
	// the auto-resume-on-launch case); the restored-history flag is
	// intentionally discarded.
	conv, _ := bootConversation(next, agent.App.System)
	fmt.Printf("  Switched to session %s - %s\n", next.ID(), next.Title())
	return conv
}
