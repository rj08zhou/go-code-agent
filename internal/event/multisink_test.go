package event

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go-code-agent/internal/logging"
)

// panicSink panics on every Emit.
type panicSink struct{}

func (panicSink) Emit(Event) { panic("boom") }

// recordingSink records every event it receives.
type recordingSink struct {
	mu    sync.Mutex
	count int
}

func (s *recordingSink) Emit(Event) {
	s.mu.Lock()
	s.count++
	s.mu.Unlock()
}

func (s *recordingSink) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

// TestMultiSinkIsolatesPanic verifies that a panicking sink does not crash
// Emit, that the remaining sinks still receive the event, and that the failure
// counter is incremented.
func TestMultiSinkIsolatesPanic(t *testing.T) {
	good := &recordingSink{}
	ms := NewMultiSink(panicSink{}, good)

	// Must not panic.
	ms.Emit(Event{Type: AgentStarted, AgentID: "lead", SessionID: "s1"})

	if good.Count() != 1 {
		t.Fatalf("healthy sink should have received the event, got %d", good.Count())
	}
	if got := ms.Failures(); got != 1 {
		t.Fatalf("expected 1 failure recorded, got %d", got)
	}
}

// TestMultiSinkMultiplePanics verifies the counter accumulates across sinks and
// calls.
func TestMultiSinkMultiplePanics(t *testing.T) {
	good := &recordingSink{}
	ms := NewMultiSink(panicSink{}, panicSink{}, good)

	ms.Emit(Event{Type: AgentStarted})
	ms.Emit(Event{Type: ModelCalled})

	if good.Count() != 2 {
		t.Fatalf("healthy sink should have received both events, got %d", good.Count())
	}
	if got := ms.Failures(); got != 4 {
		t.Fatalf("expected 4 failures recorded, got %d", got)
	}
}

func TestSessionLogSinkReportsDroppedEvents(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "agent.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	previousLogger := logging.Default()
	logging.SetDefault(logging.New(logFile, logging.LevelInfo, false))
	defer logging.SetDefault(previousLogger)

	sink, err := NewSessionLogSink(filepath.Join(t.TempDir(), "session.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	sink.Emit(Event{Type: ToolFinished, Payload: make(chan int)})
	if err := sink.f.Close(); err != nil {
		t.Fatal(err)
	}
	sink.path = t.TempDir() // Existing directory: Stat succeeds, reopen fails.
	sink.Emit(Event{Type: ToolFinished})

	if err := logFile.Sync(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{"session log marshal failed", "session log write failed", "reopen failed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("agent log missing %q: %q", want, got)
		}
	}
}

func TestConsoleSinkPreviewsReadFileOutput(t *testing.T) {
	outputLines := make([]string, 12)
	for i := range outputLines {
		outputLines[i] = fmt.Sprintf("line-%02d", i+1)
	}
	got := captureConsoleStderr(t, func() {
		NewConsoleSink().Emit(Event{
			Type:      ToolFinished,
			ToolName:  "read_file",
			AgentID:   "lead",
			SessionID: "session-1",
			Status:    "succeeded",
			Duration:  1500 * time.Millisecond,
			Output:    strings.Join(outputLines, "\n"),
			Payload: map[string]string{
				"path": "internal/event/sinks.go", "offset": "60", "limit": "12",
			},
		})
	})

	for _, want := range []string{
		"[read_file]", "succeeded", "path=internal/event/sinks.go", "offset=60", "limit=12",
		"dur=1.50s", "lines=12", "line-01", "line-08", "... 4 more lines",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("console read preview missing %q: %q", want, got)
		}
	}
	for _, unwanted := range []string{"agent=lead", "session=", "line-09"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("console read preview contains %q: %q", unwanted, got)
		}
	}
}

func TestConsoleSinkPreviewsSearchContentOutput(t *testing.T) {
	matches := make([]string, 10)
	for i := range matches {
		matches[i] = fmt.Sprintf("internal/event/sinks.go:%d:ConsoleSink", i+1)
	}
	got := captureConsoleStderr(t, func() {
		NewConsoleSink().Emit(Event{
			Type:     ToolFinished,
			ToolName: "search_content",
			Status:   "succeeded",
			Output:   strings.Join(matches, "\n"),
			Payload: map[string]string{
				"path": "internal/event", "pattern": "ConsoleSink",
			},
		})
	})

	for _, want := range []string{
		"[search_content]", "path=internal/event", `pattern="ConsoleSink"`,
		"matches=10", "sinks.go:1:ConsoleSink", "sinks.go:8:ConsoleSink", "... 2 more lines",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("console search preview missing %q: %q", want, got)
		}
	}
	if strings.Contains(got, "sinks.go:9:ConsoleSink") {
		t.Fatalf("console search preview exceeded eight lines: %q", got)
	}
}

func TestConsoleSinkReportsNoSearchMatches(t *testing.T) {
	got := captureConsoleStderr(t, func() {
		NewConsoleSink().Emit(Event{
			Type:     ToolFinished,
			ToolName: "search_content",
			Status:   "succeeded",
			Output:   "No matches found.",
		})
	})
	if !strings.Contains(got, "matches=0") || strings.Contains(got, "No matches found.") {
		t.Fatalf("no-match summary = %q", got)
	}
}

func TestConsoleSinkShowsShortSuccessfulToolOutput(t *testing.T) {
	got := captureConsoleStderr(t, func() {
		NewConsoleSink().Emit(Event{
			Type:     ToolFinished,
			ToolName: "write_file",
			Status:   "succeeded",
			Output:   "Wrote 120 bytes to internal/example.go",
		})
	})
	if !strings.Contains(got, "Wrote 120 bytes to internal/example.go") {
		t.Fatalf("short successful output was hidden: %q", got)
	}
}

func TestConsoleSinkKeepsNonLeadAgentAndFailureReason(t *testing.T) {
	got := captureConsoleStderr(t, func() {
		NewConsoleSink().Emit(Event{
			Type:      ToolFinished,
			ToolName:  "bash",
			AgentID:   "alice",
			SessionID: "session-1",
			Status:    "failed",
			Output:    "[ERROR] command failed\nsecond line",
		})
	})

	for _, want := range []string{"[bash]", "failed", "agent=alice", "error=[ERROR] command failed second line"} {
		if !strings.Contains(got, want) {
			t.Fatalf("console failure summary missing %q: %q", want, got)
		}
	}
	if strings.Contains(got, "session=") || strings.Count(got, "\n") != 1 {
		t.Fatalf("console failure summary must omit session and stay on one line: %q", got)
	}
}

func captureConsoleStderr(t *testing.T, emit func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	previous := os.Stderr
	os.Stderr = w
	emit()
	_ = w.Close()
	os.Stderr = previous

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}
