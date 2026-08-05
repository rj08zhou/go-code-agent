package application

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"go-code-agent/internal/history"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/logging"
	"go-code-agent/internal/prompt"
	"go-code-agent/internal/session"
	"go-code-agent/internal/utils"
)

const memoryBackfillTimeout = 10 * time.Minute

// StartMemoryBackfill summarizes unsaved non-archived sessions (excluding the
// active one) in a background goroutine. Exit and session-switch paths stay
// non-blocking; distillation happens on the next Build.
func (app *Application) StartMemoryBackfill(activeID string) {
	if app == nil || app.sessionRepo == nil || app.memStore == nil || app.gateway == nil {
		return
	}
	pending, err := app.sessionRepo.ListBackfillCandidates(activeID)
	if err != nil {
		logging.Default().Warn(fmt.Sprintf("[session] memory backfill list failed: %v", err))
		return
	}
	if len(pending) == 0 {
		return
	}
	logging.Default().Info(fmt.Sprintf("[session] background memory backfill: %d unsaved session(s)", len(pending)))

	ids := make([]string, len(pending))
	for i, st := range pending {
		ids[i] = st.ID
	}
	app.backfillWG.Add(1)
	go func() {
		defer app.backfillWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), memoryBackfillTimeout)
		defer cancel()
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				logging.Default().Warn(fmt.Sprintf("[session] memory backfill stopped: %v", err))
				return
			}
			msg, err := app.saveSessionToMemory(ctx, id)
			if err != nil {
				logging.Default().Warn(fmt.Sprintf("[session] backfill %s error: %v", id, err))
			} else if msg != "" {
				logging.Default().Info(fmt.Sprintf("[session] backfill %s: %s", id, msg))
			}
		}
	}()
}

// WaitMemoryBackfill blocks until all background distillation started by
// StartMemoryBackfill has finished. Exit paths intentionally skip this so
// shutdown stays non-blocking; tests should call it before TempDir cleanup.
func (app *Application) WaitMemoryBackfill() {
	if app == nil {
		return
	}
	app.backfillWG.Wait()
}

func (app *Application) saveSessionToMemory(ctx context.Context, sessionID string) (string, error) {
	if app.activeSessionID() == sessionID {
		return "session is now active, skipping backfill.", nil
	}
	st, err := app.sessionRepo.LoadSessionMeta(sessionID)
	if err != nil {
		return "", err
	}
	if st.MemorySaved {
		return "Memory already saved for this session.", nil
	}
	if st.Status == session.StatusArchived {
		return "session is archived, skipping backfill.", nil
	}

	sessionDir, err := app.sessionRepo.SessionDir(sessionID)
	if err != nil {
		return "", err
	}
	histStore, err := history.New(filepath.Join(sessionDir, "history", history.FileName))
	if err != nil {
		return "", fmt.Errorf("open history: %w", err)
	}
	defer histStore.Close()

	entries, err := histStore.ReadAll()
	if err != nil {
		return "", fmt.Errorf("read history: %w", err)
	}
	if app.activeSessionID() == sessionID {
		return "session became active during read, skipping backfill.", nil
	}
	if len(entries) == 0 {
		if err := app.sessionRepo.MarkMemorySaved(sessionID); err != nil {
			return "", err
		}
		return "No history to save.", nil
	}

	histText := formatHistoryForMemory(entries)
	loader := prompt.NewLoader()
	tmpl := loader.Load("session_to_memory")
	if strings.TrimSpace(tmpl) == "" {
		return "", fmt.Errorf("prompt template %q not found", "session_to_memory")
	}
	promptText := prompt.Render(tmpl, map[string]string{
		"session_history": histText,
	})

	modelID := "default"
	if app.cfg != nil && strings.TrimSpace(app.cfg.ModelID) != "" {
		modelID = app.cfg.ModelID
	}
	comp, err := app.gateway.Call(ctx, "lead", llm.CallParams{
		Model:       modelID,
		Messages:    []llm.Message{llm.UserMessage(promptText)},
		Temperature: 0,
	})
	if err != nil {
		return "", fmt.Errorf("LLM call failed: %w", err)
	}
	if app.activeSessionID() == sessionID {
		return "session became active during summarize, skipping backfill.", nil
	}

	items, err := parseMemoryItems(comp.Content)
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		if err := app.sessionRepo.MarkMemorySaved(sessionID); err != nil {
			return "", err
		}
		return "No valuable insights found to save.", nil
	}

	saved := 0
	var summaries []string
	for _, item := range items {
		content := strings.TrimSpace(item["content"])
		category := strings.TrimSpace(item["category"])
		if content == "" {
			continue
		}
		switch category {
		case "preference", "lesson", "fact", "context", "change_log":
		default:
			category = "fact"
		}
		result := app.memStore.Write(content, category)
		if !strings.HasPrefix(result, "Error") {
			saved++
			summaries = append(summaries, fmt.Sprintf("[%s] %s", category, utils.Truncate(content, 80)))
		}
	}
	if err := app.sessionRepo.MarkMemorySaved(sessionID); err != nil {
		return "", err
	}
	if saved == 0 {
		return "No valuable insights found to save.", nil
	}
	return fmt.Sprintf("Saved %d insights to memory:\n- %s", saved, strings.Join(summaries, "\n- ")), nil
}

func (app *Application) activeSessionID() string {
	if app == nil || app.sessionRepo == nil {
		return ""
	}
	idx, err := app.sessionRepo.LoadIndex()
	if err != nil || idx == nil {
		return ""
	}
	return idx.ActiveID
}

func formatHistoryForMemory(entries []history.Entry) string {
	const maxEntries = 100
	start := 0
	if len(entries) > maxEntries {
		start = len(entries) - maxEntries
	}
	var hist strings.Builder
	for _, e := range entries[start:] {
		switch string(e.Kind) {
		case "user":
			fmt.Fprintf(&hist, "[user]: %s\n", utils.Truncate(e.Content, 500))
		case "assistant":
			fmt.Fprintf(&hist, "[assistant]: %s\n", utils.Truncate(e.Content, 500))
		case "tool":
			fmt.Fprintf(&hist, "[tool_result]: %s\n", utils.Truncate(e.Content, 300))
		}
	}
	return hist.String()
}

func parseMemoryItems(raw string) ([]map[string]string, error) {
	content := strings.TrimSpace(raw)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, nil
	}
	var items []map[string]string
	if err := json.Unmarshal([]byte(content), &items); err != nil {
		return nil, fmt.Errorf("parse LLM response: %w, raw=%s", err, utils.Truncate(content, 200))
	}
	return items, nil
}
