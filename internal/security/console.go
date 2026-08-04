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

type stderrConsole struct{}

func (stderrConsole) WriteInteractive(text string) {
	_, _ = io.WriteString(os.Stderr, text)
}

// DefaultInteractiveConsole provides the standalone fallback used by tests
// and callers that do not have a CLI console to inject.
func DefaultInteractiveConsole() InteractiveConsole { return stderrConsole{} }
