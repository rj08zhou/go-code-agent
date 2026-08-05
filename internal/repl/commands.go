package repl

import (
	"context"
	"fmt"
	"strings"

	"go-code-agent/internal/llm"
)

func (r *Loop) handleCommand(ctx context.Context, cmd string, messages *[]llm.Message) bool {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return false
	}
	switch parts[0] {
	case "/help":
		fmt.Println(renderHelp())
	case "/task", "/tasks", "/dag", "/memory":
		r.handleTaskCommand(parts)
	case "/mcp":
		r.handleMCPCommand(ctx, parts)
	case "/team", "/inbox":
		r.handleTeamCommand(parts)
	case "/session":
		return r.handleSessionCommand(parts, messages)
	case "/judge", "/usage", "/compact":
		r.handleRuntimeCommand(ctx, parts, messages)
	case "/search":
		r.handleSearchCommand(ctx, parts)
	case "/permissions", "/security", "/decisions":
		r.handleSecurityCommands(cmd, parts)
	case "/approval":
		fmt.Println(r.handleApproval(parts))
	case "/exit", "/quit":
		fmt.Println("Goodbye!")
		return true
	default:
		fmt.Printf("Unknown command: %s\n", parts[0])
	}
	return false
}
