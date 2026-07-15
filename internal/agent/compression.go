package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/logging"
	"go-code-agent/internal/session"
	"go-code-agent/utils"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Compression layers: microCompact (in-place truncation), AutoCompact (LLM summary).

// CompactedMarker prefixes AutoCompact's synthetic user-message.
// Used by persistNewMessages (cmd/agent/main.go) to avoid duplicate
// history entries.
const CompactedMarker = "[Compressed. "

// persistedBoundaryKey carries a caller-owned "not yet persisted"
// index pointer through ctx. See WithPersistedBoundary.
type persistedBoundaryKey struct{}

// WithPersistedBoundary lets AutoCompact keep *boundary valid when it
// replaces the message slice mid-turn. nil boundary is a no-op.
func WithPersistedBoundary(ctx context.Context, boundary *int) context.Context {
	if boundary == nil {
		return ctx
	}
	return context.WithValue(ctx, persistedBoundaryKey{}, boundary)
}

func persistedBoundaryFromCtx(ctx context.Context) *int {
	if ctx == nil {
		return nil
	}
	b, _ := ctx.Value(persistedBoundaryKey{}).(*int)
	return b
}

// remapPersistedBoundary converts an old-slice index into AutoCompact's
// new [system, summary, ack, tail...] slice. A boundary in the summarized
// prefix clamps to 3 (start of tail).
func remapPersistedBoundary(oldBoundary, split int) int {
	return 3 + max(0, oldBoundary-split)
}

// estimateTokens includes ToolDefs in the estimate (every request carries the
// full tool schema). Thin alias over EstimateRequestTokens to avoid touching
// every call site.
func estimateTokens(msgs []llm.Message) int {
	return llm.EstimateRequestTokens(msgs, ToolDefs)
}

// microCompact replaces old tool-result content with short placeholders.
// Returns the number of tool results folded this pass so the caller can
// surface a decision event (this used to be completely silent).
func microCompact(msgs []llm.Message) int {
	return microCompactImpl(msgs)
}

// MicroCompact is the exported wrapper for microCompact, used by
// bootConversation to fold stale tool results on history restore.
func MicroCompact(msgs []llm.Message) int { return microCompactImpl(msgs) }

func microCompactImpl(msgs []llm.Message) int {
	// Build tool-call id -> name map for placeholder labels.
	nameMap := map[string]string{}
	for _, m := range msgs {
		if m.Role == llm.RoleAssistant {
			for _, tc := range m.ToolCalls {
				nameMap[tc.ID] = tc.Name
			}
		}
	}
	type tmInfo struct {
		index  int
		callID string
	}
	var toolMsgs []tmInfo
	for i, m := range msgs {
		if m.Role == llm.RoleTool {
			toolMsgs = append(toolMsgs, tmInfo{i, m.ToolCallID})
		}
	}
	if len(toolMsgs) <= infra.KeepRecent {
		return 0
	}
	cleared := 0
	for _, ti := range toolMsgs[:len(toolMsgs)-infra.KeepRecent] {
		if len(msgs[ti.index].Content) > 100 {
			name := nameMap[ti.callID]
			if name == "" {
				name = "unknown"
			}
			msgs[ti.index] = llm.ToolMessage(fmt.Sprintf("[cleared: %s]", name), ti.callID)
			cleared++
		}
	}
	return cleared
}

// buildCompressInput renders a message prefix into compact structured text
// for the summarization LLM call. Unlike raw JSON, it always preserves the
// first user message (the original task), truncates each message to a short
// summary (no full tool outputs), and caps total size.
func buildCompressInput(msgs []llm.Message) string {
	const (
		maxMsgChars   = 500
		maxToolChars  = 200
		maxArgChars   = 120
		maxTotalChars = 40000
	)

	// Build tool-call id -> name map for labeling tool results.
	nameMap := map[string]string{}
	for _, m := range msgs {
		if m.Role == llm.RoleAssistant {
			for _, tc := range m.ToolCalls {
				nameMap[tc.ID] = tc.Name
			}
		}
	}

	var b strings.Builder

	// Always include the first real user message as "Original task".
	for _, m := range msgs {
		if m.Role == llm.RoleUser && strings.TrimSpace(m.Content) != "" {
			b.WriteString("## Original task\n")
			b.WriteString(utils.Truncate(m.Content, maxMsgChars))
			b.WriteString("\n\n## Session history\n")
			break
		}
	}

	// Render each message as a compact one-liner.
	var lines []string
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleSystem:
			continue
		case llm.RoleUser:
			if strings.HasPrefix(m.Content, "<background-results>") ||
				strings.HasPrefix(m.Content, "<inbox>") {
				continue // skip control-block injections
			}
			lines = append(lines, fmt.Sprintf("[user]: %s",
				utils.Truncate(m.Content, maxMsgChars)))
		case llm.RoleAssistant:
			if strings.TrimSpace(m.Content) != "" {
				lines = append(lines, fmt.Sprintf("[assistant]: %s",
					utils.Truncate(m.Content, maxMsgChars)))
			}
			for _, tc := range m.ToolCalls {
				lines = append(lines, fmt.Sprintf("  -> calls %s(%s)",
					tc.Name, utils.Truncate(tc.Arguments, maxArgChars)))
			}
		case llm.RoleTool:
			name := nameMap[m.ToolCallID]
			if name == "" {
				name = "unknown"
			}
			lines = append(lines, fmt.Sprintf("  [tool:%s]: %s",
				name, utils.Truncate(m.Content, maxToolChars)))
		}
	}

	// Keep the tail of the history (most recent matters most).
	all := strings.Join(lines, "\n")
	if len(all) > maxTotalChars {
		all = all[len(all)-maxTotalChars:]
		if idx := strings.Index(all, "\n"); idx >= 0 {
			all = all[idx+1:]
		}
	}

	b.WriteString(all)
	return b.String()
}

// AutoCompact performs progressive compaction: summarizes the older prefix
// and keeps recent turns verbatim. The split is snapped to a safe turn
// boundary so tool_call/result pairs are never orphaned.
func AutoCompact(ctx context.Context, msgs []llm.Message, sys string) []llm.Message {
	origCount := len(msgs)
	origTokens := estimateTokens(msgs)
	tDir := filepath.Join(App.SessionManager.Active().Dir(), session.SessionTranscriptsDir)
	os.MkdirAll(tDir, 0o755)
	tPath := filepath.Join(tDir, fmt.Sprintf("transcript_%d.jsonl", time.Now().Unix()))
	if f, err := os.Create(tPath); err == nil {
		enc := json.NewEncoder(f)
		for _, m := range msgs {
			enc.Encode(m)
		}
		f.Close()
	}
	logging.PrintSystem(fmt.Sprintf("[transcript saved: %s]", tPath))

	// Choose the prefix/tail split. prefix = msgs[:split] is summarized,
	// tail = msgs[split:] is kept verbatim.
	split := findCompactionSplit(msgs, infra.KeepRecentMessages)

	// Summarize the prefix, skipping leading system messages (re-injected below).
	prefix := msgs[:split]
	var toSummarize []llm.Message
	for _, m := range prefix {
		if m.Role == llm.RoleSystem {
			continue
		}
		toSummarize = append(toSummarize, m)
	}

	convText := buildCompressInput(toSummarize)

	resp, err := llm.NewClient(nil).CallWithRetry(ctx, "compress", llm.CallParams{
		Model:     App.Model,
		MaxTokens: 4096,
		Messages: []llm.Message{llm.UserMessage(
			"Summarize the following EARLIER part of a coding session for continuity. " +
				"The most recent messages are NOT included here (they are kept verbatim after your summary), " +
				"so focus on durable context: the user's goals, decisions made, files/functions touched, and open threads.\n\n" +
				convText)},
	})
	summary := "(summary failed)"
	if err == nil && resp != nil && strings.TrimSpace(resp.Content) != "" {
		summary = resp.Content
	}

	// Persist the compaction as a checkpoint (supersedes prior history for replay).
	if App != nil && App.History() != nil {
		covered := App.History().WrittenCount()
		if err := App.History().AppendCheckpoint(summary, covered); err != nil {
			logging.PrintSystem(fmt.Sprintf("[history] checkpoint write failed: %v", err))
		} else {
			logging.PrintSystem(fmt.Sprintf("[history] checkpoint saved (covered %d entries)", covered))
		}
	}

	tail := msgs[split:]
	logging.PrintDecision(DecisionContext, fmt.Sprintf(
		"compacted %d/%d messages (~%d tokens) into a %d-char summary; kept %d recent messages verbatim; full transcript at %s",
		len(prefix), origCount, origTokens, len(summary), len(tail), tPath))

	// Remap any caller-tracked persisted-boundary in place (see
	// WithPersistedBoundary) now that msgs has shrunk.
	if boundary := persistedBoundaryFromCtx(ctx); boundary != nil {
		*boundary = remapPersistedBoundary(*boundary, split)
	}

	out := make([]llm.Message, 0, len(tail)+3)
	out = append(out,
		llm.SystemMessage(sys),
		llm.UserMessage(fmt.Sprintf("%sTranscript: %s]\n%s", CompactedMarker, tPath, summary)),
		llm.AssistantMessage("Understood. Continuing with the summary above plus the recent messages that follow."),
	)
	out = append(out, tail...)
	return out
}

// findCompactionSplit picks the index that separates the summarized
// prefix (msgs[:split]) from the verbatim tail (msgs[split:]) for
// AutoCompact. It targets keeping ~keepRecent trailing messages but
// snaps to a SAFE turn boundary so compaction never orphans a
// tool_call from its tool_result.
//
// A split index s is safe when BOTH hold:
//   - msgs[s] is not a tool result (else its originating assistant
//     tool_call would be summarized into the prefix, orphaning it).
//   - msgs[s-1] is not an assistant message carrying tool_calls (else
//     its tool_result messages, which follow at s.., land in the tail,
//     orphaning the call).
//
// Preference order:
//  1. From the desired index, scan BACKWARD for a RoleUser boundary -
//     the cleanest split, since a user turn always starts a fresh
//     exchange - BUT only if it is within keepRecent of desired, so a
//     distant past user turn can't force us to keep almost the whole
//     conversation verbatim (bounded drift; tail stays <= ~2*keepRecent).
//  2. Else the nearest safe boundary at/below desired (keeps the tail
//     closest to keepRecent).
//  3. Else scan FORWARD from desired for any safe boundary.
//  4. Else 0 (summarize everything) - the safe fallback that can never
//     produce an orphaned pair.
func findCompactionSplit(msgs []llm.Message, keepRecent int) int {
	n := len(msgs)
	if n <= keepRecent {
		return 0
	}
	desired := n - keepRecent
	if desired < 1 {
		return 0
	}

	// A user boundary is only "preferred" when it doesn't drift more
	// than keepRecent below desired; past that, preferring it would keep
	// far more than intended and this compaction would free too little
	// context (possibly re-triggering next round). Below the window we
	// fall back to bestSafe, the boundary closest to desired.
	minPreferUser := desired - keepRecent
	if minPreferUser < 1 {
		minPreferUser = 1
	}

	// 1 + 2: scan backward from desired.
	bestSafe := -1
	for s := desired; s >= 1; s-- {
		if !isSafeSplit(msgs, s) {
			continue
		}
		if bestSafe == -1 {
			bestSafe = s // nearest safe boundary at/below desired
		}
		if msgs[s].Role == llm.RoleUser && s >= minPreferUser {
			return s // preferred clean boundary, within the drift window
		}
	}
	if bestSafe != -1 {
		return bestSafe
	}

	// 3: nothing safe at/below desired; look forward (keep fewer).
	for s := desired + 1; s < n; s++ {
		if isSafeSplit(msgs, s) {
			return s
		}
	}

	// 4: give up on progressive keep; summarize everything.
	return 0
}

// isSafeSplit reports whether splitting at index s keeps every
// tool_call paired with its tool_result on the same side of the cut.
// See findCompactionSplit for the rule.
func isSafeSplit(msgs []llm.Message, s int) bool {
	if s <= 0 || s >= len(msgs) {
		return false
	}
	if msgs[s].Role == llm.RoleTool {
		return false
	}
	if prev := msgs[s-1]; prev.Role == llm.RoleAssistant && len(prev.ToolCalls) > 0 {
		return false
	}
	return true
}
