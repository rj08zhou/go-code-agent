package memory

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"go-code-agent/infra"
)

func TestTokenize(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "英文字符",
			input:    "hello world test",
			expected: []string{"hello", "world", "test"},
		},
		{
			name:     "数字",
			input:    "test123 abc456",
			expected: []string{"test123", "abc456"},
		},
		{
			name:     "中文字符",
			input:    "你好世界测试",
			expected: []string{"你好", "世界", "测试"},
		},
		{
			name:     "混合字符",
			input:    "hello 你好 world 世界",
			expected: []string{"hello", "你好", "world", "世界"},
		},
		{
			name:     "短词",
			input:    "a b c",
			expected: []string{"a", "b", "c"},
		},
		{
			name:     "单个字符",
			input:    "a",
			expected: []string{"a"},
		},
		{
			name:     "空字符串",
			input:    "",
			expected: []string{},
		},
		{
			name:     "无匹配字符",
			input:    "!@#$%^&*()",
			expected: []string{},
		},
		{
			name:     "包含标点",
			input:    "hello, world!",
			expected: []string{"hello", "world"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tokenize(tt.input)
			sort.Strings(result)
			sort.Strings(tt.expected)
			if !equalStringSlices(result, tt.expected) {
				t.Errorf("tokenize(%q) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}

func TestFNVHash(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected uint64
	}{
		{
			name:     "空字符串",
			input:    "",
			expected: 14695981039346656037,
		},
		{
			name:     "简单字符串",
			input:    "hello",
			expected: 11831194018420276491,
		},
		{
			name:     "重复字符串",
			input:    "hello",
			expected: 11831194018420276491,
		},
		{
			name:     "不同字符串",
			input:    "world",
			expected: 5717881983045765875,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := fnvHash(tt.input)
			if result != tt.expected {
				t.Errorf("fnvHash(%q) = %d, want %d", tt.input, result, tt.expected)
			}
		})
	}
}

func TestVectorCosine(t *testing.T) {
	tests := []struct {
		name     string
		a        []float64
		b        []float64
		expected float64
	}{
		{
			name:     "identical vectors",
			a:        []float64{1, 0, 0},
			b:        []float64{1, 0, 0},
			expected: 1.0,
		},
		{
			name:     "orthogonal vectors",
			a:        []float64{1, 0},
			b:        []float64{0, 1},
			expected: 0.0,
		},
		{
			name:     "opposite vectors",
			a:        []float64{1, 0},
			b:        []float64{-1, 0},
			expected: -1.0,
		},
		{
			name:     "partial match",
			a:        []float64{1, 2, 3},
			b:        []float64{2, 3, 4},
			expected: 0.992583,
		},
		{
			name:     "different lengths",
			a:        []float64{1, 2},
			b:        []float64{3, 4, 5},
			expected: 0.983870,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := vectorCosine(tt.a, tt.b)
			if math.Abs(result-tt.expected) > 0.0001 {
				t.Errorf("vectorCosine(%v, %v) = %f, want %f", tt.a, tt.b, result, tt.expected)
			}
		})
	}
}

func TestJaccardSimilarity(t *testing.T) {
	tests := []struct {
		name     string
		a        []string
		b        []string
		expected float64
	}{
		{
			name:     "identical sets",
			a:        []string{"a", "b", "c"},
			b:        []string{"a", "b", "c"},
			expected: 1.0,
		},
		{
			name:     "no overlap",
			a:        []string{"a", "b"},
			b:        []string{"c", "d"},
			expected: 0.0,
		},
		{
			name:     "partial overlap",
			a:        []string{"a", "b", "c"},
			b:        []string{"b", "c", "d"},
			expected: 0.5,
		},
		{
			name:     "empty first set",
			a:        []string{},
			b:        []string{"a", "b"},
			expected: 0.0,
		},
		{
			name:     "both empty",
			a:        []string{},
			b:        []string{},
			expected: 0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := jaccardSimilarity(tt.a, tt.b)
			if math.Abs(result-tt.expected) > 0.0001 {
				t.Errorf("jaccardSimilarity(%v, %v) = %f, want %f", tt.a, tt.b, result, tt.expected)
			}
		})
	}
}

func TestCategoryWeight(t *testing.T) {
	tests := []struct {
		name     string
		category string
		expected float64
	}{
		{"preference category", "preference", 1.5},
		{"lesson category", "lesson", 1.3},
		{"fact category", "fact", 1.0},
		{"context category", "context", 0.8},
		{"unknown category", "unknown", 0.7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			weight := categoryWeight(tt.category)
			if weight != tt.expected {
				t.Errorf("categoryWeight(%q) = %f, want %f", tt.category, weight, tt.expected)
			}
		})
	}
}

func TestParseEvergreenCategory(t *testing.T) {
	tests := []struct {
		name             string
		input            string
		expectedCategory string
		expectedText     string
	}{
		{
			name:             "preference category",
			input:            "[preference] user likes dark mode",
			expectedCategory: "preference",
			expectedText:     "user likes dark mode",
		},
		{
			name:             "lesson category",
			input:            "[lesson] always use context managers",
			expectedCategory: "lesson",
			expectedText:     "always use context managers",
		},
		{
			name:             "fact category",
			input:            "[fact] important information",
			expectedCategory: "fact",
			expectedText:     "important information",
		},
		{
			name:             "context category",
			input:            "[context] current task details",
			expectedCategory: "context",
			expectedText:     "current task details",
		},
		{
			name:             "no category",
			input:            "plain text without brackets",
			expectedCategory: "fact",
			expectedText:     "plain text without brackets",
		},
		{
			name:             "empty string",
			input:            "",
			expectedCategory: "fact",
			expectedText:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cat, text := parseEvergreenCategory(tt.input)
			if cat != tt.expectedCategory || text != tt.expectedText {
				t.Errorf("parseEvergreenCategory(%q) = (%q, %q), want (%q, %q)",
					tt.input, cat, text, tt.expectedCategory, tt.expectedText)
			}
		})
	}
}

func TestDecayRateForCategory(t *testing.T) {
	tests := []struct {
		name     string
		category string
		expected float64
	}{
		{"preference category", "preference", 0.01},
		{"lesson category", "lesson", 0.01},
		{"fact category", "fact", 0.01},
		{"context category", "context", 0.02},
		{"unknown category", "unknown", 0.01},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rate := decayRateForCategory(tt.category)
			if rate != tt.expected {
				t.Errorf("decayRateForCategory(%q) = %f, want %f", tt.category, rate, tt.expected)
			}
		})
	}
}

func TestTemporalDecayByCategory(t *testing.T) {
	// Use fixed reference time to make test deterministic
	// Format: 2026-06-05 00:00:00 UTC

	// Helper to calculate expected decayed score
	// decayRateForCategory returns: change_log=0.003, context=0.02, default=0.01
	calcDecay := func(score float64, cat string, daysAgo int) float64 {
		var rate float64
		switch cat {
		case "change_log":
			rate = 0.003
		case "context":
			rate = 0.02
		default:
			rate = 0.01
		}
		return score * math.Exp(-rate*float64(daysAgo))
	}

	tests := []struct {
		name     string
		results  []scoredChunk
		expected []float64
	}{
		{
			name: "all preference (no decay - MEMORY.md has no date)",
			results: []scoredChunk{
				{Chunk: memoryChunk{Path: "MEMORY.md [preference]", Text: "pref1", Category: "preference"}, Score: 0.9},
				{Chunk: memoryChunk{Path: "MEMORY.md [preference]", Text: "pref2", Category: "preference"}, Score: 0.8},
			},
			expected: []float64{0.9, 0.8}, // No decay for MEMORY.md
		},
		{
			name: "all fact (decay with fixed time)",
			results: []scoredChunk{
				{Chunk: memoryChunk{Path: "2026-05-28.jsonl [fact]", Text: "fact1", Category: "fact"}, Score: 0.9},
				{Chunk: memoryChunk{Path: "2026-05-27.jsonl [fact]", Text: "fact2", Category: "fact"}, Score: 0.8},
			},
			expected: []float64{
				calcDecay(0.9, "fact", 8), // 2026-05-28 to 2026-06-05 = 8 days
				calcDecay(0.8, "fact", 9), // 2026-05-27 to 2026-06-05 = 9 days
			},
		},
		{
			name: "mix of categories",
			results: []scoredChunk{
				{Chunk: memoryChunk{Path: "2026-05-28.jsonl [context]", Text: "ctx1", Category: "context"}, Score: 0.9},
				{Chunk: memoryChunk{Path: "2026-05-27.jsonl [fact]", Text: "fact1", Category: "fact"}, Score: 0.8},
				{Chunk: memoryChunk{Path: "MEMORY.md [preference]", Text: "pref1", Category: "preference"}, Score: 0.7},
			},
			expected: []float64{
				calcDecay(0.9, "context", 8), // context decays fastest (rate=0.02)
				calcDecay(0.8, "fact", 9),    // fact decays (rate=0.01)
				0.7,                          // MEMORY.md - no decay
			},
		},
		{
			name:     "empty results",
			results:  []scoredChunk{},
			expected: []float64{},
		},
	}

	// Note: temporalDecayByCategory uses time.Now().UTC() internally,
	// so these tests will fail if run at a different time.
	// TODO: Refactor temporalDecayByCategory to accept a 'now' parameter for testability.
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use fixed reference time for deterministic results
			// Reference: 2026-06-05 00:00:00 UTC
			decayed := temporalDecayByCategoryWithNow(tt.results, time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC))
			if len(decayed) != len(tt.expected) {
				t.Fatalf("Expected %d results, got %d", len(tt.expected), len(decayed))
			}
			for i := range decayed {
				if math.Abs(decayed[i].Score-tt.expected[i]) > 0.0001 {
					t.Errorf("Index %d: expected score %f, got %f", i, tt.expected[i], decayed[i].Score)
				}
			}
		})
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestConcurrentAccess(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	// Concurrent writes
	writesDone := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func(idx int) {
			store.WriteMemory(string(rune('a'+idx)), "fact")
			writesDone <- struct{}{}
		}(i)
	}
	for i := 0; i < 10; i++ {
		<-writesDone
	}

	// Concurrent reads
	readsDone := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func() {
			store.HybridSearch("test", 10)
			readsDone <- struct{}{}
		}()
	}
	for i := 0; i < 10; i++ {
		<-readsDone
	}
}

// ============================================================================
// MemoryStore Core Tests
// ============================================================================

func TestNewMemoryStore(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	if store == nil {
		t.Fatal("NewMemoryStore returned nil")
	}
	if store.memoryDir == "" {
		t.Error("memoryDir is empty")
	}
	if store.workspaceDir == "" {
		t.Error("workspaceDir is empty")
	}

	// Verify directory was created
	if _, err := os.Stat(filepath.Join(store.workspaceDir, "memory", "daily")); os.IsNotExist(err) {
		t.Error("memory directory was not created")
	}
}

func TestLoadEvergreen(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	// Create a MEMORY.md file
	evergreenPath := filepath.Join(store.workspaceDir, "MEMORY.md")
	err := os.WriteFile(evergreenPath, []byte("# Test Memory\n\n[preference] user likes dark mode\n\n[lesson] always use context managers"), 0o644)
	if err != nil {
		t.Fatalf("Failed to write MEMORY.md: %v", err)
	}

	content := store.LoadEvergreen()
	if content == "" {
		t.Error("LoadEvergreen returned empty string")
	}

	// Verify content is trimmed
	expected := "# Test Memory\n\n[preference] user likes dark mode\n\n[lesson] always use context managers"
	if content != expected {
		t.Errorf("LoadEvergreen returned %q, want %q", content, expected)
	}
}

func TestLoadEvergreen_NonExistent(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	content := store.LoadEvergreen()
	if content != "" {
		t.Errorf("LoadEvergreen returned %q, want empty string", content)
	}
}

func TestWriteMemory(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	// Test basic write
	result := store.WriteMemory("test memory content", "fact")
	if result == "" {
		t.Error("WriteMemory returned empty string")
	}
	if !strings.Contains(result, ".jsonl") {
		t.Error("WriteMemory result doesn't contain .jsonl")
	}

	// Test with category
	result = store.WriteMemory("another memory", "lesson")
	if result == "" {
		t.Error("WriteMemory returned empty string")
	}

	// Verify it was saved to disk
	files, _ := filepath.Glob(filepath.Join(store.memoryDir, "*.jsonl"))
	if len(files) == 0 {
		t.Error("No daily files were created")
	}
}

func TestWriteMemory_MaxLength(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	// Create a content that exceeds max length
	longContent := strings.Repeat("a", infra.MaxMemoryContentLen+100)
	result := store.WriteMemory(longContent, "fact")

	if result == "" {
		t.Error("WriteMemory should not return empty string for valid input")
	}
	if !strings.Contains(result, "too long") {
		t.Errorf("WriteMemory should return error for too long content, got: %s", result)
	}
}

func TestWriteMemory_EmptyCategory(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	result := store.WriteMemory("test content", "")
	if result == "" {
		t.Error("WriteMemory returned empty string")
	}
}

func TestWriteMemory_Duplicate(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	// Write the same content twice
	content := "duplicate test content"
	result1 := store.WriteMemory(content, "fact")
	result2 := store.WriteMemory(content, "fact")

	// Second write should replace the first
	if result2 == result1 {
		t.Error("Duplicate write should return different result")
	}
}

func TestGetStats(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	evergreenPath := filepath.Join(store.workspaceDir, "MEMORY.md")
	err := os.WriteFile(evergreenPath, []byte("[fact] initial fact"), 0o644)
	if err != nil {
		t.Fatalf("Failed to write MEMORY.md: %v", err)
	}

	// Write some daily memories
	store.WriteMemory("daily fact 1", "fact")
	store.WriteMemory("daily fact 2", "lesson")

	evergreenChars, dailyFiles, dailyEntries := store.GetStats()

	if evergreenChars <= 0 {
		t.Errorf("Expected non-zero evergreenChars, got %d", evergreenChars)
	}
	if dailyFiles < 1 {
		t.Errorf("Expected at least 1 daily file, got %d", dailyFiles)
	}
	if dailyEntries < 2 {
		t.Errorf("Expected at least 2 daily entries, got %d", dailyEntries)
	}
}

func TestDeleteMemory(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	// Write a memory to delete
	content := "memory to delete"
	store.WriteMemory(content, "fact")

	// Delete it
	result := store.DeleteMemory("memory to delete", "fact")
	if result == "" {
		t.Error("DeleteMemory returned empty string")
	}
	if !strings.Contains(result, ".jsonl") {
		t.Error("DeleteMemory result doesn't contain .jsonl")
	}

	// Verify it was actually deleted
	files, _ := filepath.Glob(filepath.Join(store.memoryDir, "*.jsonl"))
	if len(files) > 0 {
		t.Error("Daily files should be empty after deleting all entries")
	}
}

func TestDeleteMemory_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	result := store.DeleteMemory("nonexistent memory", "fact")
	if result == "" {
		t.Error("DeleteMemory should return error message for not found")
	}
	if !strings.Contains(result, "No matching memory found") {
		t.Errorf("DeleteMemory should indicate not found, got: %s", result)
	}
}

func TestDeleteMemory_EmptyQuery(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	result := store.DeleteMemory("", "fact")
	if result == "" {
		t.Error("DeleteMemory should return error for empty query")
	}
	if !strings.Contains(result, "empty query") {
		t.Errorf("DeleteMemory should indicate empty query, got: %s", result)
	}
}

// ============================================================================
// BM25 and Search Algorithm Tests
// ============================================================================

func TestBM25Score(t *testing.T) {
	tests := []struct {
		name     string
		query    []string
		doc      []string
		df       map[string]int
		n        int
		avgdl    float64
		expected float64
	}{
		{
			name:     "simple match",
			query:    []string{"hello", "world"},
			doc:      []string{"hello", "world", "test"},
			df:       map[string]int{"hello": 1, "world": 1, "test": 1},
			n:        3,
			avgdl:    2.0,
			expected: 0.5,
		},
		{
			name:     "no match",
			query:    []string{"hello"},
			doc:      []string{"world", "test"},
			df:       map[string]int{"hello": 0, "world": 1, "test": 1},
			n:        2,
			avgdl:    1.5,
			expected: 0.0,
		},
		{
			name:     "multiple matches",
			query:    []string{"a", "b", "c"},
			doc:      []string{"a", "b", "c", "d"},
			df:       map[string]int{"a": 1, "b": 1, "c": 1, "d": 1},
			n:        4,
			avgdl:    3.0,
			expected: 0.5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score := bm25Score(tt.query, tt.doc, tt.df, tt.n, tt.avgdl)
			if score < 0 {
				t.Errorf("BM25 score should be non-negative, got %f", score)
			}
		})
	}
}

func TestHashVector(t *testing.T) {
	tests := []struct {
		name        string
		text        string
		dim         int
		expectedLen int
	}{
		{
			name:        "simple text",
			text:        "hello",
			dim:         64,
			expectedLen: 64,
		},
		{
			name:        "empty text",
			text:        "",
			dim:         64,
			expectedLen: 64,
		},
		{
			name:        "CJK text",
			text:        "你好世界",
			dim:         64,
			expectedLen: 64,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vec := hashVector(tt.text, tt.dim)
			if len(vec) != tt.expectedLen {
				t.Errorf("hashVector returned length %d, want %d", len(vec), tt.expectedLen)
			}
			// Check vector properties
			sum := 0.0
			for _, v := range vec {
				sum += v * v
			}
			normalized := math.Sqrt(sum)

			// Empty text should produce zero vector
			if tt.text == "" {
				if normalized != 0 {
					t.Errorf("Empty text should produce zero vector, got norm %f", normalized)
				}
			} else {
				// Non-empty text should produce normalized vector
				if normalized == 0 {
					t.Error("Vector should not be all zeros for non-empty text")
				}
				if math.Abs(normalized-1.0) > 0.01 {
					t.Errorf("Vector should be normalized to length 1, got %f", normalized)
				}
			}
		})
	}
}

func TestKeywordSearch(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	memories := []memoryChunk{
		{Path: "2026-06-05.jsonl", Text: "golang is great for concurrency", Category: "fact"},
		{Path: "2026-06-05.jsonl", Text: "rust is also good for performance", Category: "lesson"},
		{Path: "2026-06-05.jsonl", Text: "python is easy to learn", Category: "fact"},
	}

	// Test search with no results
	results := store.keywordSearch("nonexistent", memories, 10)
	if results != nil {
		t.Error("keywordSearch should return nil for no matches")
	}

	// Test search with results
	results = store.keywordSearch("golang", memories, 10)
	if results == nil {
		t.Fatal("keywordSearch returned nil")
	}
	if len(results) == 0 {
		t.Error("keywordSearch should return results")
	}

	// Test with topK limit
	results = store.keywordSearch("golang", memories, 1)
	if results == nil {
		t.Fatal("keywordSearch returned nil")
	}
	if len(results) > 1 {
		t.Errorf("keywordSearch should limit to topK, got %d results", len(results))
	}
}

func TestVectorSearch(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	memories := []memoryChunk{
		{Path: "2026-06-05.jsonl", Text: "hello world", Category: "fact"},
		{Path: "2026-06-05.jsonl", Text: "test content", Category: "fact"},
	}

	// Test search with no results
	results := store.vectorSearch("nonexistent", memories, 10)
	if results != nil {
		t.Error("vectorSearch should return nil for no matches")
	}

	// Test search with results
	results = store.vectorSearch("hello", memories, 10)
	if results == nil {
		t.Fatal("vectorSearch returned nil")
	}
	if len(results) == 0 {
		t.Error("vectorSearch should return results")
	}
}

func TestMergeHybridResults(t *testing.T) {
	vecResults := []scoredChunk{
		{Chunk: memoryChunk{Text: "vec result 1"}, Score: 0.8},
		{Chunk: memoryChunk{Text: "vec result 2"}, Score: 0.6},
	}

	kwResults := []scoredChunk{
		{Chunk: memoryChunk{Text: "vec result 1"}, Score: 0.7},
		{Chunk: memoryChunk{Text: "kw result 1"}, Score: 0.9},
	}

	merged := mergeHybridResults(vecResults, kwResults, 0.5, 0.5)
	if merged == nil {
		t.Fatal("mergeHybridResults returned nil")
	}

	// Should have 3 results: "vec result 1" (deduplicated), "vec result 2", "kw result 1"
	if len(merged) != 3 {
		t.Errorf("mergeHybridResults should have 3 results, got %d", len(merged))
	}

	for _, r := range merged {
		if r.Chunk.Text == "vec result 1" {
			if r.Score < 0.7 || r.Score > 0.8 {
				t.Errorf("Combined score for vec result 1 should be ~0.75, got %f", r.Score)
			}
		}
	}
}

func TestMMRRerank(t *testing.T) {
	results := []scoredChunk{
		{Chunk: memoryChunk{Text: "result 1"}, Score: 0.9},
		{Chunk: memoryChunk{Text: "result 2"}, Score: 0.8},
		{Chunk: memoryChunk{Text: "result 1"}, Score: 0.7},
	}

	reranked := mmrRerank(results, 0.7)
	if reranked == nil {
		t.Fatal("mmrRerank returned nil")
	}

	// mmrRerank only reranks, does not deduplicate (that's deduplicateResults's job)
	if len(reranked) != 3 {
		t.Errorf("mmrRerank should return all 3 results, got %d", len(reranked))
	}
}

func TestDeduplicateResults(t *testing.T) {
	results := []scoredChunk{
		{Chunk: memoryChunk{Text: "same text 1"}, Score: 0.9},
		{Chunk: memoryChunk{Text: "same text 1"}, Score: 0.8},
		{Chunk: memoryChunk{Text: "different text"}, Score: 0.7},
	}

	deduped := deduplicateResults(results, 0.9)
	if deduped == nil {
		t.Fatal("deduplicateResults returned nil")
	}

	if len(deduped) != 2 {
		t.Errorf("deduplicateResults should deduplicate, got %d results", len(deduped))
	}
}

func TestFilterChunks(t *testing.T) {
	chunks := []memoryChunk{
		{Path: "2026-06-05.jsonl", Text: "today's memory", Category: "fact"},
		{Path: "2026-06-04.jsonl", Text: "yesterday's memory", Category: "fact"},
		{Path: "2026-06-03.jsonl", Text: "old memory", Category: "fact"},
		{Path: "MEMORY.md", Text: "evergreen memory", Category: "fact"},
	}

	// Test withinDays filter
	// Should return: today (06-05), yesterday (06-04), and MEMORY.md (evergreen, always included)
	filtered := filterChunks(chunks, 2, "")
	if filtered == nil {
		t.Fatal("filterChunks returned nil")
	}

	if len(filtered) != 3 {
		t.Errorf("filterChunks with withinDays=2 should return 3 results, got %d", len(filtered))
	}

	// Test category filter
	filtered = filterChunks(chunks, 0, "lesson")
	if filtered == nil {
		t.Fatal("filterChunks returned nil")
	}

	if len(filtered) != 0 {
		t.Errorf("filterChunks with category=lesson should return 0 results, got %d", len(filtered))
	}

	// Test no filter
	filtered = filterChunks(chunks, 0, "")
	if filtered == nil {
		t.Fatal("filterChunks returned nil")
	}

	if len(filtered) != 4 {
		t.Errorf("filterChunks with no filters should return all 4 results, got %d", len(filtered))
	}
}

func TestHybridSearch(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	evergreenPath := filepath.Join(store.workspaceDir, "MEMORY.md")
	err := os.WriteFile(evergreenPath, []byte("[fact] evergreen memory"), 0o644)
	if err != nil {
		t.Fatalf("Failed to write MEMORY.md: %v", err)
	}

	// Add daily memories
	store.WriteMemory("golang is great", "fact")
	store.WriteMemory("rust is fast", "lesson")

	// Test search with results
	results := store.HybridSearch("golang", 10)
	if results == nil {
		t.Fatal("HybridSearch returned nil")
	}
	if len(results) == 0 {
		t.Error("HybridSearch should return results")
	}

	// Test with empty query
	results = store.HybridSearch("", 10)
	if results == nil {
		t.Fatal("HybridSearch returned nil")
	}
}

func TestHybridSearchFiltered(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	// Add memories
	store.WriteMemory("today's memory", "fact")
	store.WriteMemory("yesterday's memory", "fact")

	// Test withinDays filter
	results := store.HybridSearchFiltered("memory", 10, 1, "")
	if results == nil {
		t.Fatal("HybridSearchFiltered returned nil")
	}
	if len(results) == 0 {
		t.Error("HybridSearchFiltered should return results")
	}

	// Test category filter
	results = store.HybridSearchFiltered("memory", 10, 0, "lesson")
	// Should return empty slice (not nil) when no matches
	if len(results) != 0 {
		t.Errorf("HybridSearchFiltered with non-matching category should return empty results, got %d", len(results))
	}
}

// ============================================================================
// Additional Tests for Missing Coverage
// ============================================================================

// TestCleanExpired - test cleanup of expired daily files
func TestCleanExpired(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	// Create daily files from different dates
	now := time.Now().UTC()
	oldDate := now.AddDate(0, 0, -90).Format("2006-01-02") // Should be cleaned
	weekAgo := now.AddDate(0, 0, -7).Format("2006-01-02")  // Should be kept
	today := now.Format("2006-01-02")                      // Should be kept

	for _, date := range []string{oldDate, weekAgo, today} {
		path := filepath.Join(store.memoryDir, date+".jsonl")
		os.WriteFile(path, []byte("test content"), 0o644)
	}

	// Verify all files exist
	files, _ := filepath.Glob(filepath.Join(store.memoryDir, "*.jsonl"))
	if len(files) != 3 {
		t.Fatalf("Expected 3 files, got %d", len(files))
	}

	// Clean expired files
	store.cleanExpired()

	// Check that old file was removed
	files, _ = filepath.Glob(filepath.Join(store.memoryDir, "*.jsonl"))
	if len(files) != 2 {
		t.Errorf("Expected 2 files after cleanup, got %d", len(files))
	}

	// Verify remaining files are not the old one
	for _, f := range files {
		base := strings.TrimSuffix(filepath.Base(f), ".jsonl")
		if base == oldDate {
			t.Error("Old file should have been removed")
		}
	}
}

// TestLoadAllChunksFromDisk - test loading from MEMORY.md and daily files
func TestLoadAllChunksFromDisk(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	// Create MEMORY.md with multiple categories (MEMORY format)
	memoryContent := `[preference] user likes dark mode

[lesson] always use context managers

plain fact about the project`
	memPath := filepath.Join(store.workspaceDir, "MEMORY.md")
	if err := os.WriteFile(memPath, []byte(memoryContent), 0o644); err != nil {
		t.Fatalf("Failed to write MEMORY.md: %v", err)
	}

	// Create daily files with entries
	now := time.Now().UTC()
	todayStr := now.Format("2006-01-02")
	yesterdayStr := now.AddDate(0, 0, -1).Format("2006-01-02")

	// Today's file
	todayPath := filepath.Join(store.memoryDir, todayStr+".jsonl")
	todayContent := `{"ts":"2026-06-05T10:00:00Z","category":"fact","content":"today's memory"}
{"ts":"2026-06-05T11:00:00Z","category":"lesson","content":"another lesson"}`

	// Yesterday's file
	yesterdayPath := filepath.Join(store.memoryDir, yesterdayStr+".jsonl")
	yesterdayContent := `{"ts":"2026-06-04T10:00:00Z","category":"context","content":"yesterday context"}`

	os.WriteFile(todayPath, []byte(todayContent), 0o644)
	os.WriteFile(yesterdayPath, []byte(yesterdayContent), 0o644)

	// Load chunks from disk
	chunks := store.loadAllChunksFromDisk()

	// Should have: evergreen chunks + daily chunks
	// Evergreen: 3 paragraphs (preference, lesson, fact)
	// Daily: 3 entries
	expectedTotal := 3 + 3
	if len(chunks) != expectedTotal {
		t.Errorf("Expected %d chunks, got %d", expectedTotal, len(chunks))
	}

	// Verify evergreen chunks
	hasPreference := false
	hasLesson := false
	hasFact := false
	for _, c := range chunks {
		if strings.Contains(c.Path, "MEMORY.md") {
			if c.Category == "preference" {
				hasPreference = true
			} else if c.Category == "lesson" {
				hasLesson = true
			} else if c.Category == "fact" {
				hasFact = true
			}
		}
	}

	if !hasPreference || !hasLesson || !hasFact {
		t.Error("Expected all three evergreen categories to be loaded")
	}

	// Verify daily chunks
	hasTodayFact := false
	hasTodayLesson := false
	hasYesterdayContext := false
	for _, c := range chunks {
		if strings.Contains(c.Path, todayStr+".jsonl") {
			if c.Category == "fact" {
				hasTodayFact = true
			} else if c.Category == "lesson" {
				hasTodayLesson = true
			}
		}
		if strings.Contains(c.Path, yesterdayStr+".jsonl") && c.Category == "context" {
			hasYesterdayContext = true
		}
	}

	if !hasTodayFact || !hasTodayLesson || !hasYesterdayContext {
		t.Error("Expected all daily chunks to be loaded")
	}
}

// TestTryReplaceDuplicate - test duplicate detection and replacement
func TestTryReplaceDuplicate(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	// Write an initial memory with high similarity (above threshold)
	// Original: "the quick brown fox jumps over the lazy dog"
	// Duplicate: "the quick brown fox jumps over the lazy dog too"
	// These share 9 of 10 tokens (jaccard = 0.9) -> should be replaced
	store.WriteMemory("the quick brown fox jumps over the lazy dog", "fact")

	// Write similar content - should trigger replacement
	result := store.WriteMemory("the quick brown fox jumps over the lazy dog too", "fact")
	if !strings.Contains(result, "updated") {
		t.Errorf("WriteMemory should detect and replace duplicate, got: %s", result)
	}

	// Verify only one entry exists
	chunks := store.loadAllChunks()
	factChunks := 0
	for _, c := range chunks {
		if c.Category == "fact" {
			factChunks++
		}
	}
	if factChunks != 1 {
		t.Errorf("Expected 1 fact chunk, got %d", factChunks)
	}
}

// TestTryReplaceDuplicate_DifferentCategory - test different category doesn't replace
func TestTryReplaceDuplicate_DifferentCategory(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	// Write to fact category
	store.WriteMemory("duplicate content", "fact")

	// Write same content to lesson category - should NOT replace (different category)
	result := store.WriteMemory("duplicate content", "lesson")
	if !strings.Contains(result, ".jsonl") && !strings.Contains(result, "saved") {
		t.Errorf("Should create new entry in different category, got: %s", result)
	}

	// Verify both entries exist
	chunks := store.loadAllChunks()
	factCount := 0
	lessonCount := 0
	for _, c := range chunks {
		if c.Category == "fact" {
			factCount++
		} else if c.Category == "lesson" {
			lessonCount++
		}
	}
	if factCount != 1 || lessonCount != 1 {
		t.Errorf("Expected 1 fact and 1 lesson chunk, got fact=%d, lesson=%d", factCount, lessonCount)
	}
}

// TestTryReplaceDuplicate_BelowThreshold - test low similarity doesn't replace
func TestTryReplaceDuplicate_BelowThreshold(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	// Write content
	store.WriteMemory("test content", "fact")

	// Write very different content - should NOT replace
	result := store.WriteMemory("completely different content", "fact")
	if !strings.Contains(result, ".jsonl") && !strings.Contains(result, "saved") {
		t.Errorf("Should create new entry for low similarity, got: %s", result)
	}

	// Verify both entries exist
	chunks := store.loadAllChunks()
	if len(chunks) < 2 {
		t.Errorf("Expected at least 2 chunks, got %d", len(chunks))
	}
}

// TestRebuildCache - test cache rebuild
func TestRebuildCache(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	// Write some initial data
	store.WriteMemory("initial memory", "fact")

	// Get initial cache
	cache1 := store.loadAllChunks()
	if len(cache1) == 0 {
		t.Error("Expected initial cache to have entries")
	}

	// Write more data
	store.WriteMemory("second memory", "lesson")

	// Rebuild cache
	store.rebuildCache()
	cache2 := store.loadAllChunks()

	// Should have both entries
	if len(cache2) < len(cache1)+1 {
		t.Errorf("Expected cache to have more entries after rebuild, got %d", len(cache2))
	}
}

// TestRebuildCache_WithEvergreen - test rebuild with MEMORY.md
func TestRebuildCache_WithEvergreen(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	// Write MEMORY.md
	memoryContent := `[preference] user likes dark mode`
	memPath := filepath.Join(store.workspaceDir, "MEMORY.md")
	if err := os.WriteFile(memPath, []byte(memoryContent), 0o644); err != nil {
		t.Fatalf("Failed to write MEMORY.md: %v", err)
	}

	// Rebuild cache
	store.rebuildCache()
	chunks := store.loadAllChunks()

	// Should have evergreen chunk
	foundEvergreen := false
	for _, c := range chunks {
		if strings.Contains(c.Path, "MEMORY.md") {
			foundEvergreen = true
			if c.Category != "preference" {
				t.Errorf("Expected category preference, got %s", c.Category)
			}
		}
	}
	if !foundEvergreen {
		t.Error("Expected evergreen chunk in cache")
	}
}

// TestGetStats_MultipleDays - test stats with multiple days
func TestGetStats_MultipleDays(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	// Write MEMORY.md
	memoryContent := `[preference] user likes dark mode`
	memPath := filepath.Join(store.workspaceDir, "MEMORY.md")
	if err := os.WriteFile(memPath, []byte(memoryContent), 0o644); err != nil {
		t.Fatalf("Failed to write MEMORY.md: %v", err)
	}

	// Write to multiple days - ensure different dates
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		date := now.AddDate(0, 0, i).Format("2006-01-02")
		path := filepath.Join(store.memoryDir, date+".jsonl")
		content := fmt.Sprintf(`{"ts":"2026-06-05T10:00:00Z","category":"fact","content":"daily entry %d"}`, i)
		os.WriteFile(path, []byte(content), 0o644)
	}

	evergreenChars, dailyFiles, dailyEntries := store.GetStats()

	if evergreenChars == 0 {
		t.Error("Expected non-zero evergreen character count")
	}
	if dailyFiles < 1 {
		t.Errorf("Expected at least 1 daily file, got %d", dailyFiles)
	}
	if dailyEntries != 5 {
		t.Errorf("Expected 5 daily entries, got %d", dailyEntries)
	}
}

// TestHybridSearchFiltered_NoCategory - test with category=""
func TestHybridSearchFiltered_NoCategory(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	// Write memories with different categories
	store.WriteMemory("fact content", "fact")
	store.WriteMemory("lesson content", "lesson")

	// category="" should include all categories
	results := store.HybridSearchFiltered("content", 10, 0, "")
	if results == nil {
		t.Fatal("HybridSearchFiltered should return results")
	}
	if len(results) == 0 {
		t.Error("Expected results with no category filter")
	}
}

// TestWriteMemory_SpecialCharacters - test with special characters
func TestWriteMemory_SpecialCharacters(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	// Test with special characters
	testCases := []struct {
		name  string
		input string
	}{
		{"with emojis", "Hello 👋 World 🌍"},
		{"with unicode", "中文测试 你好世界"},
		{"with punctuation", "Hello, world! How are you?"},
		{"with mixed", "Hello 世界! 👋"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := store.WriteMemory(tc.input, "fact")
			if strings.Contains(result, "Error") {
				t.Errorf("WriteMemory failed: %s", result)
			}
		})
	}
}

// TestDeleteMemory_WithCategory - test delete with category filter
func TestDeleteMemory_WithCategory(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	// Write memories with same text but different categories
	store.WriteMemory("shared text", "fact")
	store.WriteMemory("shared text", "lesson")
	store.WriteMemory("shared text", "preference")

	// Delete with category filter - should only delete fact
	result := store.DeleteMemory("shared text", "fact")
	if !strings.Contains(result, "Deleted") {
		t.Errorf("DeleteMemory failed: %s", result)
	}

	// Verify only fact is deleted (should be 2 chunks left: lesson and preference)
	chunks := store.loadAllChunks()
	if len(chunks) != 2 {
		t.Errorf("Expected 2 chunks after deletion (lesson and preference), got %d", len(chunks))
	}

	// Verify remaining chunks have different categories
	categories := make(map[string]bool)
	for _, c := range chunks {
		categories[c.Category] = true
	}
	if !categories["lesson"] || !categories["preference"] {
		t.Error("Expected lesson and preference categories to remain")
	}
}

// TestDeleteMemory_NonMatchingCategory - test delete with non-matching category
func TestDeleteMemory_NonMatchingCategory(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	// Write memory
	store.WriteMemory("test memory", "fact")

	// Try to delete with wrong category
	result := store.DeleteMemory("test memory", "lesson")
	if !strings.Contains(result, "No matching memory found") {
		t.Errorf("Expected not found error, got: %s", result)
	}
}

// TestLoadAllChunks - test cache access
func TestLoadAllChunks(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewMemoryStore(tmpDir)

	// Write some data
	store.WriteMemory("test memory", "fact")

	// Load chunks from cache
	chunks := store.loadAllChunks()
	if len(chunks) == 0 {
		t.Error("Expected chunks from cache")
	}

	// Modify cache should not affect disk
	store.WriteMemory("second memory", "lesson")
	chunks2 := store.loadAllChunks()
	if len(chunks2) != len(chunks)+1 {
		t.Errorf("Expected cache to reflect writes, got %d vs %d", len(chunks2), len(chunks))
	}
}
