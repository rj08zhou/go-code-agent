package agent

import "strings"

// BuildSessionContext formats dynamic session state for a per-Run user message.
// Kept out of the system prompt so the static prefix stays cacheable and so
// task/memory/MCP snapshots refresh every turn instead of freezing at session start.
func BuildSessionContext(evergreen, tasks, mcp string) string {
	var parts []string
	if s := strings.TrimSpace(evergreen); s != "" {
		parts = append(parts, "## Evergreen memory\n"+s)
	}
	if s := strings.TrimSpace(tasks); s != "" {
		parts = append(parts,
			"## Open tasks\nOnly resume these if the user explicitly asks; otherwise ignore them.\n"+s)
	}
	if s := strings.TrimSpace(mcp); s != "" && s != "No MCP servers configured." {
		parts = append(parts, "## MCP\n"+s)
	}
	if len(parts) == 0 {
		return ""
	}
	return "<session-context>\n" + strings.Join(parts, "\n\n") + "\n</session-context>"
}
