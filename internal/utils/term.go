package utils

import (
	"os"

	"github.com/chzyer/readline"
)

var terminalStylesEnabled = terminalColorsAllowed(
	readline.IsTerminal(int(os.Stdout.Fd())),
	readline.IsTerminal(int(os.Stderr.Fd())),
	noColorRequested(),
)

var (
	Reset = terminalStyle("\033[0m")

	Bold   = terminalStyle("\033[1m")
	Dim    = terminalStyle("\033[2m")
	Italic = terminalStyle("\033[3m")

	Black   = terminalStyle("\033[30m")
	Red     = terminalStyle("\033[31m")
	Green   = terminalStyle("\033[32m")
	Yellow  = terminalStyle("\033[33m")
	Blue    = terminalStyle("\033[34m")
	Magenta = terminalStyle("\033[35m")
	Cyan    = terminalStyle("\033[36m")
	White   = terminalStyle("\033[37m")

	BoldBlack   = terminalStyle("\033[1;30m")
	BoldRed     = terminalStyle("\033[1;31m")
	BoldGreen   = terminalStyle("\033[1;32m")
	BoldYellow  = terminalStyle("\033[1;33m")
	BoldBlue    = terminalStyle("\033[1;34m")
	BoldMagenta = terminalStyle("\033[1;35m")
	BoldCyan    = terminalStyle("\033[1;36m")
	BoldWhite   = terminalStyle("\033[1;37m")
)

func terminalColorsAllowed(stdoutTTY, stderrTTY, noColor bool) bool {
	return stdoutTTY && stderrTTY && !noColor
}

func noColorRequested() bool {
	_, present := os.LookupEnv("NO_COLOR")
	return present
}

func terminalStyle(code string) string {
	if !terminalStylesEnabled {
		return ""
	}
	return code
}
