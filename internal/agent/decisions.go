package agent

// Autonomous-decision audit trail.
//
// logging.PrintDecision surfaces every autonomous action (planning, context
// compaction, memory writes, judge self-evaluation, reflection) to the
// terminal. That solves "can I see it while it runs", but terminal output is
// ephemeral. This file wires a sink so each decision is ALSO appended to the
// active session's decisions.jsonl, giving a single replayable timeline of
// "what did the agent decide on its own this session" — viewable any time via
// the /decisions REPL command, even after the terminal scrollback is gone.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"go-code-agent/internal/logging"
	"go-code-agent/internal/session"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// decisionRecord is the on-disk shape of one decisions.jsonl line.
type decisionRecord struct {
	TS      string `json:"ts"`
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
}

// Decision kinds - the single source of truth for every string passed
// as logging.PrintDecision's first argument across this package
// (agent_loop.go, compression.go). Every call site should use one of
// these constants instead of a literal string.
//
// decisionKindOrder controls the preferred left-to-right ordering in
// RenderDecisions' "by kind" summary line; it existed as an inline
// literal before this and had already drifted out of sync with the
// actual kinds in use - DecisionTurn (emitted by finalizeTurn) had no
// entry, so it silently fell through to the unordered tail loop below
// instead of appearing in its intended position. Because every kind
// constant now feeds decisionKindOrder directly, that class of drift
// is no longer possible: forgetting to add a new kind here means it's
// not a usable constant, so a new call site would have to either
// reuse an existing one or add itself here first.
const (
	DecisionPlan    = "plan"
	DecisionContext = "context"
	DecisionMemory  = "memory"
	DecisionJudge   = "judge"
	DecisionReflect = "reflect"
	DecisionTurn    = "turn"
)

var decisionKindOrder = []string{
	DecisionPlan, DecisionContext, DecisionMemory,
	DecisionJudge, DecisionReflect, DecisionTurn,
}

// InitDecisionLog wires logging.PrintDecision to also persist each event to the
// active session's decisions.jsonl. The sink resolves the active session
// lazily, so it automatically follows session switches.
func InitDecisionLog() {
	logging.SetDecisionSink(appendDecision)
}

// decisionsPath returns the active session's decisions.jsonl path, or "" if
// no session is active.
func decisionsPath() string {
	if App == nil || App.SessionManager == nil || App.SessionManager.Active() == nil {
		return ""
	}
	return filepath.Join(App.SessionManager.Active().Dir(), session.SessionDecisionsFile)
}

// appendDecision persists one decision event. Best-effort: failures are
// silent so a logging hiccup never disrupts the agent loop.
func appendDecision(kind, summary string) {
	path := decisionsPath()
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	data, err := json.Marshal(decisionRecord{
		TS:      time.Now().Format(time.RFC3339),
		Kind:    kind,
		Summary: summary,
	})
	if err != nil {
		return
	}
	f.Write(append(data, '\n'))
}

// RenderDecisions reads the active session's decisions.jsonl and returns a
// human-readable timeline for the /decisions command.
func RenderDecisions() string {
	path := decisionsPath()
	if path == "" {
		return "(no active session)"
	}
	f, err := os.Open(path)
	if err != nil {
		return "(no autonomous decisions recorded this session yet)"
	}
	defer f.Close()

	var b strings.Builder
	counts := map[string]int{}
	total := 0
	var body strings.Builder

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec decisionRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		total++
		counts[rec.Kind]++
		fmt.Fprintf(&body, "  %s  [%-7s] %s\n", rec.TS, rec.Kind, rec.Summary)
	}

	if total == 0 {
		return "(no autonomous decisions recorded this session yet)"
	}

	fmt.Fprintf(&b, "--- Autonomous decision timeline (%d events) ---\n", total)
	b.WriteString("by kind: ")
	first := true
	for _, k := range decisionKindOrder {
		if counts[k] == 0 {
			continue
		}
		if !first {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%d", k, counts[k])
		first = false
		delete(counts, k)
	}
	// Any kinds not in the known ordering.
	for k, c := range counts {
		if !first {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%d", k, c)
		first = false
	}
	b.WriteString("\n\n")
	b.WriteString(body.String())
	return b.String()
}
