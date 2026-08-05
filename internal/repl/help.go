package repl

import (
	"strings"

	"github.com/chzyer/readline"
)

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

// NewCompleter builds Tab completion for slash commands.
// Keep in sync with renderHelp and handleCommand when commands change.
func NewCompleter() readline.AutoCompleter {
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
