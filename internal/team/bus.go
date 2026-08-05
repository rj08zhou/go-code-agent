// Package team manages teammate coordination and messaging.
package team

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"go-code-agent/internal/store"
)

// MessageBus handles inter-agent messaging via JSONL inboxes.
type MessageBus struct {
	dir string
	mu  sync.Mutex
}

func NewBus(dir string) *MessageBus {
	_ = store.EnsurePrivateDir(dir)
	return &MessageBus{dir: dir}
}

// validateBusAgentID rejects empty IDs and any value that could escape the
// inbox directory when used as a path component.
func validateBusAgentID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("empty agent id")
	}
	if id == "." || id == ".." {
		return fmt.Errorf("invalid agent id %q", id)
	}
	if strings.ContainsRune(id, 0) {
		return fmt.Errorf("invalid agent id %q: null byte", id)
	}
	if strings.ContainsAny(id, `/\`) || strings.ContainsRune(id, filepath.Separator) {
		return fmt.Errorf("invalid agent id %q: path separators not allowed", id)
	}
	return nil
}

func (b *MessageBus) inboxPath(id string) (string, error) {
	if err := validateBusAgentID(id); err != nil {
		return "", err
	}
	root := filepath.Clean(b.dir)
	path := filepath.Join(root, id+".jsonl")
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid agent id %q: path escapes inbox", id)
	}
	if filepath.Base(path) != id+".jsonl" {
		return "", fmt.Errorf("invalid agent id %q", id)
	}
	return path, nil
}

func (b *MessageBus) Send(from, to, content, msgType string, meta map[string]any) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sendLocked(from, to, content, msgType, meta)
}

func (b *MessageBus) sendLocked(from, to, content, msgType string, meta map[string]any) string {
	path, err := b.inboxPath(to)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	if msgType == "" {
		msgType = "message"
	}
	msg := map[string]any{
		"from":    from,
		"to":      to,
		"type":    msgType,
		"content": content,
	}
	for k, v := range meta {
		msg[k] = v
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	f, err := store.OpenPrivateAppend(path)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	return fmt.Sprintf("Sent to %s", to)
}

func (b *MessageBus) ReadInbox(id string) []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	path, err := b.inboxPath(id)
	if err != nil {
		return nil
	}
	f, err := os.Open(path)
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
	// Drain inbox.
	_ = os.Truncate(path, 0)
	return msgs
}

func (b *MessageBus) Broadcast(from, content string, recipients []string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var results []string
	for _, to := range recipients {
		results = append(results, b.sendLocked(from, to, content, "broadcast", nil))
	}
	return strings.Join(results, "\n")
}
