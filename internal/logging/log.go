package logging

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Level represents log severity.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

type Logger struct {
	mu    sync.Mutex
	level Level
	w     *os.File
	color bool
}

func New(w *os.File, level Level, color bool) *Logger {
	return &Logger{w: w, level: level, color: color}
}

func (l *Logger) log(level Level, tag, msg string) {
	if level < l.level {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	ts := time.Now().Format(time.RFC3339)
	l.w.WriteString(fmt.Sprintf("%s [%s] %s\n", ts, tag, msg))
}

func (l *Logger) Debug(msg string) { l.log(LevelDebug, "DEBUG", msg) }
func (l *Logger) Info(msg string)  { l.log(LevelInfo, "INFO", msg) }
func (l *Logger) Warn(msg string)  { l.log(LevelWarn, "WARN", msg) }
func (l *Logger) Error(msg string) { l.log(LevelError, "ERROR", msg) }

// Default logger to stdout. Thread-safe via atomic pointer.
var defaultLogger atomic.Pointer[Logger]

func init() {
	defaultLogger.Store(New(os.Stdout, LevelInfo, false))
}

// Default returns the current default logger.
func Default() *Logger { return defaultLogger.Load() }

// SetDefault replaces the default logger.
func SetDefault(l *Logger) { defaultLogger.Store(l) }

// PrintSystem prints a system message to stdout with a color prefix.
func PrintSystem(msg string) {
	c := "\033[36m" // cyan
	r := "\033[0m"
	if !isTerminal() {
		c, r = "", ""
	}
	fmt.Printf("%s[system]%s %s\n", c, r, msg)
}

// PrintError prints an error message to stderr.
func PrintError(msg string) {
	c := "\033[31m" // red
	r := "\033[0m"
	if !isTerminal() {
		c, r = "", ""
	}
	fmt.Fprintf(os.Stderr, "%s[error]%s %s\n", c, r, msg)
}

func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// MultiHandler writes to multiple outputs.
type MultiHandler struct {
	mu      sync.Mutex
	writers []*os.File
}

func NewMultiHandler(writers ...*os.File) *MultiHandler {
	return &MultiHandler{writers: writers}
}

func (m *MultiHandler) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.writers {
		w.Write(p)
	}
	return len(p), nil
}

var _ = strings.TrimSpace
