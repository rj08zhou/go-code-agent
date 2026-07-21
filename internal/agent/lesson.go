package agent

import (
	"context"
	"fmt"
	"go-code-agent-refactor/internal/config"
	"go-code-agent-refactor/internal/llm"
	"go-code-agent-refactor/internal/model"
	"go-code-agent-refactor/internal/prompt"
	"strings"
)

// LessonMemory is the minimal memory boundary needed by the auto-lesson flow.
type LessonMemory interface {
	Write(content, category string) string
	Search(query string, topK, withinDays int, category string) string
}

// LLMLessonWriter turns a sufficiently long or error-prone run into a durable
// lesson. It deliberately performs one bounded, non-tool LLM call and writes
// only the resulting summary to the lesson category.
type LLMLessonWriter struct {
	gateway *model.Gateway
	memory  LessonMemory
	prompts *prompt.Loader
	modelID string
}

func NewLLMLessonWriter(gateway *model.Gateway, memory LessonMemory, prompts *prompt.Loader, modelID string) *LLMLessonWriter {
	return &LLMLessonWriter{gateway: gateway, memory: memory, prompts: prompts, modelID: modelID}
}

func (w *LLMLessonWriter) RecordFailure(parent context.Context, messages []llm.Message) {
	if w == nil || w.gateway == nil || w.memory == nil || len(messages) == 0 {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, config.LlmCallTimeout)
	defer cancel()

	instruction := "Extract durable, project-specific lessons from this coding-agent run. " +
		"Return a concise lesson only; do not include secrets, credentials, or conversational filler.\n\n"
	if w.prompts != nil {
		if tmpl := strings.TrimSpace(w.prompts.Load("auto_lesson")); tmpl != "" {
			instruction = tmpl + "\n\n"
		}
	}
	transcript := renderLessonMessages(messages)
	resp, err := w.gateway.Call(ctx, "lesson", llm.CallParams{
		Model:     w.modelID,
		Messages:  []llm.Message{llm.UserMessage(instruction + transcript)},
		MaxTokens: 1200,
	})
	if err != nil || resp == nil {
		return
	}
	lesson := strings.TrimSpace(resp.Content)
	if lesson == "" || strings.EqualFold(lesson, "(none)") {
		return
	}
	if len(lesson) > config.MaxMemoryContentLen {
		lesson = lesson[:config.MaxMemoryContentLen]
	}
	if w.HasLesson(lesson) {
		return
	}
	w.memory.Write(lesson, "lesson")
}

func (w *LLMLessonWriter) HasLesson(issue string) bool {
	if w == nil || w.memory == nil || strings.TrimSpace(issue) == "" {
		return false
	}
	result := w.memory.Search(issue, 1, config.MemoryTTLDays, "lesson")
	return result != "" && result != "No relevant memories found."
}

func renderLessonMessages(messages []llm.Message) string {
	var b strings.Builder
	for _, m := range messages {
		if m.Role == llm.RoleSystem {
			continue
		}
		content := strings.TrimSpace(m.Content)
		if len(content) > 800 {
			content = content[:800]
		}
		switch m.Role {
		case llm.RoleUser:
			fmt.Fprintf(&b, "[user] %s\n", content)
		case llm.RoleAssistant:
			fmt.Fprintf(&b, "[assistant] %s\n", content)
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "[tool-call] %s %s\n", tc.Name, truncateLesson(tc.Arguments, 300))
			}
		case llm.RoleTool:
			fmt.Fprintf(&b, "[tool-result] %s\n", truncateLesson(content, 800))
		}
	}
	return b.String()
}

func truncateLesson(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
