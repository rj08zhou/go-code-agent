package repl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-code-agent/internal/history"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/utils"
)

func formatTurnOutcomeError(turnErr, sessionErr error) string {
	if turnErr == nil {
		return ""
	}
	if errors.Is(turnErr, context.Canceled) {
		if sessionErr == nil {
			return utils.Dim + "[interrupted] current turn stopped" + utils.Reset
		}
		return ""
	}
	return fmt.Sprintf("%s[error]%s %v", utils.Red, utils.Reset, turnErr)
}

func persistUserHistory(histStore *history.Store, content string) error {
	if err := histStore.AppendUser(content); err != nil {
		return fmt.Errorf("append user message: %w", err)
	}
	if err := histStore.Sync(); err != nil {
		return fmt.Errorf("sync user message: %w", err)
	}
	return nil
}

func persistAgentHistory(histStore *history.Store, messages []llm.Message) error {
	if err := appendHistoryMessages(histStore, messages); err != nil {
		return err
	}
	if err := histStore.Sync(); err != nil {
		return fmt.Errorf("sync agent output: %w", err)
	}
	return nil
}

func formatHistorySaveWarning(err error) string {
	detail := strings.Join(strings.Fields(err.Error()), " ")
	return fmt.Sprintf("%s[warn]%s history save failed; continuing in memory (this turn may not be restorable): %s",
		utils.Yellow, utils.Reset, utils.Truncate(detail, 300))
}

func appendHistoryMessages(histStore *history.Store, messages []llm.Message) error {
	for _, message := range messages {
		switch message.Role {
		case llm.RoleAssistant:
			if err := histStore.AppendAssistant(message.Content, message.ToolCalls); err != nil {
				return fmt.Errorf("append assistant message: %w", err)
			}
		case llm.RoleTool:
			if err := histStore.AppendTool(message.ToolCallID, message.Content); err != nil {
				return fmt.Errorf("append tool result %q: %w", message.ToolCallID, err)
			}
		}
	}
	return nil
}

func summarizeMessages(messages []llm.Message) string {
	var parts []string
	for i := len(messages) - 1; i >= 0 && len(parts) < 5; i-- {
		if messages[i].Role == llm.RoleUser || messages[i].Role == llm.RoleAssistant {
			text := strings.TrimSpace(messages[i].Content)
			if text != "" {
				parts = append(parts, utils.Truncate(text, 300))
			}
		}
	}
	return strings.Join(parts, "\n")
}
