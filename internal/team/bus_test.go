package team

import (
	"testing"
)

func TestMessageBus_SendAndRead(t *testing.T) {
	dir := t.TempDir()
	bus := NewBus(dir)

	// Send a message from alice to bob
	result := bus.Send("alice", "bob", "Hello Bob!", "message", nil)
	if result == "" || result[:5] == "Error" {
		t.Fatalf("send failed: %s", result)
	}

	// Read bob's inbox
	msgs := bus.ReadInbox("bob")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0]["from"] != "alice" {
		t.Errorf("expected from=alice, got %v", msgs[0]["from"])
	}
	if msgs[0]["content"] != "Hello Bob!" {
		t.Errorf("expected content='Hello Bob!', got %v", msgs[0]["content"])
	}

	// Second read should be empty (drained inbox)
	msgs2 := bus.ReadInbox("bob")
	if len(msgs2) != 0 {
		t.Errorf("expected empty inbox after drain, got %d", len(msgs2))
	}
}

func TestMessageBus_MultipleMessages(t *testing.T) {
	dir := t.TempDir()
	bus := NewBus(dir)

	bus.Send("x", "y", "msg1", "", nil)
	bus.Send("x", "y", "msg2", "", nil)
	bus.Send("z", "y", "msg3", "", nil)

	msgs := bus.ReadInbox("y")
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
}

func TestMessageBus_Broadcast(t *testing.T) {
	dir := t.TempDir()
	bus := NewBus(dir)

	result := bus.Broadcast("lead", "meeting in 5 min", []string{"alice", "bob", "charlie"})
	if result == "" {
		t.Fatal("broadcast returned empty")
	}

	for _, name := range []string{"alice", "bob", "charlie"} {
		msgs := bus.ReadInbox(name)
		if len(msgs) != 1 {
			t.Errorf("%s: expected 1 message, got %d", name, len(msgs))
		}
	}
}

func TestMessageBus_EmptyInbox(t *testing.T) {
	dir := t.TempDir()
	bus := NewBus(dir)

	msgs := bus.ReadInbox("nobody")
	if len(msgs) != 0 {
		t.Errorf("expected empty inbox, got %d", len(msgs))
	}
}

func TestMessageBus_ShutdownRequest(t *testing.T) {
	dir := t.TempDir()
	bus := NewBus(dir)

	result := bus.Send("lead", "teammate1", "", "shutdown_request", nil)
	if result == "" || result[:5] == "Error" {
		t.Fatalf("shutdown request failed: %s", result)
	}

	msgs := bus.ReadInbox("teammate1")
	if len(msgs) != 1 {
		t.Fatal("shutdown request not received")
	}
	if msgs[0]["type"] != "shutdown_request" {
		t.Errorf("expected shutdown_request type, got %v", msgs[0]["type"])
	}
}

func TestProtocolStore_HasApprovedPlan(t *testing.T) {
	bus := NewBus(t.TempDir())
	ps := NewProtocolStore(bus)
	if HasApprovedPlan(ps, "newborn") {
		t.Error("newborn agent should not have an approved plan")
	}
}
