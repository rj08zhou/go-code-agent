package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/log"
	"go-code-agent/internal/session"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Compression - three layers:
//   microCompact: in-place tool-result truncation
//   autoCompact: LLM-summarized compaction with checkpoint persistence
//   manual: triggered by the `compress` tool

// compactedMarker prefixes autoCompact's synthetic user-message.
// Used by persistNewMessages to avoid duplicate history entries.
const compactedMarker = "[Compressed. "

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

// autoCompact saves transcript to disk and replaces messages with an LLM summary.
// Writes a checkpoint to HistoryStore so restarts resume from the summary.
func autoCompact(ctx context.Context, msgs []llm.Message, sys string) []llm.Message {
	origCount := len(msgs)
	origTokens := estimateTokens(msgs)
	tDir := filepath.Join(app.SessionManager.Active().Dir(), session.SessionTranscriptsDir)
	os.MkdirAll(tDir, 0o755)
	tPath := filepath.Join(tDir, fmt.Sprintf("transcript_%d.jsonl", time.Now().Unix()))
	if f, err := os.Create(tPath); err == nil {
		enc := json.NewEncoder(f)
		for _, m := range msgs {
			enc.Encode(m)
		}
		f.Close()
	}
	log.PrintSystem(fmt.Sprintf("[transcript saved: %s]", tPath))

	convData, _ := json.Marshal(msgs)
	convText := string(convData)
	if len(convText) > 80000 {
		convText = convText[:80000]
	}
	resp, err := llm.NewClient(nil).CallWithRetry(ctx, "compress", llm.CallParams{
		Model:    model,
		Messages: []llm.Message{llm.UserMessage("Summarize for continuity:\n" + convText)},
	})
	summary := "(summary failed)"
	if err == nil && resp != nil && strings.TrimSpace(resp.Content) != "" {
		summary = resp.Content
	}

	// Persist the compaction as a checkpoint. The checkpoint supersedes
	// all prior history entries for replay purposes; raw entries stay
	// on disk for audit.
	if app != nil && app.History() != nil {
		covered := app.History().WrittenCount()
		if err := app.History().AppendCheckpoint(summary, covered); err != nil {
			log.PrintSystem(fmt.Sprintf("[history] checkpoint write failed: %v", err))
		} else {
			log.PrintSystem(fmt.Sprintf("[history] checkpoint saved (covered %d entries)", covered))
		}
	}

	log.PrintDecision("context", fmt.Sprintf(
		"compacted %d messages (~%d tokens) into a %d-char summary; full transcript kept at %s",
		origCount, origTokens, len(summary), tPath))

	return []llm.Message{
		llm.SystemMessage(sys),
		llm.UserMessage(fmt.Sprintf("%sTranscript: %s]\n%s", compactedMarker, tPath, summary)),
		llm.AssistantMessage("Understood. Continuing with summary context."),
	}
}
