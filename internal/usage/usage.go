package usage

import (
	"encoding/json"
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/llm"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Token-usage telemetry.
//
// Every successful LLM call yields a Usage struct. UsageRecorder:
//   1. Appends one JSONL row per call to {workdir}/memory/usage.jsonl
//   2. Maintains in-memory rollups (by source, by model) for /usage command
//
// Exempt from the 7-day TTL sweep. Best-effort writes.

// UsageEntry is one row in usage.jsonl.
//
// Field names are short and stable; downstream tooling (jq, log
// shippers) keys off them.
type UsageEntry struct {
	Timestamp         string `json:"ts"`                          // RFC3339 UTC
	SessionID         string `json:"session_id"`                  // active session at call time, "" if none
	TraceID           string `json:"trace_id,omitempty"`          // correlates all LLM calls for one user request
	Source            string `json:"source"`                      // see "Source taxonomy" above
	Provider          string `json:"provider"`                    // openai | anthropic | gemini
	Model             string `json:"model"`                       // model id used for this call
	PromptTokens      int64  `json:"prompt_tokens,omitempty"`     // input tokens billed
	CompletionTokens  int64  `json:"completion_tokens,omitempty"` // output tokens billed
	TotalTokens       int64  `json:"total_tokens,omitempty"`      // prompt + completion (canonical)
	CachedReadTokens  int64  `json:"cached_read_tokens,omitempty"`
	CacheCreateTokens int64  `json:"cache_create_tokens,omitempty"`
	DurationMs        int64  `json:"duration_ms,omitempty"` // wall-clock ms for the LLM call (optional)
}

// usageRollup is the in-memory aggregation backing /usage. We track
// rollups along two independent axes (by source, by model) plus a
// global session-wide total. Counts of CALLS are kept alongside token
// totals so operators can spot "10k calls of 50 tokens each" patterns.
type usageRollup struct {
	Calls       int64
	Prompt      int64
	Completion  int64
	Total       int64
	CachedRead  int64
	CacheCreate int64
}

func (r *usageRollup) add(u llm.Usage) {
	r.Calls++
	r.Prompt += u.PromptTokens
	r.Completion += u.CompletionTokens
	r.Total += u.TotalTokens
	r.CachedRead += u.CachedReadTokens
	r.CacheCreate += u.CacheCreateTokens
}

// validUsageSources is checked by Record so a typo at a call site
// (e.g. "agnet") surfaces as a one-time stderr warning AND as a
// quarantined "(unknown:agnet)" bucket in the rollup. We don't reject
// the call - data fidelity beats taxonomic purity.
var validUsageSources = map[string]bool{
	"agent":       true,
	"agent-final": true,
	"judge":       true,
	"subagent":    true,
	"team":        true,
	"compress":    true,
	"memory-save": true,
}

// UsageRecorder owns the usage.jsonl file and in-memory rollups.
type UsageRecorder struct {
	mu       sync.Mutex
	path     string // absolute file path; empty until Init succeeds
	disabled bool   // true if Init failed; subsequent Record calls become no-ops

	// In-memory rollups for /usage command (not persisted).
	bySource map[string]*usageRollup
	byModel  map[string]*usageRollup
	total    usageRollup
}

// usageRecorder is the package-level singleton. main() calls
// InitUsageRecorder once after the workspace is known.
var usageRecorder = &UsageRecorder{
	bySource: map[string]*usageRollup{},
	byModel:  map[string]*usageRollup{},
}

// sessionIDFn is a callback that returns the current active session ID.
// Set by the main package via SetSessionIDFunc to avoid circular imports.
var sessionIDFn func() string

// SetSessionIDFunc registers a callback that returns the active session ID.
// Called once at startup from main.
func SetSessionIDFunc(fn func() string) {
	sessionIDFn = fn
}

// Record is the package-level convenience function that delegates to
// the singleton usageRecorder. Sub-packages (e.g. internal/llm) call
// this instead of reaching into the unexported singleton directly.
func Record(source, provider, model, traceID string, u llm.Usage, dur time.Duration) {
	usageRecorder.Record(source, provider, model, traceID, u, dur)
}

// Render returns a human-readable summary of usage rollups.
func Render() string {
	return usageRecorder.Render()
}

// InitUsageRecorder configures the usage log location. Safe to call
// more than once (e.g. on workdir change), though in practice we call
// it once at startup. workdir is the workspace root, NOT the memory
// subdir.
func InitUsageRecorder(workdir string) {
	if workdir == "" {
		usageRecorder.disabled = true
		return
	}
	dir := filepath.Join(workdir, infra.AppRootDirName, "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "[usage] cannot create %s: %v (telemetry disabled)\n", dir, err)
		usageRecorder.disabled = true
		return
	}
	usageRecorder.mu.Lock()
	usageRecorder.path = filepath.Join(dir, "usage.jsonl")
	usageRecorder.disabled = false
	usageRecorder.mu.Unlock()
}

// Record appends one usage row and updates rollups (best-effort).
func (r *UsageRecorder) Record(source, provider, model, traceID string, u llm.Usage, dur time.Duration) {
	if r == nil || u.IsZero() {
		return
	}

	// Defensive: derive TotalTokens if a provider didn't fill it.
	// Same convention applied in provider_anthropic.go's stream path.
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}

	if source == "" {
		source = "unknown"
	} else if !validUsageSources[source] {
		// Quarantine unknown sources so a typo is visible but the row
		// still persists.
		source = "(unknown:" + source + ")"
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Update rollups regardless of whether file IO succeeds: in-memory
	// stats should reflect what actually happened on the wire.
	r.total.add(u)
	if r.bySource[source] == nil {
		r.bySource[source] = &usageRollup{}
	}
	r.bySource[source].add(u)
	if model != "" {
		if r.byModel[model] == nil {
			r.byModel[model] = &usageRollup{}
		}
		r.byModel[model].add(u)
	}

	if r.disabled || r.path == "" {
		return
	}

	sessionID := ""
	if sessionIDFn != nil {
		sessionID = sessionIDFn()
	}

	entry := UsageEntry{
		Timestamp:         time.Now().UTC().Format(time.RFC3339),
		SessionID:         sessionID,
		TraceID:           traceID,
		Source:            source,
		Provider:          provider,
		Model:             model,
		PromptTokens:      u.PromptTokens,
		CompletionTokens:  u.CompletionTokens,
		TotalTokens:       u.TotalTokens,
		CachedReadTokens:  u.CachedReadTokens,
		CacheCreateTokens: u.CacheCreateTokens,
		DurationMs:        dur.Milliseconds(),
	}
	data, err := json.Marshal(&entry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[usage] marshal error: %v\n", err)
		return
	}
	data = append(data, '\n')

	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[usage] open %s: %v\n", r.path, err)
		return
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		fmt.Fprintf(os.Stderr, "[usage] write %s: %v\n", r.path, err)
		return
	}
	// fsync rationale: usage rows are individually small and
	// infrequent (one per LLM call). Paying the syscall keeps billing
	// data on disk if the agent crashes mid-session, matching the
	// durability guarantees of hitl_audit.jsonl and MemoryStore writes.
	_ = f.Sync()
}

// Render returns a human-readable summary of usage rollups.
func (r *UsageRecorder) Render() string {
	if r == nil {
		return "(usage recorder not initialized)"
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.total.Calls == 0 {
		return "(no LLM calls yet)"
	}

	var b []byte
	add := func(s string) { b = append(b, s...) }
	addf := func(format string, args ...any) { b = append(b, fmt.Sprintf(format, args...)...) }

	add("--- LLM token usage (this process) ---\n")
	addf("Total: %d calls | prompt %d | completion %d | total %d",
		r.total.Calls, r.total.Prompt, r.total.Completion, r.total.Total)
	if r.total.CachedRead > 0 || r.total.CacheCreate > 0 {
		addf(" | cached_read %d | cache_create %d", r.total.CachedRead, r.total.CacheCreate)
	}
	add("\n")

	add("By source:\n")
	for _, k := range sortedRollupKeys(r.bySource) {
		v := r.bySource[k]
		addf("  %-14s %5d calls  prompt %7d  completion %7d  total %7d\n",
			k, v.Calls, v.Prompt, v.Completion, v.Total)
	}
	add("By model:\n")
	for _, k := range sortedRollupKeys(r.byModel) {
		v := r.byModel[k]
		addf("  %-24s %5d calls  prompt %7d  completion %7d  total %7d\n",
			k, v.Calls, v.Prompt, v.Completion, v.Total)
	}
	if r.path != "" {
		addf("Log: %s\n", r.path)
	}
	return string(b)
}

// sortedRollupKeys returns map keys sorted descending by total tokens
// so the most expensive bucket is at the top - that's almost always
// what the operator cares about first.
func sortedRollupKeys(m map[string]*usageRollup) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return m[keys[i]].Total > m[keys[j]].Total
	})
	return keys
}
