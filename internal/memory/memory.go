package memory

import (
	"encoding/json"
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/logging"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemoryStore - two-layer storage with deduplication
//
// Layer 1: MEMORY.md       = long-term facts (manually maintained)
// Layer 2: memory/daily/{date}.jsonl = daily logs (written via agent tools)
//
// Features:
//   - TTL: auto-clean daily files older than memoryTTLDays on startup
//   - Dedup: same-category writes with high similarity replace the old entry
//
// Search algorithms live in memory_search.go.

type memoryChunk struct {
	Path     string
	Text     string
	Category string

	// File/Line locate this chunk's exact source line for in-place
	// rewrite (tryReplaceDuplicate) or removal (DeleteMemory) without
	// re-scanning every daily file on disk. Line is the 0-indexed
	// position within strings.Split(fileContents, "\n") — the same
	// convention every read/rewrite site in this file uses.
	//
	// File is empty for chunks parsed out of the evergreen MEMORY.md:
	// those are not per-line JSONL records and are not eligible for
	// WriteMemory's dedup-replace or DeleteMemory (both only ever
	// touch daily/*.jsonl).
	File string
	Line int
}

// categoryWeights controls search ranking priority by memory type.
var categoryWeights = map[string]float64{
	"preference": 1.5,
	"lesson":     1.3,
	"change_log": 1.2, // code change journal (for emergent-bug detection)
	"fact":       1.0,
	"context":    0.8,
}

func categoryWeight(cat string) float64 {
	if w, ok := categoryWeights[cat]; ok {
		return w
	}
	return 0.7
}

type searchResult struct {
	Path    string
	Score   float64
	Snippet string
}

type MemoryStore struct {
	workspaceDir string
	memoryDir    string
	mu           sync.RWMutex
	chunks       []memoryChunk // in-memory cache, rebuilt on startup and mutated on write/delete
}

func NewMemoryStore(dir string) *MemoryStore {
	// Store memory under {dataDir}/memory/daily/ where dataDir is the
	// resolved per-project state directory (see infra.ResolveDataDir).
	memDir := filepath.Join(dir, "memory", "daily")
	os.MkdirAll(memDir, 0o755)
	ms := &MemoryStore{workspaceDir: dir, memoryDir: memDir}
	ms.cleanExpired()
	ms.rebuildCache()
	return ms
}

// rebuildCache reloads all chunks from disk into the in-memory cache.
func (ms *MemoryStore) rebuildCache() {
	ms.chunks = ms.loadAllChunksFromDisk()
}

// cleanExpired removes daily JSONL files older than memoryTTLDays.
func (ms *MemoryStore) cleanExpired() {
	cutoff := time.Now().UTC().AddDate(0, 0, -infra.MemoryTTLDays)
	files, _ := filepath.Glob(filepath.Join(ms.memoryDir, "*.jsonl"))
	removed := 0
	for _, f := range files {
		base := strings.TrimSuffix(filepath.Base(f), ".jsonl")
		fileDate, err := time.Parse("2006-01-02", base)
		if err != nil {
			continue
		}
		if fileDate.Before(cutoff) {
			os.Remove(f)
			removed++
		}
	}
	if removed > 0 {
		logging.PrintSystem(fmt.Sprintf("[memory] Cleaned %d expired daily files (older than %d days)", removed, infra.MemoryTTLDays))
	}
}

// WriteMemory saves a fact to today's daily JSONL file.
func (ms *MemoryStore) WriteMemory(content, category string) string {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	if len(content) > infra.MaxMemoryContentLen {
		return fmt.Sprintf("Error: memory content too long (%d chars, max %d). Summarize the key facts first, then retry.",
			len(content), infra.MaxMemoryContentLen)
	}

	newTokens := tokenize(content)

	if replaced := ms.tryReplaceDuplicate(content, category, newTokens); replaced != "" {
		return replaced
	}

	today := time.Now().UTC().Format("2006-01-02")
	path := filepath.Join(ms.memoryDir, today+".jsonl")
	entry := map[string]string{
		"ts":       time.Now().UTC().Format(time.RFC3339),
		"category": category,
		"content":  content,
	}
	data, _ := json.Marshal(entry)

	// The new entry's line index is the count of entries already
	// cached for this exact file — i.e. it lands at the position
	// right after the last one, matching what O_APPEND will do on
	// disk. This only holds because every mutation in this file keeps
	// the cache and the on-disk line layout in lockstep (see cachePut
	// callers below and in tryReplaceDuplicate/DeleteMemory).
	newLine := 0
	for _, c := range ms.chunks {
		if c.File == path {
			newLine++
		}
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Sprintf("Error writing memory: %v", err)
	}
	defer f.Close()
	f.Write(data)
	f.WriteString("\n")
	// Durability: memory entries are precious (one per session
	// distillation), worth the syscall to ensure they survive a crash.
	_ = f.Sync()

	// Update in-memory cache.
	label := today + ".jsonl"
	if category != "" {
		label += " [" + category + "]"
	}
	ms.chunks = append(ms.chunks, memoryChunk{Path: label, Text: content, Category: category, File: path, Line: newLine})

	return fmt.Sprintf("Memory saved to %s.jsonl (%s)", today, category)
}

// tryReplaceDuplicate scans the in-memory chunk cache (no disk IO) for
// a same-category daily entry similar to content. On a hit, it
// rewrites only that entry's single line in its source file — not
// every daily file, as the previous Glob+ReadFile-all-files version
// did.
func (ms *MemoryStore) tryReplaceDuplicate(content, category string, newTokens []string) string {
	for i, c := range ms.chunks {
		if c.File == "" || c.Category != category {
			continue // evergreen chunk (no File) or category mismatch
		}
		sim := jaccardSimilarity(newTokens, tokenize(c.Text))
		if sim < infra.DeduplicateThreshold {
			continue
		}

		data, err := os.ReadFile(c.File)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		if c.Line < 0 || c.Line >= len(lines) {
			continue // cache out of sync with disk - fall through to a fresh append
		}
		var entry map[string]string
		if json.Unmarshal([]byte(strings.TrimSpace(lines[c.Line])), &entry) != nil {
			continue
		}
		entry["content"] = content
		entry["ts"] = time.Now().UTC().Format(time.RFC3339)
		updated, _ := json.Marshal(entry)
		lines[c.Line] = string(updated)
		os.WriteFile(c.File, []byte(strings.Join(lines, "\n")), 0o644)

		base := strings.TrimSuffix(filepath.Base(c.File), ".jsonl")
		ms.chunks[i].Text = content
		return fmt.Sprintf("Memory updated in %s.jsonl (%s) - replaced similar entry (similarity: %.0f%%)", base, category, sim*100)
	}
	return ""
}

// LoadEvergreen reads the long-term MEMORY.md file.
// Located under {workspaceDir}/memory/MEMORY.md (same parent as daily/*.jsonl).
func (ms *MemoryStore) LoadEvergreen() string {
	path := filepath.Join(ms.workspaceDir, "memory", "MEMORY.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// parseEvergreenCategory extracts an optional [category] prefix from a MEMORY.md paragraph.
// e.g. "[preference] user likes dark mode" -> ("preference", "user likes dark mode")
// Leading whitespace before the [category] tag is tolerated.
// Paragraphs without a prefix default to "fact".
var evergreenCategoryRe = regexp.MustCompile(`^\s*\[(preference|lesson|fact|context)\]\s*`)

func parseEvergreenCategory(para string) (category, text string) {
	if m := evergreenCategoryRe.FindStringSubmatch(para); m != nil {
		return m[1], strings.TrimSpace(para[len(m[0]):])
	}
	return "fact", para
}

// loadAllChunks returns the cached chunks (no disk IO).
func (ms *MemoryStore) loadAllChunks() []memoryChunk {
	return ms.chunks
}

// loadAllChunksFromDisk reads both evergreen and daily memory chunks from disk.
func (ms *MemoryStore) loadAllChunksFromDisk() []memoryChunk {
	var chunks []memoryChunk
	if eg := ms.LoadEvergreen(); eg != "" {
		for _, para := range strings.Split(eg, "\n\n") {
			para = strings.TrimSpace(para)
			if para == "" {
				continue
			}
			cat, text := parseEvergreenCategory(para)
			chunks = append(chunks, memoryChunk{Path: "MEMORY.md [" + cat + "]", Text: text, Category: cat})
		}
	}
	files, _ := filepath.Glob(filepath.Join(ms.memoryDir, "*.jsonl"))
	sort.Strings(files)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		base := filepath.Base(f)
		for idx, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var entry map[string]string
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				continue
			}
			text := entry["content"]
			if text == "" {
				continue
			}
			cat := entry["category"]
			label := base
			if cat != "" {
				label = base + " [" + cat + "]"
			}
			chunks = append(chunks, memoryChunk{Path: label, Text: text, Category: cat, File: f, Line: idx})
		}
	}
	return chunks
}

// GetStats returns memory statistics.
//
// Unlike WriteMemory/tryReplaceDuplicate/DeleteMemory (which now search
// the in-memory chunk cache instead of re-reading every daily file),
// this intentionally stays disk-based: it is only called from the
// startup banner and the `/memory` REPL command — not the per-turn hot
// path — and daily jsonl files are occasionally created/edited outside
// the WriteMemory API (tests do this; so could an operator), which the
// write-through cache has no way to observe. Trading a rarely-called
// O(N) disk scan for guaranteed freshness is the right call here.
func (ms *MemoryStore) GetStats() (evergreenChars, dailyFiles, dailyEntries int) {
	evergreenChars = len(ms.LoadEvergreen())
	files, _ := filepath.Glob(filepath.Join(ms.memoryDir, "*.jsonl"))
	dailyFiles = len(files)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) != "" {
				dailyEntries++
			}
		}
	}
	return
}

// DeleteMemory finds and removes the most similar daily memory entry
// matching query (and optionally category). Candidate search runs
// entirely against the in-memory chunk cache; only the one file that
// actually contains the winning entry is read/rewritten on disk.
func (ms *MemoryStore) DeleteMemory(query, category string) string {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	queryTokens := tokenize(query)
	if len(queryTokens) == 0 {
		return "Error: empty query - provide keywords to match the memory you want to delete."
	}

	bestSim := 0.0
	bestIdx := -1
	for i, c := range ms.chunks {
		if c.File == "" { // evergreen chunk - not eligible for delete
			continue
		}
		if category != "" && c.Category != category {
			continue
		}
		if sim := jaccardSimilarity(queryTokens, tokenize(c.Text)); sim > bestSim {
			bestSim = sim
			bestIdx = i
		}
	}

	if bestIdx < 0 || bestSim < infra.DeduplicateThreshold {
		return fmt.Sprintf("No matching memory found (best similarity: %.0f%%, threshold: %.0f%%).",
			bestSim*100, infra.DeduplicateThreshold*100)
	}

	target := ms.chunks[bestIdx]

	data, err := os.ReadFile(target.File)
	if err != nil {
		return fmt.Sprintf("Error reading file: %v", err)
	}
	lines := strings.Split(string(data), "\n")
	if target.Line < 0 || target.Line >= len(lines) {
		// Cache drifted from disk (e.g. the file was edited outside
		// this process). Self-heal by rebuilding from ground truth so
		// the next call is consistent again, rather than leaving a
		// permanently-broken index.
		ms.chunks = ms.loadAllChunksFromDisk()
		return "Error: memory index was out of sync with disk - rebuilt it, please retry the delete."
	}
	lines = append(lines[:target.Line], lines[target.Line+1:]...)
	// Clean up: remove file if empty, otherwise rewrite.
	remaining := strings.TrimSpace(strings.Join(lines, "\n"))
	if remaining == "" {
		os.Remove(target.File)
	} else {
		os.WriteFile(target.File, []byte(strings.Join(lines, "\n")), 0o644)
	}

	// Sync cache: drop the deleted chunk, then shift every other
	// chunk from the *same file* that came after the removed line up
	// by one, since the on-disk rewrite above shifted them too.
	ms.chunks = append(ms.chunks[:bestIdx], ms.chunks[bestIdx+1:]...)
	for i := range ms.chunks {
		if ms.chunks[i].File == target.File && ms.chunks[i].Line > target.Line {
			ms.chunks[i].Line--
		}
	}

	base := strings.TrimSuffix(filepath.Base(target.File), ".jsonl")
	snippet := target.Text
	if len(snippet) > 80 {
		snippet = snippet[:80] + "..."
	}
	return fmt.Sprintf("Deleted memory from %s.jsonl (similarity: %.0f%%): %s", base, bestSim*100, snippet)
}
