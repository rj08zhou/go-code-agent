package utils

const (
	Reset = "\033[0m"

	Bold   = "\033[1m"
	Dim    = "\033[2m"
	Italic = "\033[3m"

	Black   = "\033[30m"
	Red     = "\033[31m"
	Green   = "\033[32m"
	Yellow  = "\033[33m"
	Blue    = "\033[34m"
	Magenta = "\033[35m"
	Cyan    = "\033[36m"
	White   = "\033[37m"

	BoldBlack   = "\033[1;30m"
	BoldRed     = "\033[1;31m"
	BoldGreen   = "\033[1;32m"
	BoldYellow  = "\033[1;33m"
	BoldBlue    = "\033[1;34m"
	BoldMagenta = "\033[1;35m"
	BoldCyan    = "\033[1;36m"
	BoldWhite   = "\033[1;37m"
)

// Color wraps s in ANSI color codes.
func Color(s, code string) string { return code + s + Reset }
