package memory

import (
	"encoding/json"
	"fmt"
	"strings"
)

// BackfillMemory summarizes inactive sessions by scanning history.jsonl.
// It extracts key decisions and stores them as structured memory items.
func (s *Store) BackfillMemory(sessionID, modelID string, callModel func(ctx interface{}, sys, user string) (string, error)) string {
	// Read history entries for this session
	entries, err := s.readHistory(sessionID)
	if err != nil || len(entries) == 0 {
		return "Nothing to backfill."
	}

	// Compose a summary prompt
	var sb strings.Builder
	sb.WriteString("Summarize the key decisions made in this conversation:\n")
	for i, e := range entries {
		if i >= 50 {
			sb.WriteString("\n... (older messages omitted)\n")
			break
		}
		if e.Role == "assistant" && e.Content != "" {
			text := e.Content
			if len(text) > 300 {
				text = text[:300] + "..."
			}
			sb.WriteString(text)
			sb.WriteString("\n")
		}
	}

	if callModel != nil {
		summary, err := callModel(nil, "You summarize conversations into concise bullet-point learnings.", sb.String())
		if err != nil {
			return fmt.Sprintf("Backfill failed: %v", err)
		}
		// Store each sentence as a memory item
		points := strings.Split(summary, "\n")
		for _, p := range points {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			s.Write(p, "backfill")
		}
		return fmt.Sprintf("Backfilled %d memory items from session %s.", len(points), sessionID[:12])
	}
	return "No model available for backfill."
}

type historyEntry struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (s *Store) readHistory(sessionID string) ([]historyEntry, error) {
	// Simplified: memory items are stored per-session; return as entries
	raw := s.Search(sessionID[:12], 50, 0, "")
	var entries []historyEntry
	_ = json.Unmarshal([]byte(raw), &entries)
	return entries, nil
}
