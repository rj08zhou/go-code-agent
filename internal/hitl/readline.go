package hitl

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// ReadLine reads a line from stdin. Replaceable for testing via SetReadLine.
var readLineFn atomicReadLine

type atomicReadLine struct {
	fn atomic.Value // stores func(prompt string) (string, error)
}

func (a *atomicReadLine) Load() func(prompt string) (string, error) {
	v := a.fn.Load()
	if v == nil {
		return defaultReadLine
	}
	return v.(func(string) (string, error))
}

func (a *atomicReadLine) Store(fn func(prompt string) (string, error)) {
	a.fn.Store(fn)
}

var (
	defaultReadLineMu     sync.Mutex
	defaultReadLineSource = os.Stdin
	defaultReadLineReader = bufio.NewReader(os.Stdin)
)

func readLineFrom(reader *bufio.Reader, writer io.Writer, prompt string) (string, error) {
	if _, err := fmt.Fprint(writer, prompt); err != nil {
		return "", err
	}
	line, err := reader.ReadString('\n')
	if err != nil && (len(line) == 0 || !errors.Is(err, io.EOF)) {
		return "", err
	}
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	return line, nil
}

var defaultReadLine = func(prompt string) (string, error) {
	defaultReadLineMu.Lock()
	defer defaultReadLineMu.Unlock()

	if defaultReadLineSource != os.Stdin {
		defaultReadLineSource = os.Stdin
		defaultReadLineReader = bufio.NewReader(os.Stdin)
	}
	return readLineFrom(defaultReadLineReader, os.Stdout, prompt)
}

// ReadLine calls the current ReadLine function.
func ReadLine(prompt string) (string, error) {
	return readLineFn.Load()(prompt)
}

// SetReadLine replaces the ReadLine function for custom terminal frontends and tests.
func SetReadLine(fn func(string) (string, error)) {
	readLineFn.Store(fn)
}

// ResetReadLine restores the default stdin-backed line reader.
func ResetReadLine() {
	readLineFn.Store(defaultReadLine)
}
