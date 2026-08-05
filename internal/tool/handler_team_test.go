package tool

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type recordingTeamService struct {
	spawnName   string
	spawnRole   string
	spawnPrompt string
	members     []string
}

func (f *recordingTeamService) Spawn(ctx context.Context, name, role, prompt string) string {
	f.spawnName, f.spawnRole, f.spawnPrompt = name, role, prompt
	return "spawned " + name
}
func (f *recordingTeamService) ListAll() string {
	if len(f.members) == 0 {
		return "No teammates."
	}
	return "Team: default"
}
func (f *recordingTeamService) MemberNames() []string {
	return append([]string(nil), f.members...)
}

type recordingMessageBus struct {
	from, to, content, msgType string
	broadcastFrom              string
	broadcastContent           string
	broadcastRecipients        []string
	inbox                      []map[string]any
}

func (f *recordingMessageBus) Send(from, to, content, msgType string, meta map[string]any) string {
	f.from, f.to, f.content, f.msgType = from, to, content, msgType
	return "sent"
}
func (f *recordingMessageBus) ReadInbox(id string) []map[string]any { return f.inbox }
func (f *recordingMessageBus) Broadcast(from, content string, recipients []string) string {
	f.broadcastFrom, f.broadcastContent, f.broadcastRecipients = from, content, recipients
	return "broadcast ok"
}

func TestTeamTools(t *testing.T) {
	scope := &ToolScope{AgentID: "lead"}

	t.Run("spawn_teammate requires name and prompt", func(t *testing.T) {
		tool := mustTool(t, teamTools(builtinDeps{teamSvc: &recordingTeamService{}}), "spawn_teammate")
		got := tool.Handler(scope, json.RawMessage(`{"name":"","prompt":""}`))
		if got.Status != StatusFailed || !strings.Contains(got.Output, "name and prompt are required") {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("spawn_teammate delegates", func(t *testing.T) {
		team := &recordingTeamService{}
		tool := mustTool(t, teamTools(builtinDeps{teamSvc: team}), "spawn_teammate")
		got := tool.Handler(scope, json.RawMessage(`{"name":"alice","role":"coder","prompt":"fix it"}`))
		if got.Status != StatusSucceeded || !strings.Contains(got.Output, "spawned alice") {
			t.Fatalf("got %#v", got)
		}
		if team.spawnName != "alice" || team.spawnRole != "coder" || team.spawnPrompt != "fix it" {
			t.Fatalf("spawn = %+v", team)
		}
	})

	t.Run("spawn_teammate nil service", func(t *testing.T) {
		tool := mustTool(t, teamTools(builtinDeps{}), "spawn_teammate")
		got := tool.Handler(scope, json.RawMessage(`{"name":"a","prompt":"p"}`))
		if got.Status != StatusFailed || !strings.Contains(got.Output, "team spawn unavailable") {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("send_message requires fields", func(t *testing.T) {
		tool := mustTool(t, teamTools(builtinDeps{bus: &recordingMessageBus{}}), "send_message")
		got := tool.Handler(scope, json.RawMessage(`{"to":"","content":""}`))
		if got.Status != StatusFailed || !strings.Contains(got.Output, "to and content are required") {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("send_message delegates with AgentID", func(t *testing.T) {
		bus := &recordingMessageBus{}
		tool := mustTool(t, teamTools(builtinDeps{bus: bus}), "send_message")
		got := tool.Handler(scope, json.RawMessage(`{"to":"alice","content":"hello"}`))
		if got.Status != StatusSucceeded {
			t.Fatalf("got %#v", got)
		}
		if bus.from != "lead" || bus.to != "alice" || bus.content != "hello" || bus.msgType != "message" {
			t.Fatalf("send = %+v", bus)
		}
	})

	t.Run("read_inbox empty", func(t *testing.T) {
		bus := &recordingMessageBus{}
		tool := mustTool(t, teamTools(builtinDeps{bus: bus}), "read_inbox")
		got := tool.Handler(scope, json.RawMessage(`{}`))
		if got.Status != StatusSucceeded || got.Output != "[]" {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("read_inbox returns messages", func(t *testing.T) {
		bus := &recordingMessageBus{inbox: []map[string]any{{"from": "alice", "content": "hi"}}}
		tool := mustTool(t, teamTools(builtinDeps{bus: bus}), "read_inbox")
		got := tool.Handler(scope, json.RawMessage(`{}`))
		if got.Status != StatusSucceeded || !strings.Contains(got.Output, "alice") {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("broadcast no teammates", func(t *testing.T) {
		team := &recordingTeamService{}
		bus := &recordingMessageBus{}
		tool := mustTool(t, teamTools(builtinDeps{teamSvc: team, bus: bus}), "broadcast")
		got := tool.Handler(scope, json.RawMessage(`{"content":"ping"}`))
		if got.Status != StatusSucceeded || !strings.Contains(got.Output, "No teammates") {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("broadcast uses MemberNames", func(t *testing.T) {
		team := &recordingTeamService{members: []string{"Alice", "Bob"}}
		bus := &recordingMessageBus{}
		tool := mustTool(t, teamTools(builtinDeps{teamSvc: team, bus: bus}), "broadcast")
		got := tool.Handler(scope, json.RawMessage(`{"content":"ping"}`))
		if got.Status != StatusSucceeded || !strings.Contains(got.Output, "broadcast ok") {
			t.Fatalf("got %#v", got)
		}
		if bus.broadcastFrom != "lead" || bus.broadcastContent != "ping" {
			t.Fatalf("broadcast meta = %+v", bus)
		}
		want := []string{"Alice", "Bob"}
		if !reflect.DeepEqual(bus.broadcastRecipients, want) {
			t.Fatalf("recipients = %#v, want %#v", bus.broadcastRecipients, want)
		}
	})

	t.Run("broadcast unavailable without bus", func(t *testing.T) {
		tool := mustTool(t, teamTools(builtinDeps{teamSvc: &recordingTeamService{}}), "broadcast")
		got := tool.Handler(scope, json.RawMessage(`{"content":"ping"}`))
		if got.Status != StatusFailed || !strings.Contains(got.Output, "broadcast unavailable") {
			t.Fatalf("got %#v", got)
		}
	})
}
