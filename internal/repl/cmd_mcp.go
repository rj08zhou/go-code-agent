package repl

import (
	"context"
	"fmt"
)

func (r *Loop) handleMCPCommand(ctx context.Context, parts []string) {
	if r.built.Team.MCP == nil {
		fmt.Println("MCP is unavailable.")
		return
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
}
