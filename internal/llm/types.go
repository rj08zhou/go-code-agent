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

// ReasoningRequest is the minimal opt-in switch for native model reasoning.
// No policy engine: absence (nil) means "provider default, reasoning off".
type ReasoningRequest struct {
	Enabled bool
	// Effort is an optional provider-interpreted hint ("low"/"medium"/"high").
	// Providers ignore values they do not understand.
	Effort string
}

// ReasoningState is a provider-private continuation blob (e.g. encrypted
// reasoning items, thinking signatures). It is bound to the provider
// *instance* and model that produced it and MUST NOT be displayed, logged, or
// persisted. Provider contains a non-secret instance ID, not merely a provider
// type name. Custom JSON marshalling redacts Payload so accidental JSON
// serialization (transcripts, eval dumps, debug logs) cannot leak it.
type ReasoningState struct {
	Provider string
	Model    string
	// Kind is a provider-defined discriminator (e.g. "openai.reasoning_content").
	Kind string
	// Payload is the raw opaque blob, passed back verbatim on continuation.
	Payload json.RawMessage
}

// Compatible reports whether this state may be sent to the given provider
// instance/model. Adapters must drop (not transform) incompatible state.
func (rs *ReasoningState) Compatible(providerInstanceID, model string) bool {
	return rs != nil && rs.Provider == providerInstanceID && rs.Model == model
}

// MarshalJSON never emits Payload: opaque reasoning state is redacted in every
// JSON serialization path by construction.
func (rs ReasoningState) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Provider string `json:"provider,omitempty"`
		Model    string `json:"model,omitempty"`
		Kind     string `json:"kind,omitempty"`
		Redacted bool   `json:"payload_redacted"`
	}{rs.Provider, rs.Model, rs.Kind, len(rs.Payload) > 0})
}

// Reasoning is the provider-neutral reasoning result attached to an assistant
// turn. Summary is eligible for a separate UI display, but this domain layer
// does not render or persist it; State is opaque continuation data.
type Reasoning struct {
	Summary string          `json:"summary,omitempty"`
	State   *ReasoningState `json:"state,omitempty"`
}

type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	// Reasoning carries native reasoning through the current tool loop only.
	// It is intentionally absent from history persistence (history.Entry) —
	// reasoning does not survive session resume.
	Reasoning *Reasoning `json:"reasoning,omitempty"`
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

// StructuredOutput asks a provider to constrain the response to a JSON Schema.
// Providers must reject the request rather than silently ignore this contract.
type StructuredOutput struct {
	Name        string
	Description string
	Schema      map[string]any
}

type CallParams struct {
	Model            string
	Messages         []Message
	Tools            []ToolDef
	MaxTokens        int
	Temperature      float64
	StructuredOutput *StructuredOutput
	// Reasoning opts into native model reasoning. nil = disabled (default,
	// fully backward compatible).
	Reasoning *ReasoningRequest
}

type Usage struct {
	PromptTokens      int64 `json:"prompt_tokens,omitempty"`
	CompletionTokens  int64 `json:"completion_tokens,omitempty"`
	TotalTokens       int64 `json:"total_tokens,omitempty"`
	CachedReadTokens  int64 `json:"cached_read_tokens,omitempty"`
	CacheMissTokens   int64 `json:"cache_miss_tokens,omitempty"`
	CacheCreateTokens int64 `json:"cache_create_tokens,omitempty"`
	// ReasoningTokens counts provider-reported native reasoning tokens.
	// They are typically billed as completion tokens but are not visible text.
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
}

func (u Usage) IsZero() bool {
	return u.PromptTokens == 0 && u.CompletionTokens == 0 &&
		u.TotalTokens == 0 && u.CachedReadTokens == 0 &&
		u.CacheMissTokens == 0 && u.CacheCreateTokens == 0 &&
		u.ReasoningTokens == 0
}

// Add accumulates every usage dimension. Keeping this operation next to the
// type prevents new token categories (such as ReasoningTokens) from being
// silently omitted at individual call sites.
func (u *Usage) Add(other Usage) {
	if u == nil {
		return
	}
	u.PromptTokens += other.PromptTokens
	u.CompletionTokens += other.CompletionTokens
	u.TotalTokens += other.TotalTokens
	u.CachedReadTokens += other.CachedReadTokens
	u.CacheMissTokens += other.CacheMissTokens
	u.CacheCreateTokens += other.CacheCreateTokens
	u.ReasoningTokens += other.ReasoningTokens
}

type Completion struct {
	Content      string     `json:"content,omitempty"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	FinishReason string     `json:"finish_reason,omitempty"`
	Usage        Usage      `json:"usage,omitempty"`
	Reasoning    *Reasoning `json:"reasoning,omitempty"`
}

func (c *Completion) ToAssistantMessage() Message {
	return Message{
		Role:      RoleAssistant,
		Content:   c.Content,
		ToolCalls: append([]ToolCall(nil), c.ToolCalls...),
		Reasoning: c.Reasoning,
	}
}

type StreamResult struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
	Reasoning    *Reasoning
}

func (s *StreamResult) ToAssistantMessage() Message {
	return Message{
		Role:      RoleAssistant,
		Content:   s.Content,
		ToolCalls: append([]ToolCall(nil), s.ToolCalls...),
		Reasoning: s.Reasoning,
	}
}

func EstimateRequestTokens(msgs []Message, tools []ToolDef) int {
	dataMsgs, _ := json.Marshal(msgs)
	if len(tools) == 0 {
		return len(dataMsgs) / 4
	}
	dataTools, _ := json.Marshal(tools)
	return (len(dataMsgs) + len(dataTools)) / 4
}
