package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/session"
	"go-code-agent/internal/usage"
	"strings"
)

// REPL slash-commands.
// handleReplCommand returns (handled, newHistory).
// Session-aware commands may replace the conversation slice entirely.

func handleReplCommand(ctx context.Context, query string, history []llm.Message) (bool, []llm.Message) {
	switch {
	case query == "/compact":
		if len(history) > 1 {
			fmt.Println("[manual compact via /compact]")
			history = autoCompact(ctx, history, system)
		}
		return true, history

	case query == "/tasks":
		fmt.Println(app.TaskMgr().ListAll())
		return true, history

	case query == "/dag":
		fmt.Println(app.DagSched().TopoView())
		return true, history

	case query == "/team":
		fmt.Println(app.TeamMgr().ListAll())
		return true, history

	case query == "/inbox":
		data, _ := json.MarshalIndent(app.Bus().ReadInbox("lead"), "", "  ")
		fmt.Println(string(data))
		return true, history

	case query == "/memory":
		ec, df, de := memStore.GetStats()
		fmt.Printf("  Evergreen (MEMORY.md): %d chars\n", ec)
		fmt.Printf("  Daily files: %d\n", df)
		fmt.Printf("  Daily entries: %d\n", de)
		return true, history

	case query == "/usage":
		// In-process token-usage rollups (by source + by model).
		// The full per-call log lives in memory/usage.jsonl - the
		// Render output points operators at it.
		fmt.Println(usage.Render())
		return true, history

	case strings.HasPrefix(query, "/search "):
		q := strings.TrimSpace(strings.TrimPrefix(query, "/search "))
		if q == "" {
			fmt.Println("Usage: /search <query>")
			return true, history
		}
		results := memStore.HybridSearch(q, 5)
		if len(results) == 0 {
			fmt.Println("  (no results)")
		} else {
			for _, r := range results {
				fmt.Printf("  [%.4f] %s\n    %s\n", r.Score, r.Path, r.Snippet)
			}
		}
		return true, history

	case query == "/mcp":
		fmt.Println(mcpMgr.ListAll())
		return true, history

	case strings.HasPrefix(query, "/mcp connect "):
		// /mcp connect <name> <command> [args...]
		parts := strings.Fields(strings.TrimPrefix(query, "/mcp connect "))
		if len(parts) < 2 {
			fmt.Println("Usage: /mcp connect <name> <command> [args...]")
			return true, history
		}
		name, cmd := parts[0], parts[1]
		args := parts[2:]
		if err := mcpMgr.Connect(name, cmd, args, nil); err != nil {
			fmt.Printf("  Error: %v\n", err)
		} else {
			fmt.Printf("  Connected '%s' (%d tools)\n", name, mcpMgr.ToolCount())
			// Rebuild toolDefs with new MCP tools.
			initTools()
		}
		return true, history

	case strings.HasPrefix(query, "/mcp disconnect "):
		name := strings.TrimSpace(strings.TrimPrefix(query, "/mcp disconnect "))
		fmt.Println(mcpMgr.Disconnect(name))
		// Rebuild toolDefs without disconnected tools.
		initTools()
		return true, history

	// ---- Session commands ----

	case query == "/session" || query == "/sessions":
		if app == nil || app.SessionManager == nil {
			fmt.Println("(session manager not initialized)")
			return true, history
		}
		fmt.Println(app.SessionManager.Render())
		return true, history

	case strings.HasPrefix(query, "/session new"):
		title := strings.TrimSpace(strings.TrimPrefix(query, "/session new"))
		if title == "" {
			title = "New session"
		}
		return true, sessionSwitchTo(history, "", title)

	case strings.HasPrefix(query, "/session switch "):
		id := strings.TrimSpace(strings.TrimPrefix(query, "/session switch "))
		if id == "" {
			fmt.Println("Usage: /session switch <id>")
			return true, history
		}
		return true, sessionSwitchTo(history, id, "")

	case strings.HasPrefix(query, "/session rename "):
		rest := strings.TrimSpace(strings.TrimPrefix(query, "/session rename "))
		// Accept "<id> <title...>" or just "<title...>" (renames active).
		var id, title string
		if sp := strings.IndexByte(rest, ' '); sp > 0 {
			first := rest[:sp]
			// If first token looks like an id that exists, treat as id.
			if app != nil && app.SessionManager != nil && hasSessionID(app.SessionManager, first) {
				id = first
				title = strings.TrimSpace(rest[sp+1:])
			}
		}
		if id == "" {
			if app != nil && app.SessionManager.Active() != nil {
				id = app.SessionManager.Active().ID()
			}
			title = rest
		}
		if id == "" || title == "" {
			fmt.Println("Usage: /session rename [<id>] <new-title>")
			return true, history
		}
		if err := app.SessionManager.Rename(id, title); err != nil {
			fmt.Printf("  Error: %v\n", err)
		} else {
			fmt.Printf("  Renamed %s -> %s\n", id, title)
		}
		return true, history

	case strings.HasPrefix(query, "/session archive"):
		id := strings.TrimSpace(strings.TrimPrefix(query, "/session archive"))
		if id == "" {
			if app != nil && app.SessionManager.Active() != nil {
				id = app.SessionManager.Active().ID()
			}
		}
		if id == "" {
			fmt.Println("Usage: /session archive [<id>]   (default: active)")
			return true, history
		}
		wasActive := app != nil && app.SessionManager.Active() != nil && app.SessionManager.Active().ID() == id
		if err := app.SessionManager.Archive(id); err != nil {
			fmt.Printf("  Error: %v\n", err)
			return true, history
		}
		fmt.Printf("  Archived session %s\n", id)
		// If we archived the active session, switch to the most recent
		// remaining one (or create a new one if none remain).
		if wasActive {
			next := app.SessionManager.MostRecentActiveID()
			if next == "" {
				return true, sessionSwitchTo(history, "", "New session")
			}
			return true, sessionSwitchTo(history, next, "")
		}
		return true, history

	case query == "/session help" || query == "/session ?":
		fmt.Println("Session commands:")
		fmt.Println("  /session                        list sessions")
		fmt.Println("  /session new [title]            create and switch to a new session")
		fmt.Println("  /session switch <id>            switch to an existing session")
		fmt.Println("  /session rename [<id>] <title>  rename a session (default: active)")
		fmt.Println("  /session archive [<id>]         archive a session (default: active)")
		return true, history

	// ---- Security commands ----

	case query == "/approve" || query == "/approve status":
		autoSafe := globalApproval.IsAutoApproveSafe()
		autoAll := globalApproval.IsAutoApproveAll()
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
		for _, meta := range toolSecurityMap {
			fmt.Printf("  %-20s %s\n", meta.Name, meta.Level)
		}
		return true, history

	case query == "/approve safe":
		globalApproval.SetAutoApproveSafe(true)
		globalApproval.SetAutoApproveAll(false)
		fmt.Println("\u2705 Auto-approve ENABLED for safe-level tools (write, edit, task ops).")
		fmt.Println("   Danger-level tools (bash, delete) still require confirmation.")
		return true, history

	case query == "/approve danger" || query == "/approve all":
		globalApproval.SetAutoApproveAll(true)
		globalApproval.SetAutoApproveSafe(true)
		fmt.Println("\u26a0\ufe0f Auto-approve ALL enabled - including bash/delete/force-push!")
		fmt.Println("   The agent will execute any tool without asking. Use with caution!")
		fmt.Println("   Run '/approve off' to re-enable safety.")
		return true, history

	case query == "/approve off" || query == "/approve reset":
		globalApproval.SetAutoApproveSafe(false)
		globalApproval.SetAutoApproveAll(false)
		fmt.Println("\U0001f6e1 Security: all approvals set to manual mode.")
		fmt.Println("   Safe and dangerous tools will require explicit confirmation.")
		return true, history

	case query == "/security" || query == "/security status":
		fmt.Println("--- Security Status ---")
		autoSafe := globalApproval.IsAutoApproveSafe()
		autoAll := globalApproval.IsAutoApproveAll()
		fmt.Printf("Approval: safe=%v, all=%v\n", autoSafe, autoAll)
		fmt.Printf("Bash policy: %d allowed commands\n", len(DefaultBashPolicy.AllowCommands))
		fmt.Printf("Danger patterns: %d rules\n", len(DefaultBashPolicy.DangerPatterns))
		fmt.Printf("Require-confirm patterns: %d rules\n", len(DefaultBashPolicy.RequireConfirm))
		fmt.Printf("Path sandbox: symlink resolution ENABLED\n")
		fmt.Printf("Secrets sanitizer: %d patterns loaded\n", len(secretsSanitizer.patterns))
		return true, history

	case strings.HasPrefix(query, "/security test-bash "):
		testCmd := strings.TrimSpace(strings.TrimPrefix(query, "/security test-bash "))
		if testCmd == "" {
			fmt.Println("Usage: /security test-bash <command>")
			return true, history
		}
		allowed, needConfirm, reason := DefaultBashPolicy.Validate(testCmd)
		fmt.Printf("Command: '%s'\n", testCmd)
		fmt.Printf("Allowed:     %v\n", allowed)
		fmt.Printf("NeedConfirm: %v\n", needConfirm)
		if reason != "" {
			fmt.Printf("Reason:      %s\n", reason)
		}
		return true, history
	}

	return false, history
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
	if app == nil || app.SessionManager == nil {
		fmt.Println("(session manager not initialized)")
		return oldHistory
	}

	// Deactivate current, if any.
	if app.SessionManager.Active() != nil {
		shutdownTeammates()
		app.SessionManager.Deactivate(app.SessionManager.Active())
	}

	var next *session.Session
	var err error
	if id == "" {
		// Create a fresh session.
		next, err = app.SessionManager.NewSession(newTitle)
		if err != nil {
			fmt.Printf("  Error: %v\n", err)
			return oldHistory
		}
	} else {
		next, err = app.SessionManager.LoadSession(id)
		if err != nil {
			fmt.Printf("  Error: %v\n", err)
			return oldHistory
		}
	}

	app.SessionManager.Activate(next)

	// Wire TeammateManager for the new session.
	app.SetTeamMgr(NewTeamMgr(
		next.TeamDir(), next.Bus, next.TaskMgr, next.DagSched,
		next.TasksDir(), next.Protocols,
	))

	// Rebuild system prompt for the new session.
	system = buildSystemPrompt("")
	app.System = system

	conv := bootConversation(next, system)
	fmt.Printf("  Switched to session %s - %s\n", next.ID(), next.Title())
	return conv
}
