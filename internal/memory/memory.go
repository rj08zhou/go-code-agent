package memory

import (
	"encoding/json"
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/log"
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
	// Store memory under {workdir}/.go-code-agent/memory/daily/
	appRoot := filepath.Join(dir, infra.AppRootDirName)
	memDir := filepath.Join(appRoot, "memory", "daily")
	os.MkdirAll(memDir, 0o755)
	ms := &MemoryStore{workspaceDir: appRoot, memoryDir: memDir}
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
		log.PrintSystem(fmt.Sprintf("[memory] Cleaned %d expired daily files (older than %d days)", removed, infra.MemoryTTLDays))
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
	ms.chunks = append(ms.chunks, memoryChunk{Path: label, Text: content, Category: category})

	return fmt.Sprintf("Memory saved to %s.jsonl (%s)", today, category)
}

// tryReplaceDuplicate scans daily files for a same-category entry similar to content.
func (ms *MemoryStore) tryReplaceDuplicate(content, category string, newTokens []string) string {
	files, _ := filepath.Glob(filepath.Join(ms.memoryDir, "*.jsonl"))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var entry map[string]string
			if json.Unmarshal([]byte(line), &entry) != nil {
				continue
			}
			if entry["category"] != category {
				continue
			}
			oldTokens := tokenize(entry["content"])
			sim := jaccardSimilarity(newTokens, oldTokens)
			if sim >= infra.DeduplicateThreshold {
				oldContent := entry["content"]
				entry["content"] = content
				entry["ts"] = time.Now().UTC().Format(time.RFC3339)
				updated, _ := json.Marshal(entry)
				lines[i] = string(updated)
				os.WriteFile(f, []byte(strings.Join(lines, "\n")), 0o644)
				base := strings.TrimSuffix(filepath.Base(f), ".jsonl")

				// Sync cache: find and update the matching chunk.
				for ci := range ms.chunks {
					if ms.chunks[ci].Category == category && ms.chunks[ci].Text == oldContent {
						ms.chunks[ci].Text = content
						break
					}
				}

				return fmt.Sprintf("Memory updated in %s.jsonl (%s) - replaced similar entry (similarity: %.0f%%)", base, category, sim*100)
			}
		}
	}
	return ""
}

// LoadEvergreen reads the long-term MEMORY.md file.
func (ms *MemoryStore) LoadEvergreen() string {
	data, err := os.ReadFile(filepath.Join(ms.workspaceDir, "MEMORY.md"))
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
		for _, line := range strings.Split(string(data), "\n") {
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
			chunks = append(chunks, memoryChunk{Path: label, Text: text, Category: cat})
		}
	}
	return chunks
}

// GetStats returns memory statistics.
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

// DeleteMemory finds and removes the most similar daily memory entry matching query (and optionally category).
func (ms *MemoryStore) DeleteMemory(query, category string) string {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	queryTokens := tokenize(query)
	if len(queryTokens) == 0 {
		return "Error: empty query - provide keywords to match the memory you want to delete."
	}

	// Scan daily files for the best-matching entry.
	bestSim := 0.0
	bestFile := ""
	bestLine := -1
	bestContent := ""

	files, _ := filepath.Glob(filepath.Join(ms.memoryDir, "*.jsonl"))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var entry map[string]string
			if json.Unmarshal([]byte(line), &entry) != nil {
				continue
			}
			if category != "" && entry["category"] != category {
				continue
			}
			sim := jaccardSimilarity(queryTokens, tokenize(entry["content"]))
			if sim > bestSim {
				bestSim = sim
				bestFile = f
				bestLine = i
				bestContent = entry["content"]
			}
		}
	}

	if bestSim < infra.DeduplicateThreshold {
		return fmt.Sprintf("No matching memory found (best similarity: %.0f%%, threshold: %.0f%%).",
			bestSim*100, infra.DeduplicateThreshold*100)
	}

	// Remove the line from file.
	data, err := os.ReadFile(bestFile)
	if err != nil {
		return fmt.Sprintf("Error reading file: %v", err)
	}
	lines := strings.Split(string(data), "\n")
	lines = append(lines[:bestLine], lines[bestLine+1:]...)
	// Clean up: remove file if empty, otherwise rewrite.
	remaining := strings.TrimSpace(strings.Join(lines, "\n"))
	if remaining == "" {
		os.Remove(bestFile)
	} else {
		os.WriteFile(bestFile, []byte(strings.Join(lines, "\n")), 0o644)
	}

	// Sync cache: remove the matching chunk.
	for i := range ms.chunks {
		if ms.chunks[i].Text == bestContent {
			ms.chunks = append(ms.chunks[:i], ms.chunks[i+1:]...)
			break
		}
	}

	base := strings.TrimSuffix(filepath.Base(bestFile), ".jsonl")
	snippet := bestContent
	if len(snippet) > 80 {
		snippet = snippet[:80] + "..."
	}
	return fmt.Sprintf("Deleted memory from %s.jsonl (similarity: %.0f%%): %s", base, bestSim*100, snippet)
}
