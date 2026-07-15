package llm

import (
	"encoding/json"
)

// Neutral LLM data model - provider-agnostic messages & tools.
//
// These types are the lingua franca for the entire project. A Provider
// translates them to/from a concrete vendor SDK.

// Role is the message author role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall is a single tool invocation emitted by the model.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Message is a single turn in the conversation.
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

// ToolDef is a tool definition exposed to the model (JSON Schema).
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// CallParams is the request envelope passed to Provider.Call / Stream.
type CallParams struct {
	Model    string
	Messages []Message
	Tools    []ToolDef

	// Optional overrides. Zero values mean "provider default".
	MaxTokens   int
	Temperature float64
}

// Usage is the neutral token-usage report for a single LLM call.
type Usage struct {
	PromptTokens      int64 `json:"prompt_tokens,omitempty"`
	CompletionTokens  int64 `json:"completion_tokens,omitempty"`
	TotalTokens       int64 `json:"total_tokens,omitempty"`
	CachedReadTokens  int64 `json:"cached_read_tokens,omitempty"`
	CacheCreateTokens int64 `json:"cache_create_tokens,omitempty"`
}

// IsZero reports whether the usage record carries no signal.
func (u Usage) IsZero() bool {
	return u.PromptTokens == 0 && u.CompletionTokens == 0 &&
		u.TotalTokens == 0 && u.CachedReadTokens == 0 && u.CacheCreateTokens == 0
}

// Completion is the non-streaming response shape.
type Completion struct {
	Content      string     `json:"content,omitempty"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	FinishReason string     `json:"finish_reason,omitempty"`
	Usage        Usage      `json:"usage,omitempty"`
}

// ToAssistantMessage converts a Completion into an assistant Message.
func (c *Completion) ToAssistantMessage() Message {
	return Message{
		Role:      RoleAssistant,
		Content:   c.Content,
		ToolCalls: append([]ToolCall(nil), c.ToolCalls...),
	}
}

// StreamResult is the accumulated result of a streaming call.
type StreamResult struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
}

// ToAssistantMessage mirrors Completion.ToAssistantMessage for streaming.
func (s *StreamResult) ToAssistantMessage() Message {
	return Message{
		Role:      RoleAssistant,
		Content:   s.Content,
		ToolCalls: append([]ToolCall(nil), s.ToolCalls...),
	}
}

// EstimateTokens is a rough token estimator (len(json)/4).
func EstimateTokens(msgs []Message) int {
	data, _ := json.Marshal(msgs)
	return len(data) / 4
}

// EstimateRequestTokens is like EstimateTokens but also accounts for tool
// definitions, which are sent verbatim on every request. Omitting tools from
// the estimate caused AutoCompact to under-count, especially with MCP tools.
func EstimateRequestTokens(msgs []Message, tools []ToolDef) int {
	dataMsgs, _ := json.Marshal(msgs)
	if len(tools) == 0 {
		return len(dataMsgs) / 4
	}
	dataTools, _ := json.Marshal(tools)
	return (len(dataMsgs) + len(dataTools)) / 4
}
