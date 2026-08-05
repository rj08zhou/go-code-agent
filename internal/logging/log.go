package logging

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Level represents log severity.
type Level int

const (
	LevelInfo Level = iota
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
