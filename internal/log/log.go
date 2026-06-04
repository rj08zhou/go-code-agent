package log

import (
	"fmt"
	"go-code-agent/utils"
	"os"
)

// Unified output functions - consistent colored terminal output

const (
	ColorReset   = "\033[0m"
	ColorGreen   = "\033[32m"
	ColorYellow  = "\033[33m"
	ColorCyan    = "\033[36m"
	ColorRed     = "\033[31m"
	ColorWhite   = "\033[97m"
	ColorBold    = "\033[1m"
	ColorDim     = "\033[90m"
	ColorMagenta = "\033[35m"
)

// PrintAgent prints the LLM's final response in green.
func PrintAgent(msg string) {
	fmt.Printf("%s%s%s\n", ColorGreen, msg, ColorReset)
}

// PrintAgentBegin starts streaming output in green (no newline).
func PrintAgentBegin() {
	fmt.Print(ColorGreen)
}

// PrintAgentDelta prints a streaming text delta (no newline, no color change).
func PrintAgentDelta(delta string) {
	fmt.Print(delta)
}

// PrintAgentEnd finishes streaming output with color reset and newline.
func PrintAgentEnd() {
	fmt.Println(ColorReset)
}

// PrintTool prints a tool execution result.
func PrintTool(name, output string) {
	fmt.Printf("%s> %s%s: %s\n", ColorYellow, name, ColorReset, utils.Truncate(output, 200))
}

// PrintTeamTool prints a teammate's tool execution result.
func PrintTeamTool(teammate, name, output string) {
	fmt.Printf("  %s[%s] %s%s: %s\n", ColorDim, teammate, name, ColorReset, utils.Truncate(output, 120))
}

// PrintSubTool prints a subagent's tool execution result.
func PrintSubTool(name, output string) {
	fmt.Printf("  %s[subagent] %s%s: %s\n", ColorCyan, name, ColorReset, utils.Truncate(output, 120))
}

// PrintSystem prints an informational system message in dim.
func PrintSystem(msg string) {
	fmt.Printf("%s  %s%s\n", ColorDim, msg, ColorReset)
}

// PrintError prints an error message to stderr in red.
func PrintError(msg string) {
	fmt.Fprintf(os.Stderr, "%sError: %s%s\n", ColorRed, msg, ColorReset)
}

// decisionSink, if set, receives every decision event in addition to the
// terminal print. The cmd layer wires this to persist a replayable timeline
// (decisions.jsonl) without coupling this package to session/file internals.
var decisionSink func(kind, summary string)

// SetDecisionSink installs a callback invoked for every PrintDecision call.
// Pass nil to disable persistence. Safe to call once at startup.
func SetDecisionSink(fn func(kind, summary string)) {
	decisionSink = fn
}

// PrintDecision prints an autonomous-decision event in a distinct, scannable
// format. These are actions the agent (or the loop) takes on its own —
// planning, context compaction, memory writes, self-evaluation, reflection —
// that the user would otherwise not see clearly. Surfacing them gives the user
// a single, consistent "what did the agent decide on its own" timeline.
//
// In addition to printing, the event is forwarded to the decisionSink (if set)
// so it can be persisted for after-the-fact replay (e.g. via /decisions).
//
// kind is a short stable tag, e.g. "plan", "context", "memory", "judge",
// "reflect". summary is a one-line human description of the decision.
func PrintDecision(kind, summary string) {
	fmt.Printf("%s%s◆ [%s]%s %s\n", ColorBold, ColorMagenta, kind, ColorReset, summary)
	if decisionSink != nil {
		decisionSink(kind, summary)
	}
}
