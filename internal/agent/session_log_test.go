package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// sessionLogEntry represents one JSON line from .go-code-agent/sessions/*/session.log.
type sessionLogEntry struct {
	Ts    string `json:"ts"`
	Level string `json:"level"`
	Msg   string `json:"message"`
}

// parseSessionLog reads a session.log file and returns all entries.
func parseSessionLog(t *testing.T, path string) []sessionLogEntry {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var entries []sessionLogEntry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e sessionLogEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Logf("WARN: skip non-JSON line: %s", line[:min(len(line), 80)])
			continue
		}
		entries = append(entries, e)
	}
	return entries
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// findLatestSessionLog locates the most recent session.log for testing.
func findLatestSessionLog(t *testing.T) string {
	t.Helper()
	dir := "."
	for i := 0; i < 6; i++ {
		matches, _ := filepath.Glob(filepath.Join(dir, ".go-code-agent/sessions/*/session.log"))
		if len(matches) > 0 {
			sort.Strings(matches)
			return matches[len(matches)-1]
		}
		dir = filepath.Join(dir, "..")
	}
	return ""
}

// reflectEvent is the parsed reflection event from a log line.
type reflectEvent struct {
	ts              time.Time
	roundsSinceComp int
	kinds           []string
	rawMsg          string
}

// parseReflectEvent parses the decision message to extract roundsSinceComplete and kinds.
// Format: "... (consecutiveFailures=N, roundsSinceComplete=M, kinds=[...])"
func parseReflectEvent(msg string) (roundsSinceComp int, kinds []string) {
	// Extract roundsSinceComplete
	if idx := strings.Index(msg, "roundsSinceComplete="); idx != -1 {
		rest := msg[idx+len("roundsSinceComplete="):]
		end := strings.IndexAny(rest, ",)")
		if end == -1 {
			end = len(rest)
		}
		fmt.Sscanf(rest[:end], "%d", &roundsSinceComp)
	}
	// Extract kinds — Go's %v on []string produces "[a b c]" (space-separated).
	if idx := strings.Index(msg, "kinds="); idx != -1 {
		rest := msg[idx+len("kinds="):]
		// Strip trailing ")" or whitespace
		if end := strings.Index(rest, ")"); end != -1 {
			rest = rest[:end]
		}
		rest = strings.TrimSpace(rest)
		// Handle %v output: []string{"a","b"} → "[a b]"
		rest = strings.TrimPrefix(rest, "[")
		rest = strings.TrimSuffix(rest, "]")
		rest = strings.TrimSpace(rest)
		if rest != "" {
			kinds = strings.Fields(rest)
		}
	}
	return
}

// TestSessionLog_ReflectionCooldownViolation parses a real session log and
// reports any cool-down violations for strategy-change, stuck, and todo-nag.
//
// NOTE: We cannot perfectly detect violations because the log records
// roundsSinceComplete (not toolRounds). We approximate by checking that
// the same kind does not appear in back-to-back reflection events.
func TestSessionLog_ReflectionCooldownViolation(t *testing.T) {
	logPath := findLatestSessionLog(t)
	if logPath == "" {
		t.Skip("no session log found; skipping")
	}
	t.Logf("Using session log: %s", logPath)
	entries := parseSessionLog(t, logPath)

	var events []reflectEvent
	for _, e := range entries {
		if e.Level != "decision" || !strings.Contains(e.Msg, "[reflect]") {
			continue
		}
		ts, err := time.Parse(time.RFC3339, e.Ts)
		if err != nil {
			continue
		}
		rsc, kinds := parseReflectEvent(e.Msg)
		events = append(events, reflectEvent{
			ts:              ts,
			roundsSinceComp: rsc,
			kinds:           kinds,
			rawMsg:          e.Msg,
		})
	}

	if len(events) == 0 {
		t.Skip("no reflection events found in log")
	}

	t.Logf("Found %d reflection events:", len(events))
	for _, ev := range events {
		t.Logf("  @ %s  rsc=%d  kinds=%v",
			ev.ts.Format("15:04:05"), ev.roundsSinceComp, ev.kinds)
	}

	// Check: if kinds is non-empty, no same kind should appear in
	// consecutive events (approximating cool-down).
	// This is a conservative check — it may have false negatives
	// if toolRounds and roundsSinceComplete diverge significantly.
	kindLastSeen := make(map[string]int)
	violations := 0
	for i, ev := range events {
		for _, kind := range ev.kinds {
			last := kindLastSeen[kind]
			if last != 0 && i-last == 1 {
				t.Logf("MAYBE VIOLATION: kind %q appears in consecutive events [%d] and [%d]",
					kind, last, i)
				violations++
			}
			kindLastSeen[kind] = i
		}
	}
	if violations == 0 {
		t.Logf("No obvious cool-down violations detected (kinds=%v in log; may be empty if using old code)",
			events[0].kinds)
	}
}

// TestSessionLog_CompileErrorLoop detects a pattern where the agent
// repeatedly hits the same compile error without making progress.
func TestSessionLog_CompileErrorLoop(t *testing.T) {
	logPath := findLatestSessionLog(t)
	if logPath == "" {
		t.Skip("no session log found; skipping")
	}
	t.Logf("Using session log: %s", logPath)
	entries := parseSessionLog(t, logPath)

	type compileErr struct {
		ts   time.Time
		msg  string
		line string
	}
	var errors []compileErr
	for _, e := range entries {
		if e.Level == "tool" && strings.Contains(e.Msg, "FAIL") {
			ts, err := time.Parse(time.RFC3339, e.Ts)
			if err != nil {
				continue
			}
			firstLine := strings.SplitN(e.Msg, "\n", 2)[0]
			errors = append(errors, compileErr{ts, e.Msg, firstLine})
		}
	}

	if len(errors) == 0 {
		t.Skip("no compile errors found")
	}

	t.Logf("Found %d compile error events:", len(errors))
	for _, err := range errors {
		t.Logf("  @ %s: %s", err.ts.Format("15:04:05"), err.line)
	}

	// Check for repeated same errors (agent stuck in a loop).
	// Normalize: take the first 120 chars as signature.
	msgCount := make(map[string]int)
	for _, err := range errors {
		sig := err.line
		if len(sig) > 120 {
			sig = sig[:120]
		}
		msgCount[sig]++
	}
	for sig, count := range msgCount {
		if count >= 3 {
			t.Logf("REPEATED ERROR (x%d): %s...", count, sig)
		}
	}
}

// TestSessionLog_ContextCompactionRate measures how often the context
// window is compacted. Excessive compaction causes the agent to lose
// file contents (leading to edit_file "Text not found" errors).
func TestSessionLog_ContextCompactionRate(t *testing.T) {
	logPath := findLatestSessionLog(t)
	if logPath == "" {
		t.Skip("no session log found; skipping")
	}
	t.Logf("Using session log: %s", logPath)
	entries := parseSessionLog(t, logPath)

	compactions := 0
	var firstTs, lastTs time.Time
	for _, e := range entries {
		if e.Level == "decision" && strings.Contains(e.Msg, "micro-compacted") {
			compactions++
			ts, err := time.Parse(time.RFC3339, e.Ts)
			if err != nil {
				continue
			}
			if firstTs.IsZero() {
				firstTs = ts
			}
			lastTs = ts
		}
	}

	if compactions == 0 {
		t.Skip("no context compaction events found")
	}

	duration := lastTs.Sub(firstTs).Minutes()
	if duration < 1.0 {
		duration = 1.0
	}
	t.Logf("Context compaction: %d events over %.1f minutes (%.1f/min)",
		compactions, duration, float64(compactions)/duration)

	// Flag excessive compaction: >3/min is likely too aggressive.
	if float64(compactions)/duration > 3.0 {
		t.Logf("EXCESSIVE COMPACTION: %d compactions in %.1f min = %.1f/min (expected < 3/min)",
			compactions, duration, float64(compactions)/duration)
	}
}

// TestSessionLog_DangerousCommandAttempts checks for blocked dangerous
// commands (e.g., "rm /*").
func TestSessionLog_DangerousCommandAttempts(t *testing.T) {
	logPath := findLatestSessionLog(t)
	if logPath == "" {
		t.Skip("no session log found; skipping")
	}
	t.Logf("Using session log: %s", logPath)
	entries := parseSessionLog(t, logPath)

	found := 0
	for _, e := range entries {
		if e.Level == "tool" && strings.Contains(e.Msg, "Security blocked") {
			t.Logf("DANGEROUS COMMAND BLOCKED: %s", e.Msg)
			found++
		}
	}
	if found == 0 {
		t.Log("No dangerous command attempts found (good)")
	}
}

// TestSessionLog_ReflectionTriggerPattern analyzes the pattern of
// reflection triggers to detect stuck loops (e.g., firing every round).
func TestSessionLog_ReflectionTriggerPattern(t *testing.T) {
	logPath := findLatestSessionLog(t)
	if logPath == "" {
		t.Skip("no session log found; skipping")
	}
	t.Logf("Using session log: %s", logPath)
	entries := parseSessionLog(t, logPath)

	// Extract reflection events
	var events []reflectEvent
	for _, e := range entries {
		if e.Level != "decision" || !strings.Contains(e.Msg, "[reflect]") {
			continue
		}
		ts, err := time.Parse(time.RFC3339, e.Ts)
		if err != nil {
			continue
		}
		rsc, kinds := parseReflectEvent(e.Msg)
		events = append(events, reflectEvent{
			ts:              ts,
			roundsSinceComp: rsc,
			kinds:           kinds,
		})
	}

	if len(events) < 2 {
		t.Skip("too few reflection events to analyze pattern")
	}

	// Compute intervals between consecutive reflection events (in seconds).
	intervals := make([]float64, len(events)-1)
	for i := 1; i < len(events); i++ {
		intervals[i-1] = events[i].ts.Sub(events[i-1].ts).Seconds()
	}
	t.Logf("Reflection intervals (seconds): %v", intervals)

	// Flag: if median interval < 60s, reflections are firing too fast.
	sorted := make([]float64, len(intervals))
	copy(sorted, intervals)
	sort.Float64s(sorted)
	median := sorted[len(sorted)/2]
	t.Logf("Median interval: %.1fs", median)

	if median < 60.0 {
		t.Logf("REFLECTION TOO FREQUENT: median interval %.1fs < 60s; agent may be stuck in a reflection loop",
			median)
	}

	// Also check for repeated roundsSinceComplete values (agent not making progress).
	valCount := make(map[int]int)
	for _, ev := range events {
		valCount[ev.roundsSinceComp]++
	}
	for val, cnt := range valCount {
		if cnt >= 3 {
			t.Logf("STUCK PROGRESS: roundsSinceComplete=%d appears %d times (agent may not be completing tasks)",
				val, cnt)
		}
	}
}

// TestSessionLog_EditFileFailures detects repeated edit_file failures
// ("Text not found"), indicating the agent is operating on stale context.
func TestSessionLog_EditFileFailures(t *testing.T) {
	logPath := findLatestSessionLog(t)
	if logPath == "" {
		t.Skip("no session log found; skipping")
	}
	t.Logf("Using session log: %s", logPath)
	entries := parseSessionLog(t, logPath)

	failures := 0
	for _, e := range entries {
		if e.Level == "tool" && strings.Contains(e.Msg, "Error: Text not found") {
			failures++
			ts, _ := time.Parse(time.RFC3339, e.Ts)
			t.Logf("  edit_file failure @ %s", ts.Format("15:04:05"))
		}
	}

	if failures == 0 {
		t.Log("No edit_file 'Text not found' errors (good)")
		return
	}

	t.Logf("Total edit_file 'Text not found' errors: %d", failures)
	if failures >= 3 {
		t.Logf("EDIT_FILE STUCK: %d 'Text not found' errors — agent likely operating on stale context",
			failures)
	}
}

// TestSessionLog_LLMRetry analyzes LLM retry events to detect rate-limiting
// or persistent API failures.
func TestSessionLog_LLMRetry(t *testing.T) {
	logPath := findLatestSessionLog(t)
	if logPath == "" {
		t.Skip("no session log found; skipping")
	}
	t.Logf("Using session log: %s", logPath)
	entries := parseSessionLog(t, logPath)

	retries := 0
	var lastErr string
	for _, e := range entries {
		if e.Level == "system" && strings.Contains(e.Msg, "llm-retry") {
			retries++
			if strings.Contains(e.Msg, "429") {
				lastErr = "429 Too Many Requests"
			}
		}
	}

	if retries == 0 {
		t.Log("No LLM retries observed (good)")
		return
	}

	t.Logf("Total LLM retries: %d", retries)
	if retries >= 5 {
		t.Logf("LLM RETRY STORM: %d retries observed (last err: %s) — check API quota/rate-limits",
			retries, lastErr)
	}
}
