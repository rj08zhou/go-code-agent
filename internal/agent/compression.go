package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/logging"
	"go-code-agent/internal/session"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Compression - three layers:
//   microCompact: in-place tool-result truncation
//   AutoCompact: LLM-summarized compaction with checkpoint persistence
//   manual: triggered by the `compress` tool

// CompactedMarker prefixes AutoCompact's synthetic user-message.
// Used by persistNewMessages (cmd/agent/main.go) to avoid duplicate
// history entries.
const CompactedMarker = "[Compressed. "

// estimateTokens is a thin alias over EstimateTokens (llm_types.go).
// Keeping the lowercase name avoids touching every call site.
func estimateTokens(msgs []llm.Message) int { return llm.EstimateTokens(msgs) }

// microCompact replaces old tool-result content with short placeholders.
// Returns the number of tool results folded this pass so the caller can
// surface a decision event (this used to be completely silent).
func microCompact(msgs []llm.Message) int {
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

// AutoCompact performs PROGRESSIVE compaction: it summarizes only the
// OLDER prefix of the conversation and keeps the most recent turns
// VERBATIM, rather than replacing the entire history with a single
// lossy summary. This is the key fix for the old behavior where, right
// after compaction, the model had zero verbatim recent context (only a
// summary) and would re-explore work it had just done.
//
// Result shape:
//
//	[ system(sys), user(summary-of-old-prefix), assistant(ack), <recent tail...> ]
//
// The split between "summarized prefix" and "verbatim tail" is snapped
// to a safe turn boundary by findCompactionSplit so a tool_call is
// never separated from its tool_result (both OpenAI and Anthropic
// reject such orphans). If no safe boundary exists (pathological
// input) it falls back to summarizing everything - never emitting a
// corrupt message sequence.
//
// Restart note: the checkpoint written here still covers the whole
// prior history (covered = WrittenCount), so a RESTART resumes from the
// summary alone, without the verbatim tail. Preserving the tail across
// restarts would require mapping history entries to messages 1:1 (they
// are not - system/gate-injection messages have no entry); that is a
// separate follow-up. The in-memory progressive keep already fixes the
// common same-session case.
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

	// Summarize the prefix, skipping any leading system message(s) - the
	// fresh sys is re-injected below, so summarizing the old system text
	// would be wasteful and redundant.
	prefix := msgs[:split]
	var toSummarize []llm.Message
	for _, m := range prefix {
		if m.Role == llm.RoleSystem {
			continue
		}
		toSummarize = append(toSummarize, m)
	}

	convData, _ := json.Marshal(toSummarize)
	convText := string(convData)
	// Keep the TAIL of the prefix when truncating: the most recent of
	// the to-be-summarized messages matter most for continuity (the old
	// code kept the first 80KB, i.e. the least relevant, oldest part).
	const maxSummaryInput = 80000
	if len(convText) > maxSummaryInput {
		convText = convText[len(convText)-maxSummaryInput:]
	}

	resp, err := llm.NewClient(nil).CallWithRetry(ctx, "compress", llm.CallParams{
		Model: App.Model,
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

	// Persist the compaction as a checkpoint. The checkpoint supersedes
	// all prior history entries for replay purposes; raw entries stay
	// on disk for audit.
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
