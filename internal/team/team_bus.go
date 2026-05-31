package team

import (
	"encoding/json"
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/utils"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// MessageBus - JSONL inbox per agent
//
// Delivery model
// --------------
// Send: append-line to {dir}/{recipient}.jsonl. Atomic at line
// granularity (single Write of marshalled JSON + '\n').
// ReadInbox: read full file then truncate (consume-and-clear). Any
// crash between read and truncate = at-most-once duplication; we
// accept this as the bus is best-effort by design.
//
// Dead-letter queue
// -----------------
// Two failure modes are now captured into {dir}/_deadletter.jsonl:
//  1. Send retried > maxBusSendAttempts and still couldn't write.
//  2. Read found a line that fails JSON parse OR exceeds
//     maxTeamMessageSize — those entries are quarantined instead of
//     silently dropped.
// The DLQ file is append-only, never truncated by the bus itself.

const (
	maxBusSendAttempts = 3
	deadLetterFilename = "_deadletter.jsonl"
)

type MessageBus struct {
	dir string
	mu  sync.Mutex
}

func NewBus(dir string) *MessageBus {
	os.MkdirAll(dir, 0o755)
	return &MessageBus{dir: dir}
}

// writeDeadLetter appends a quarantined entry to the DLQ. Never
// blocks the caller on its own errors — DLQ best-effort.
func (b *MessageBus) writeDeadLetter(entry map[string]any) {
	entry["_dlq_ts"] = time.Now().Unix()
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	path := filepath.Join(b.dir, deadLetterFilename)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(data)
	f.Write([]byte("\n"))
	_ = f.Sync()
}

func (b *MessageBus) Send(sender, to, content, msgType string, extra map[string]any) string {
	msg := map[string]any{
		"type":      msgType,
		"from":      sender,
		"content":   content,
		"timestamp": time.Now().Unix(),
	}
	for k, v := range extra {
		msg[k] = v
	}

	// Reject oversize early so we don't pollute the inbox.
	data, err := json.Marshal(msg)
	if err != nil {
		b.writeDeadLetter(map[string]any{
			"reason": fmt.Sprintf("marshal: %v", err),
			"to":     to,
			"from":   sender,
			"type":   msgType,
		})
		return fmt.Sprintf("Error: marshal failed (%v)", err)
	}
	if len(data) > infra.MaxTeamMessageSize {
		b.writeDeadLetter(map[string]any{
			"reason":  "oversize",
			"size":    len(data),
			"to":      to,
			"from":    sender,
			"type":    msgType,
			"preview": utils.Truncate(content, 200),
		})
		return fmt.Sprintf("Error: message size %d exceeds %d", len(data), infra.MaxTeamMessageSize)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	path := filepath.Join(b.dir, to+".jsonl")
	var lastErr error
	for attempt := 1; attempt <= maxBusSendAttempts; attempt++ {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt) * 50 * time.Millisecond)
			continue
		}
		_, werr := f.Write(append(data, '\n'))
		if werr == nil {
			_ = f.Sync()
		}
		f.Close()
		if werr == nil {
			return fmt.Sprintf("Sent %s to %s", msgType, to)
		}
		lastErr = werr
		time.Sleep(time.Duration(attempt) * 50 * time.Millisecond)
	}

	// All retries exhausted — quarantine.
	b.writeDeadLetter(map[string]any{
		"reason":   fmt.Sprintf("send retry exhausted: %v", lastErr),
		"to":       to,
		"from":     sender,
		"type":     msgType,
		"attempts": maxBusSendAttempts,
		"payload":  msg,
	})
	return fmt.Sprintf("Error: delivery failed after %d attempts (%v)", maxBusSendAttempts, lastErr)
}

func (b *MessageBus) ReadInbox(name string) []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	path := filepath.Join(b.dir, name+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var msgs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			// Quarantine corrupt line so it doesn't get re-read or lost.
			b.writeDeadLetter(map[string]any{
				"reason": fmt.Sprintf("parse: %v", err),
				"to":     name,
				"raw":    utils.Truncate(line, 500),
			})
			continue
		}
		if len(line) > infra.MaxTeamMessageSize {
			b.writeDeadLetter(map[string]any{
				"reason":  "oversize on read",
				"to":      name,
				"size":    len(line),
				"payload": m,
			})
			continue
		}
		msgs = append(msgs, m)
	}
	// Truncate atomically (rename pattern not needed: we hold the lock).
	if err := os.WriteFile(path, []byte(""), 0o644); err == nil {
		if f, ferr := os.OpenFile(path, os.O_WRONLY, 0o644); ferr == nil {
			_ = f.Sync()
			f.Close()
		}
	}
	return msgs
}

func (b *MessageBus) Broadcast(sender, content string, names []string) string {
	count := 0
	for _, n := range names {
		if n != sender {
			b.Send(sender, n, content, "broadcast", nil)
			count++
		}
	}
	return fmt.Sprintf("Broadcast to %d teammates", count)
}


