package log

import (
	"fmt"
	"go-code-agent/utils"
	"os"
)

// Unified output functions - consistent colored terminal output

const (
	ColorReset  = "\033[0m"
	ColorGreen  = "\033[32m"
	ColorYellow = "\033[33m"
	ColorCyan   = "\033[36m"
	ColorRed    = "\033[31m"
	ColorWhite  = "\033[97m"
	ColorBold   = "\033[1m"
	ColorDim    = "\033[90m"
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
