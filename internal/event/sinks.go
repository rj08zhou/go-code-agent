package event

import (
	"fmt"
	"os"
	"sync"

	"go-code-agent/internal/logging"
	"go-code-agent/internal/store"
)

// ConsoleSink renders concise, user-facing event summaries to stderr.
type ConsoleSink struct {
	mu sync.Mutex
}

func NewConsoleSink() *ConsoleSink { return &ConsoleSink{} }

// WriteInteractive renders blocking prompts through the same serialized
// console writer without turning prompt text into a persisted event.
func (s *ConsoleSink) WriteInteractive(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = os.Stderr.Write([]byte(text))
}

func (s *ConsoleSink) Emit(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = fmt.Fprint(os.Stderr, renderConsoleEvent(e))
}

// SessionLogSink appends structured events to a JSONL file for session replay.
type SessionLogSink struct {
	mu   sync.Mutex
	path string
	f    *os.File
}

func NewSessionLogSink(path string) (*SessionLogSink, error) {
	s := &SessionLogSink{path: path}
	if err := s.reopen(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SessionLogSink) reopen() error {
	f, err := store.OpenPrivateAppend(s.path)
	if err != nil {
		return err
	}
	if s.f != nil {
		_ = s.f.Close()
	}
	s.f = f
	return nil
}

func (s *SessionLogSink) Emit(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Keep structured task state complete so session replay can recover the
	// current checklist and DAG. Other tool output remains capped to avoid
	// allowing large file reads to grow session.log without bound.
	if !isTaskStateTool(e.ToolName) && len(e.Output) > 4000 {
		e.Output = e.Output[:4000] + "\n... (truncated for session.log)"
	}
	data, err := e.MarshalJSON()
	if err != nil {
		logging.Default().Error(fmt.Sprintf("session log marshal failed for %s: %v", e.Type, err))
		return
	}
	line := append(data, '\n')
	// Dir wipe leaves an open FD writing to an unlinked inode — detect via Stat.
	if s.f == nil {
		if err := s.reopen(); err != nil {
			logging.Default().Error(fmt.Sprintf("session log reopen failed: %v", err))
			return
		}
	} else if _, err := os.Stat(s.path); err != nil {
		if err := s.reopen(); err != nil {
			logging.Default().Error(fmt.Sprintf("session log reopen failed: %v", err))
			return
		}
	}
	if _, writeErr := s.f.Write(line); writeErr != nil {
		if err := s.reopen(); err != nil {
			logging.Default().Error(fmt.Sprintf("session log write failed: %v; reopen failed: %v", writeErr, err))
			return
		}
		if _, err := s.f.Write(line); err != nil {
			logging.Default().Error(fmt.Sprintf("session log retry write failed: %v", err))
		}
	}
}

func (s *SessionLogSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}
