package agent

import (
	"context"
	"go-code-agent-refactor/internal/llm"
	"go-code-agent-refactor/internal/model"
	"testing"
)

type lessonProvider struct{}

func (lessonProvider) Name() string { return "lesson-test" }
func (lessonProvider) Call(context.Context, llm.CallParams) (*llm.Completion, error) {
	return &llm.Completion{Content: "Prefer atomic writes and validate paths before mutation."}, nil
}
func (lessonProvider) Stream(context.Context, llm.CallParams, model.StreamSink) (*llm.StreamResult, error) {
	return &llm.StreamResult{Content: "unused"}, nil
}

type lessonMemory struct{ content, category string }

func (m *lessonMemory) Write(content, category string) string {
	m.content, m.category = content, category
	return "saved"
}
func (*lessonMemory) Search(string, int, int, string) string { return "No relevant memories found." }

func TestLLMLessonWriter_RecordFailureUsesLLMAndMemory(t *testing.T) {
	gw := model.NewGateway(lessonProvider{}, model.NewRoleThrottle(10))
	mem := &lessonMemory{}
	writer := NewLLMLessonWriter(gw, mem, nil, "lesson-model")
	writer.RecordFailure(context.Background(), []llm.Message{
		llm.UserMessage("Implement a safe file update"),
		llm.ToolMessage("permission denied", "call-1"),
	})
	if mem.category != "lesson" || mem.content == "" {
		t.Fatalf("lesson was not persisted: %#v", mem)
	}
}
