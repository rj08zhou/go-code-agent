package agent

import (
	"encoding/json"
	"fmt"
	"go-code-agent-refactor/internal/llm"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// UsageTracker records LLM token usage per session.
type UsageTracker struct {
	path string
	mu   sync.Mutex
	file *os.File
}

func NewUsageTracker(sessionDir string) (*UsageTracker, error) {
	path := filepath.Join(sessionDir, "usage.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &UsageTracker{path: path, file: f}, nil
}

func (u *UsageTracker) Record(source, role, model, traceID string, usage llm.Usage, durationSec float64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	entry := map[string]interface{}{
		"source":              source,
		"role":                role,
		"model":               model,
		"trace_id":            traceID,
		"in_tokens":           usage.PromptTokens,
		"out_tokens":          usage.CompletionTokens,
		"cached_read_tokens":  usage.CachedReadTokens,
		"cache_miss_tokens":   usage.CacheMissTokens,
		"cache_create_tokens": usage.CacheCreateTokens,
		"total":               usage.TotalTokens,
		"duration_s":          fmt.Sprintf("%.2f", durationSec),
	}
	data, _ := json.Marshal(entry)
	u.file.Write(append(data, '\n'))
}

func (u *UsageTracker) Close() error { return u.file.Close() }

// Render reads and returns a summary of recorded usage.
func (u *UsageTracker) Render() string {
	data, err := os.ReadFile(u.path)
	if err != nil || len(data) == 0 {
		return "No usage recorded."
	}
	lines := strings.Split(string(data), "\n")
	var totalIn, totalOut, totalHit, totalMiss, totalCreate, count int64
	for _, line := range lines {
		if line == "" {
			continue
		}
		var e map[string]interface{}
		if json.Unmarshal([]byte(line), &e) == nil {
			if in, ok := e["in_tokens"].(float64); ok {
				totalIn += int64(in)
			}
			if out, ok := e["out_tokens"].(float64); ok {
				totalOut += int64(out)
			}
			if cp, ok := e["cached_read_tokens"].(float64); ok {
				totalHit += int64(cp)
			}
			if cm, ok := e["cache_miss_tokens"].(float64); ok {
				totalMiss += int64(cm)
			}
			if cc, ok := e["cache_create_tokens"].(float64); ok {
				totalCreate += int64(cc)
			}
			count++
		}
	}
	input := totalIn
	if input == 0 {
		input = totalHit + totalMiss
	}
	rate := float64(0)
	if input > 0 {
		rate = float64(totalHit) / float64(input) * 100
	}
	return fmt.Sprintf("Usage: %d calls, %d in, %d out, %d total, cache hit %d, miss %d, create %d (%.1f%%)",
		count, totalIn, totalOut, totalIn+totalOut, totalHit, totalMiss, totalCreate, rate)
}
