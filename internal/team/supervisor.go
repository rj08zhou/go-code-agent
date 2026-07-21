// Package team manages teammate lifecycle and message routing.
package team

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Actor represents a teammate's runtime state.
type Actor struct {
	ID     string
	Name   string
	Role   string
	Status string // "idle", "working"
}

// MessageBus handles inter-agent messaging via JSONL inboxes.
type MessageBus struct {
	dir string
	mu  sync.Mutex
}

func NewBus(dir string) *MessageBus {
	os.MkdirAll(dir, 0o755)
	return &MessageBus{dir: dir}
}

func (b *MessageBus) inboxPath(id string) string {
	return filepath.Join(b.dir, id+".jsonl")
}

func (b *MessageBus) Send(from, to, content, msgType string, meta map[string]any) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if msgType == "" {
		msgType = "message"
	}
	msg := map[string]any{
		"from":    from,
		"to":      to,
		"type":    msgType,
		"content": content,
	}
	if meta != nil {
		for k, v := range meta {
			msg[k] = v
		}
	}
	data, _ := json.Marshal(msg)
	f, err := os.OpenFile(b.inboxPath(to), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	defer f.Close()
	f.Write(append(data, '\n'))
	return fmt.Sprintf("Sent to %s", to)
}

func (b *MessageBus) ReadInbox(id string) []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	f, err := os.Open(b.inboxPath(id))
	if err != nil {
		return nil
	}
	defer f.Close()
	var msgs []map[string]any
	dec := json.NewDecoder(f)
	for dec.More() {
		var m map[string]any
		if dec.Decode(&m) == nil {
			msgs = append(msgs, m)
		}
	}
	// Drain inbox
	os.Truncate(b.inboxPath(id), 0)
	return msgs
}

func (b *MessageBus) Broadcast(from, content string, recipients []string) string {
	var results []string
	for _, to := range recipients {
		results = append(results, b.Send(from, to, content, "broadcast", nil))
	}
	return strings.Join(results, "\n")
}

// Supervisor manages teammate lifecycle (Spawn, Stop, List).
type Supervisor struct {
	mu     sync.Mutex
	actors map[string]*Actor
}

func NewSupervisor() *Supervisor {
	return &Supervisor{actors: make(map[string]*Actor)}
}

func (s *Supervisor) Spawn(ctx context.Context, name, role, prompt string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Generate safe ID
	id := "tm_" + strings.ReplaceAll(strings.ToLower(name), " ", "_")

	if _, ok := s.actors[id]; ok {
		return fmt.Sprintf("Teammate %s already exists", name)
	}

	s.actors[id] = &Actor{
		ID:     id,
		Name:   name,
		Role:   role,
		Status: "idle",
	}
	return fmt.Sprintf("Spawned teammate %s (%s): %s", name, id, role)
}

func (s *Supervisor) Stop(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := "tm_" + strings.ReplaceAll(strings.ToLower(name), " ", "_")
	if _, ok := s.actors[id]; ok {
		delete(s.actors, id)
		return fmt.Sprintf("Stopped teammate %s", name)
	}
	return fmt.Sprintf("Teammate %s not found", name)
}

func (s *Supervisor) ListAll() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.actors) == 0 {
		return "No teammates."
	}
	var lines []string
	for _, a := range s.actors {
		lines = append(lines, fmt.Sprintf("[%s] %s (%s) - %s", a.Status, a.Name, a.ID, a.Role))
	}
	return strings.Join(lines, "\n")
}

func (s *Supervisor) MemberNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.actors))
	for _, a := range s.actors {
		names = append(names, a.Name)
	}
	return names
}
