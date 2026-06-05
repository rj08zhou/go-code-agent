package main

// File-backed persistence for all Print* calls, implemented as a slog
// JSONHandler bound to a session-aware io.Writer.
//
// Every Print* call routes through the package-level slog.Logger in
// internal/log; this file installs the file-side handler so each
// record becomes one JSONL line under the active session directory.
// The writer resolves the session path lazily on every Write() call,
// so session switches need no extra wiring.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"go-code-agent/internal/log"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// sessionLogFile is the basename inside the session directory
// (alongside decisions.jsonl, history/, etc.).
// Format: JSONL, one line per slog record.
const sessionLogFile = "session.log"

// sessionLogWriter is an io.Writer that appends every Write() to the
// active session's session.log. It is the bridge between slog
// (which expects a stable io.Writer) and our session-scoped path
// (which moves when the user switches sessions).
type sessionLogWriter struct{}

// Write opens the active session's log file in append mode for the
// duration of one record, writes the bytes, and closes. Best-effort:
// any I/O error is swallowed so a full disk or permission hiccup never
// disrupts the agent loop. Returns len(p) on the swallow path so slog
// considers the write successful and does not retry.
func (sessionLogWriter) Write(p []byte) (int, error) {
	path := logPath()
	if path == "" {
		return len(p), nil
	}

	// Defense-in-depth: directory should already exist, but if a user
	// rmdir'd it mid-session we transparently recreate it.
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return len(p), nil
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return len(p), nil
	}
	defer f.Close()

	_, _ = f.Write(p)
	return len(p), nil
}

// initFileLog installs a slog JSONHandler whose sink is the session
// log file. Call once after session bootstrap.
func initFileLog() {
	h := slog.NewJSONHandler(sessionLogWriter{}, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})
	log.SetFileHandler(h)
}

// shutdownFileLog clears the file handler so the agent shuts down
// cleanly. Call from the SIGINT handler / main defer.
func shutdownFileLog() {
	log.SetFileHandler(nil)
}

// logPath returns the active session's session.log path, or "" if no
// session is active. Resolved lazily so it follows session switches.
func logPath() string {
	if app == nil || app.SessionManager == nil || app.SessionManager.Active() == nil {
		return ""
	}
	return filepath.Join(app.SessionManager.Active().Dir(), sessionLogFile)
}

// renderSessionLog reads the active session's session.log and returns
// a human-readable summary for a future /log REPL command. Returns
// plain text (no ANSI codes) suitable for printing.
//
// The file is JSONL written by slog.JSONHandler, so every line carries
// at least: time, level, msg, kind. We surface kind in the summary
// (matching the previous "by level" breakdown).
func renderSessionLog() string {
	path := logPath()
	if path == "" {
		return "(no active session)"
	}
	f, err := os.Open(path)
	if err != nil {
		return "(no session log recorded yet)"
	}
	defer f.Close()

	return renderSessionLogReader(f)
}

// renderSessionLogReader is split out for testability: callers in
// production hand it an *os.File; tests can pass any io.Reader.
func renderSessionLogReader(r io.Reader) string {
	var b strings.Builder
	kinds := make(map[string]int)
	total := 0

	// Generous max-line cap covers very long tool outputs that landed
	// in the log via PrintTool.
	var buf [64 * 1024]byte
	sc := bufio.NewScanner(r)
	sc.Buffer(buf[:], len(buf))

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// We unmarshal into a generic map because slog's JSONHandler
		// emits a flat object whose schema depends on attrs (tool
		// records carry "name" + "output", decisions carry "tag", etc.).
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		total++

		ts, _ := rec["time"].(string)
		msg, _ := rec["msg"].(string)
		kind, _ := rec["kind"].(string)
		if kind == "" {
			kind = "(none)"
		}
		kinds[kind]++

		fmt.Fprintf(&b, "  %s  [%-9s] %s\n", ts, kind, msg)
	}

	if total == 0 {
		return "(no session log recorded yet)"
	}

	fmt.Fprintf(&b, "\n--- Session log (%d entries) ---\n", total)
	b.WriteString("by kind: ")
	first := true
	knownKinds := []string{"agent", "tool", "team_tool", "sub_tool", "system", "error", "decision"}
	for _, k := range knownKinds {
		if kinds[k] == 0 {
			continue
		}
		if !first {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%d", k, kinds[k])
		first = false
	}
	for k, c := range kinds {
		if slices.Contains(knownKinds, k) {
			continue
		}
		if !first {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%d", k, c)
		first = false
	}
	b.WriteString("\n")
	return b.String()
}
