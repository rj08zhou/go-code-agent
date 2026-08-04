package repl

import (
	"fmt"
	"strings"

	"go-code-agent/internal/application"
	"go-code-agent/internal/llm"
)

// handleSessionCommand returns true when the current REPL should exit and rebuild
// using r.next (switch, new, or successful archive).
func (r *Loop) handleSessionCommand(parts []string, messages *[]llm.Message) bool {
	if len(parts) < 2 {
		fmt.Printf("Session: %s (%s)\n", r.built.Session.ID, r.built.Session.Title)
		fmt.Println("Usage: /session [list|switch <id>|new|rename <title>|archive]")
		return false
	}
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
	return false
}
