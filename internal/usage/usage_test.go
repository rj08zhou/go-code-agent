package usage

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go-code-agent/internal/llm"
)

// newTestRecorder returns a fresh, file-disabled UsageRecorder so tests
// exercise pure in-memory rollup logic without touching disk.
func newTestRecorder() *UsageRecorder {
	return &UsageRecorder{
		bySource: map[string]*usageRollup{},
		byModel:  map[string]*usageRollup{},
		disabled: true, // path == "" → no file IO, rollups still update
	}
}

func u(prompt, completion, total int64) llm.Usage {
	return llm.Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
	}
}

// --- usageRollup.add -------------------------------------------------------

func TestRollupAddAccumulates(t *testing.T) {
	r := &usageRollup{}
	r.add(u(10, 20, 30))
	r.add(u(5, 5, 10))
	if r.Calls != 2 {
		t.Errorf("Calls = %d, want 2", r.Calls)
	}
	if r.Prompt != 15 {
		t.Errorf("Prompt = %d, want 15", r.Prompt)
	}
	if r.Completion != 25 {
		t.Errorf("Completion = %d, want 25", r.Completion)
	}
	if r.Total != 40 {
		t.Errorf("Total = %d, want 40", r.Total)
	}
}

func TestRollupAddTracksCacheTokens(t *testing.T) {
	r := &usageRollup{}
	r.add(llm.Usage{
		PromptTokens:      100,
		CompletionTokens:  50,
		TotalTokens:       150,
		CachedReadTokens:  80,
		CacheCreateTokens: 30,
	})
	if r.CachedRead != 80 || r.CacheCreate != 30 {
		t.Errorf("cache rollup = read %d create %d, want 80/30", r.CachedRead, r.CacheCreate)
	}
}

// --- Record: nil / zero guards ---------------------------------------------

func TestRecordNilReceiverNoop(t *testing.T) {
	var r *UsageRecorder
	r.Record("agent", "openai", "gpt-4o", "", llm.Usage{}, 0) // must not panic
}

func TestRecordZeroUsageNoop(t *testing.T) {
	r := newTestRecorder()
	r.Record("agent", "openai", "gpt-4o", "", llm.Usage{}, 0)
	if r.total.Calls != 0 {
		t.Errorf("zero-usage Record updated rollup: Calls = %d, want 0", r.total.Calls)
	}
}

// --- Record: TotalTokens derivation ----------------------------------------

func TestRecordDerivesTotalWhenZero(t *testing.T) {
	r := newTestRecorder()
	r.Record("agent", "anthropic", "claude", "", u(100, 40, 0), 0)
	if r.total.Total != 140 {
		t.Errorf("derived Total = %d, want 140 (prompt+completion)", r.total.Total)
	}
}

// --- Record: source normalization ------------------------------------------

func TestRecordEmptySourceBecomesUnknown(t *testing.T) {
	r := newTestRecorder()
	r.Record("", "openai", "gpt-4o", "", u(1, 1, 2), 0)
	if _, ok := r.bySource["unknown"]; !ok {
		t.Errorf("empty source should roll up into \"unknown\" bucket, got keys %v", keysOf(r.bySource))
	}
}

func TestRecordInvalidSourceQuarantined(t *testing.T) {
	r := newTestRecorder()
	r.Record("agnet", "openai", "gpt-4o", "", u(1, 1, 2), 0)
	if _, ok := r.bySource["(unknown:agnet)"]; !ok {
		t.Errorf("typo source should be quarantined into \"(unknown:agnet)\", got keys %v", keysOf(r.bySource))
	}
}

func TestRecordValidSourceRollsUp(t *testing.T) {
	r := newTestRecorder()
	r.Record("judge", "openai", "gpt-4o", "", u(2, 3, 5), 0)
	if r.bySource["judge"] == nil || r.bySource["judge"].Total != 5 {
		t.Errorf("valid source \"judge\" not rolled up correctly: %+v", r.bySource["judge"])
	}
}

func TestRecordEmptyModelSkipsByModel(t *testing.T) {
	r := newTestRecorder()
	r.Record("agent", "openai", "", "", u(1, 1, 2), 0)
	if len(r.byModel) != 0 {
		t.Errorf("empty model should not create byModel bucket, got %v", keysOf(r.byModel))
	}
}

// --- Render ----------------------------------------------------------------

func TestRenderNilRecorder(t *testing.T) {
	var r *UsageRecorder
	if got := r.Render(); got != "(usage recorder not initialized)" {
		t.Errorf("Render() = %q, want not-initialized message", got)
	}
}

func TestRenderNoCalls(t *testing.T) {
	r := newTestRecorder()
	if got := r.Render(); got != "(no LLM calls yet)" {
		t.Errorf("Render() = %q, want no-calls message", got)
	}
}

func TestRenderShowsCachedWhenNonZero(t *testing.T) {
	r := newTestRecorder()
	r.Record("agent", "anthropic", "claude", "", llm.Usage{
		PromptTokens:     100,
		CompletionTokens: 10,
		TotalTokens:      110,
		CachedReadTokens: 60,
	}, 0)
	out := r.Render()
	if !strings.Contains(out, "cached_read") {
		t.Errorf("Render should include cached_read when non-zero, got:\n%s", out)
	}
}

// sortedRollupKeys returns map keys sorted by total tokens desc.
func TestSortedRollupKeysOrdersByTotalDesc(t *testing.T) {
	m := map[string]*usageRollup{
		"small": {Total: 10},
		"big":   {Total: 100},
		"mid":   {Total: 50},
	}
	got := sortedRollupKeys(m)
	want := []string{"big", "mid", "small"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sortedRollupKeys = %v, want %v", got, want)
			break
		}
	}
}

func TestSortedRollupKeysEmpty(t *testing.T) {
	if got := sortedRollupKeys(map[string]*usageRollup{}); len(got) != 0 {
		t.Errorf("empty map should yield no keys, got %v", got)
	}
}

// --- InitUsageRecorder -----------------------------------------------------

func TestInitEmptyWorkdirDisables(t *testing.T) {
	InitUsageRecorder("")
	if !usageRecorder.disabled {
		t.Errorf("InitUsageRecorder(\"\") should disable the recorder")
	}
}

// --- File-backed recording -------------------------------------------------

func TestRecordWritesJSONLRow(t *testing.T) {
	dir := t.TempDir()
	r := &UsageRecorder{
		bySource: map[string]*usageRollup{},
		byModel:  map[string]*usageRollup{},
	}
	memDir := filepath.Join(dir, ".go-code-agent", "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	r.path = filepath.Join(memDir, "usage.jsonl")

	SetSessionIDFunc(func() string { return "sess-1" })
	r.Record("agent", "openai", "gpt-4o", "trace-9", u(12, 34, 46), 7*time.Millisecond)

	data, err := os.ReadFile(r.path)
	if err != nil {
		t.Fatalf("read usage.jsonl: %v", err)
	}
	lines := scanLines(data)
	if len(lines) != 1 {
		t.Fatalf("expected 1 JSONL row, got %d", len(lines))
	}
	var e UsageEntry
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatalf("unmarshal row: %v", err)
	}
	if e.SessionID != "sess-1" || e.TraceID != "trace-9" || e.Source != "agent" {
		t.Errorf("entry metadata wrong: %+v", e)
	}
	if e.PromptTokens != 12 || e.CompletionTokens != 34 || e.TotalTokens != 46 {
		t.Errorf("entry tokens wrong: %+v", e)
	}
	if e.DurationMs != 7 {
		t.Errorf("DurationMs = %d, want 7", e.DurationMs)
	}
}

// --- helpers ---------------------------------------------------------------

func keysOf(m map[string]*usageRollup) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func scanLines(b []byte) []string {
	var out []string
	s := bufio.NewScanner(strings.NewReader(string(b)))
	for s.Scan() {
		if line := strings.TrimSpace(s.Text()); line != "" {
			out = append(out, line)
		}
	}
	return out
}
