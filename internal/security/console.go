package security

import (
	"io"
	"os"
)

// InteractiveConsole renders blocking approval and diff-review UI. Prompt
// text is deliberately separate from event.Sink so it is not persisted as an
// audit event.
type InteractiveConsole interface {
	WriteInteractive(string)
}

// InteractiveIO is the terminal surface used by HITL and diff review:
// write prompts, read choices, and detect whether interactive prompting is
// available (TTY).
type InteractiveIO interface {
	InteractiveConsole
	ReadLine(prompt string) (string, error)
	IsTTY() bool
}

type stderrConsole struct{}

func (stderrConsole) WriteInteractive(text string) {
	_, _ = io.WriteString(os.Stderr, text)
}

// DefaultInteractiveConsole provides the standalone fallback used by tests
// and callers that do not have a CLI console to inject.
func DefaultInteractiveConsole() InteractiveConsole { return stderrConsole{} }

type funcInteractiveIO struct {
	write    InteractiveConsole
	readLine func(string) (string, error)
	tty      bool
}

// NewInteractiveIO composes a writer with an explicit line reader. A nil
// writer falls back to stderr; a nil reader falls back to the process-wide
// ReadLine hook (kept for tests and non-CLI callers).
func NewInteractiveIO(write InteractiveConsole, readLine func(string) (string, error), isTTY bool) InteractiveIO {
	if write == nil {
		write = DefaultInteractiveConsole()
	}
	return &funcInteractiveIO{write: write, readLine: readLine, tty: isTTY}
}

// DefaultInteractiveIO uses stderr output, the process-wide ReadLine hook,
// and the real stdin TTY state.
func DefaultInteractiveIO() InteractiveIO {
	return NewInteractiveIO(DefaultInteractiveConsole(), nil, isStdinTTY())
}

func (f *funcInteractiveIO) WriteInteractive(text string) {
	f.write.WriteInteractive(text)
}

func (f *funcInteractiveIO) ReadLine(prompt string) (string, error) {
	if f.readLine != nil {
		return f.readLine(prompt)
	}
	return ReadLine(prompt)
}

func (f *funcInteractiveIO) IsTTY() bool { return f.tty }

func isStdinTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
