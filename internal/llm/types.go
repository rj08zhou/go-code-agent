// Package llm defines provider-independent LLM data types.
// These are the lingua franca for the entire project.
package llm

import "encoding/json"

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

func SystemMessage(content string) Message {
	return Message{Role: RoleSystem, Content: content}
}
func UserMessage(content string) Message {
	return Message{Role: RoleUser, Content: content}
}
func AssistantMessage(content string) Message {
	return Message{Role: RoleAssistant, Content: content}
}
func ToolMessage(content, toolCallID string) Message {
	return Message{Role: RoleTool, Content: content, ToolCallID: toolCallID}
}

type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type CallParams struct {
	Model       string
	Messages    []Message
	Tools       []ToolDef
	MaxTokens   int
	Temperature float64
}

type Usage struct {
	PromptTokens      int64 `json:"prompt_tokens,omitempty"`
	CompletionTokens  int64 `json:"completion_tokens,omitempty"`
	TotalTokens       int64 `json:"total_tokens,omitempty"`
	CachedReadTokens  int64 `json:"cached_read_tokens,omitempty"`
	CacheMissTokens   int64 `json:"cache_miss_tokens,omitempty"`
	CacheCreateTokens int64 `json:"cache_create_tokens,omitempty"`
}

func (u Usage) IsZero() bool {
	return u.PromptTokens == 0 && u.CompletionTokens == 0 &&
		u.TotalTokens == 0 && u.CachedReadTokens == 0 &&
		u.CacheMissTokens == 0 && u.CacheCreateTokens == 0
}

type Completion struct {
	Content      string     `json:"content,omitempty"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	FinishReason string     `json:"finish_reason,omitempty"`
	Usage        Usage      `json:"usage,omitempty"`
}

func (c *Completion) ToAssistantMessage() Message {
	return Message{
		Role:      RoleAssistant,
		Content:   c.Content,
		ToolCalls: append([]ToolCall(nil), c.ToolCalls...),
	}
}

type StreamResult struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
}

func (s *StreamResult) ToAssistantMessage() Message {
	return Message{
		Role:      RoleAssistant,
		Content:   s.Content,
		ToolCalls: append([]ToolCall(nil), s.ToolCalls...),
	}
}

func EstimateTokens(msgs []Message) int {
	data, _ := json.Marshal(msgs)
	return len(data) / 4
}

func EstimateRequestTokens(msgs []Message, tools []ToolDef) int {
	dataMsgs, _ := json.Marshal(msgs)
	if len(tools) == 0 {
		return len(dataMsgs) / 4
	}
	dataTools, _ := json.Marshal(tools)
	return (len(dataMsgs) + len(dataTools)) / 4
}
