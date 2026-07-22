package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/internal/config"
	"go-code-agent/internal/history"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/model"
	"go-code-agent/internal/utils"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CompactedMarker prefixes AutoCompact's synthetic user-message.
const CompactedMarker = "[Compressed. "

// persistedBoundaryKey carries a caller-owned "not yet persisted"
// index pointer through ctx. Used by WithPersistedBoundary so that
// AutoCompact can keep the boundary valid when it replaces the message
// slice mid-turn.
type persistedBoundaryKey struct{}

// WithPersistedBoundary lets AutoCompact keep *boundary valid when it
// replaces the message slice. nil boundary is a no-op. Matching original.
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
// prefix clamps to 3 (start of tail). Matching original.
func remapPersistedBoundary(oldBoundary, split int) int {
	return 3 + maxval(oldBoundary-split, 0)
}

func maxval(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Compression handles microCompact (in-place truncation) and
// AutoCompact (LLM summary) to keep context windows under budget.
type Compression struct {
	gateway    *model.Gateway
	histStore  *history.Store
	dataDir    string
	modelID    string
	keepRecent int
}

func NewCompression(gw *model.Gateway, hs *history.Store, dataDir, modelID string) *Compression {
	return &Compression{
		gateway:    gw,
		histStore:  hs,
		dataDir:    dataDir,
		modelID:    modelID,
		keepRecent: config.KeepRecentMessages,
	}
}

// MicroCompact replaces old tool-result content with short placeholders,
// keeping the most recent config.KeepRecent tool results verbatim.
//
// minClearBytes is a clear_at_least guard: if the total bytes reclaimable from
// the clearable (old, >100-char) tool results is below minClearBytes, nothing
// is cleared and the message slice is left untouched. This avoids invalidating
// the provider prompt-cache prefix for a negligible saving. Pass 0 to disable
// the guard and always clear.
//
// It returns the number of tool results cleared and the number of bytes
// reclaimed.
func MicroCompact(msgs []llm.Message, minClearBytes int) (cleared int, reclaimed int) {
	nameMap := map[string]string{}
	for _, m := range msgs {
		if m.Role == llm.RoleAssistant {
			for _, tc := range m.ToolCalls {
				nameMap[tc.ID] = tc.Name
			}
		}
	}
	var toolIdx []int
	for i, m := range msgs {
		if m.Role == llm.RoleTool {
			toolIdx = append(toolIdx, i)
		}
	}
	if len(toolIdx) <= config.KeepRecent {
		return 0, 0
	}

	// First pass: identify old, large tool results and how many bytes clearing
	// each one would free (original length minus the placeholder length).
	type target struct {
		index int
		name  string
	}
	var targets []target
	reclaimable := 0
	for _, idx := range toolIdx[:len(toolIdx)-config.KeepRecent] {
		content := msgs[idx].Content
		if len(content) <= 100 {
			continue
		}
		name := nameMap[msgs[idx].ToolCallID]
		if name == "" {
			name = "unknown"
		}
		gain := len(content) - len("[cleared: ]") - len(name)
		if gain <= 0 {
			continue
		}
		targets = append(targets, target{index: idx, name: name})
		reclaimable += gain
	}

	// clear_at_least guard: skip entirely if the saving isn't worth breaking
	// the prompt-cache prefix.
	if minClearBytes > 0 && reclaimable < minClearBytes {
		return 0, 0
	}

	// Second pass: apply. Same-length in-place rewrite (only Content changes),
	// so message indices and the persisted boundary stay valid.
	for _, t := range targets {
		msgs[t.index] = llm.ToolMessage(fmt.Sprintf("[cleared: %s]", t.name), msgs[t.index].ToolCallID)
		cleared++
	}
	return cleared, reclaimable
}

// buildCompressInput renders a message prefix into compact structured text.
func buildCompressInput(msgs []llm.Message) string {
	const maxMsgChars = 500
	const maxToolChars = 200
	const maxArgChars = 120
	const maxTotalChars = 40000

	nameMap := map[string]string{}
	for _, m := range msgs {
		if m.Role == llm.RoleAssistant {
			for _, tc := range m.ToolCalls {
				nameMap[tc.ID] = tc.Name
			}
		}
	}
	var b strings.Builder
	for _, m := range msgs {
		if m.Role == llm.RoleUser && strings.TrimSpace(m.Content) != "" {
			b.WriteString("## Original task\n")
			b.WriteString(utils.Truncate(m.Content, maxMsgChars))
			b.WriteString("\n\n## Session history\n")
			break
		}
	}
	var lines []string
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleSystem:
			continue
		case llm.RoleUser:
			if strings.HasPrefix(m.Content, "<background-results>") ||
				strings.HasPrefix(m.Content, "<inbox>") {
				continue
			}
			lines = append(lines, fmt.Sprintf("[user]: %s", utils.Truncate(m.Content, maxMsgChars)))
		case llm.RoleAssistant:
			if strings.TrimSpace(m.Content) != "" {
				lines = append(lines, fmt.Sprintf("[assistant]: %s", utils.Truncate(m.Content, maxMsgChars)))
			}
			for _, tc := range m.ToolCalls {
				lines = append(lines, fmt.Sprintf("  -> calls %s(%s)", tc.Name, utils.Truncate(tc.Arguments, maxArgChars)))
			}
		case llm.RoleTool:
			name := nameMap[m.ToolCallID]
			if name == "" {
				name = "unknown"
			}
			lines = append(lines, fmt.Sprintf("  [tool:%s]: %s", name, utils.Truncate(m.Content, maxToolChars)))
		}
	}
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
// and keeps recent turns verbatim.
func (c *Compression) AutoCompact(ctx context.Context, msgs []llm.Message, sys string) []llm.Message {
	origCount := len(msgs)
	origTokens := llm.EstimateRequestTokens(msgs, nil)

	// Save full transcript
	tDir := filepath.Join(c.dataDir, "transcripts")
	os.MkdirAll(tDir, 0o755)
	tPath := filepath.Join(tDir, fmt.Sprintf("transcript_%d.jsonl", time.Now().Unix()))
	if f, err := os.Create(tPath); err == nil {
		enc := json.NewEncoder(f)
		for _, m := range msgs {
			enc.Encode(m)
		}
		f.Close()
	}
	fmt.Printf("[transcript saved: %s]\n", tPath)

	// Choose the prefix/tail split
	split := findCompactionSplit(msgs, c.keepRecent)

	// Summarize the prefix
	prefix := msgs[:split]
	var toSummarize []llm.Message
	for _, m := range prefix {
		if m.Role == llm.RoleSystem {
			continue
		}
		toSummarize = append(toSummarize, m)
	}

	convText := buildCompressInput(toSummarize)
	summary := "(summary failed)"

	resp, err := c.gateway.Call(ctx, "compress", llm.CallParams{
		Model:     c.modelID,
		MaxTokens: 4096,
		Messages: []llm.Message{llm.UserMessage(
			"Summarize the following EARLIER part of a coding session for continuity. " +
				"The most recent messages are NOT included here (they are kept verbatim after your summary), " +
				"so focus on durable context: the user's goals, decisions made, files/functions touched, and open threads.\n\n" +
				convText)},
	})
	if err == nil && resp != nil && strings.TrimSpace(resp.Content) != "" {
		summary = resp.Content
	}

	// Persist checkpoint
	if c.histStore != nil {
		covered := c.histStore.WrittenCount()
		if err := c.histStore.AppendCheckpoint(summary, covered); err == nil {
			fmt.Printf("[history] checkpoint saved (covered %d entries)\n", covered)
		}
	}

	tail := msgs[split:]
	fmt.Printf("[compacted %d/%d messages (~%d tokens) into %d-char summary; kept %d recent]\n",
		len(prefix), origCount, origTokens, len(summary), len(tail))

	// Remap caller-tracked persisted-boundary in place after the slice
	// is rebuilt, matching original.
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

// NeedsCompaction checks if context exceeds the budget threshold.
// Matching original: uses EstimateRequestTokens (includes tool defs in
// the estimate) and has no minimum-message guard.
func NeedsCompaction(msgs []llm.Message, tools []llm.ToolDef, contextWindowTokens int) bool {
	est := llm.EstimateRequestTokens(msgs, tools)
	threshold := int(float64(contextWindowTokens) * config.CompactionThresholdFrac)
	if threshold > config.TokenThreshold {
		threshold = config.TokenThreshold
	}
	return est > threshold
}

// findCompactionSplit picks a safe split index.
func findCompactionSplit(msgs []llm.Message, keepRecent int) int {
	n := len(msgs)
	if n <= keepRecent {
		return 0
	}
	desired := n - keepRecent
	if desired < 1 {
		return 0
	}
	minPreferUser := desired - keepRecent
	if minPreferUser < 1 {
		minPreferUser = 1
	}
	bestSafe := -1
	for s := desired; s >= 1; s-- {
		if !isSafeSplit(msgs, s) {
			continue
		}
		if bestSafe == -1 {
			bestSafe = s
		}
		if msgs[s].Role == llm.RoleUser && s >= minPreferUser {
			return s
		}
	}
	if bestSafe != -1 {
		return bestSafe
	}
	for s := desired + 1; s < n; s++ {
		if isSafeSplit(msgs, s) {
			return s
		}
	}
	return 0
}

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
