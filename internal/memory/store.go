// Package memory provides long-term memory persistence with TTL cleanup,
// deduplication, and structured daily JSONL storage.
package memory

import (
	"encoding/json"
	"fmt"
	"go-code-agent/internal/config"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store persists user preferences, lessons, and facts across sessions.
// Daily entries use structured JSONL with TTL auto-cleanup and
// deduplication via Jaccard similarity.
type Store struct {
	dataDir  string
	dailyDir string
	mu       sync.RWMutex
	chunks   []memoryChunk // in-memory cache, rebuilt on startup
}

func NewStore(dataDir string) *Store {
	dailyDir := filepath.Join(dataDir, "daily")
	if err := os.MkdirAll(dailyDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "[warn] memory: create daily dir: %v\n", err)
	}
	s := &Store{dataDir: dataDir, dailyDir: dailyDir}
	s.cleanExpired()
	s.rebuildCache()
	return s
}

// --- paths ---

func (s *Store) evergreenPath() string {
	return filepath.Join(s.dataDir, "MEMORY.md")
}

func (s *Store) dailyFilePath(t time.Time) string {
	return filepath.Join(s.dailyDir, t.Format("2006-01-02")+".jsonl")
}

// --- TTL cleanup ---

func (s *Store) cleanExpired() {
	cutoff := time.Now().UTC().AddDate(0, 0, -config.MemoryTTLDays)
	files, _ := filepath.Glob(filepath.Join(s.dailyDir, "*.jsonl"))
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
		fmt.Printf("[memory] cleaned %d expired daily files (older than %d days)\n", removed, config.MemoryTTLDays)
	}
}

// --- cache ---

func (s *Store) rebuildCache() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chunks = s.loadAllFromDisk()
}

// --- Write ---

func (s *Store) Write(content, category string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(content) > config.MaxMemoryContentLen {
		return fmt.Sprintf("Error: memory content too long (%d chars, max %d)", len(content), config.MaxMemoryContentLen)
	}
	if category == "" {
		category = "fact"
	}

	// Dedup: if a same-category daily entry is similar, replace it.
	if replaced := s.tryReplaceDuplicate(content, category); replaced != "" {
		return replaced
	}

	today := time.Now().UTC().Format("2006-01-02")
	path := filepath.Join(s.dailyDir, today+".jsonl")
	entry := map[string]string{
		"ts":       time.Now().UTC().Format(time.RFC3339),
		"category": category,
		"content":  content,
	}
	data, _ := json.Marshal(entry)

	// Compute line index = number of cached entries for this file.
	newLine := 0
	for _, c := range s.chunks {
		if c.File == path {
			newLine++
		}
	}

	if err := os.MkdirAll(s.dailyDir, 0o755); err != nil {
		return fmt.Sprintf("Error writing memory: %v", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Sprintf("Error writing memory: %v", err)
	}
	defer f.Close()
	f.Write(data)
	f.WriteString("\n")
	_ = f.Sync()

	label := today + ".jsonl"
	if category != "" {
		label += " [" + category + "]"
	}
	s.chunks = append(s.chunks, memoryChunk{
		Path: label, Text: content, Category: category,
		File: path, Line: newLine,
	})
	return fmt.Sprintf("Saved [%s]: %s", category, content)
}

func (s *Store) tryReplaceDuplicate(content, category string) string {
	newTokens := tokenize(content)
	for i, c := range s.chunks {
		if c.File == "" || c.Category != category {
			continue
		}
		sim := jaccardSimilarity(newTokens, tokenize(c.Text))
		if sim < config.DeduplicateThreshold {
			continue
		}
		data, err := os.ReadFile(c.File)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		if c.Line < 0 || c.Line >= len(lines) {
			continue
		}
		var oldEntry map[string]string
		if json.Unmarshal([]byte(strings.TrimSpace(lines[c.Line])), &oldEntry) != nil {
			continue
		}
		oldEntry["content"] = content
		oldEntry["ts"] = time.Now().UTC().Format(time.RFC3339)
		updated, _ := json.Marshal(oldEntry)
		lines[c.Line] = string(updated)
		os.WriteFile(c.File, []byte(strings.Join(lines, "\n")), 0o644)
		s.chunks[i].Text = content
		base := strings.TrimSuffix(filepath.Base(c.File), ".jsonl")
		return fmt.Sprintf("Memory updated in %s.jsonl (%s) — replaced similar entry (%.0f%%)", base, category, sim*100)
	}
	return ""
}

// --- Search ---

func (s *Store) Search(query string, topK, withinDays int, category string) string {
	if topK <= 0 {
		topK = 5
	}
	results := s.HybridSearchFiltered(query, topK, withinDays, category)
	if len(results) == 0 {
		return "No relevant memories found."
	}
	var lines []string
	for _, r := range results {
		lines = append(lines, r.Path+": "+r.Snippet)
	}
	return strings.Join(lines, "\n")
}

// --- Delete ---

func (s *Store) Delete(query, category string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	queryTokens := tokenize(query)
	if len(queryTokens) == 0 {
		return "Error: empty query"
	}

	bestSim := 0.0
	bestIdx := -1
	for i, c := range s.chunks {
		if c.File == "" {
			continue // doesn't delete evergreen via keyword
		}
		if category != "" && c.Category != category {
			continue
		}
		if sim := jaccardSimilarity(queryTokens, tokenize(c.Text)); sim > bestSim {
			bestSim = sim
			bestIdx = i
		}
	}
	if bestIdx < 0 || bestSim < config.DeduplicateThreshold {
		return fmt.Sprintf("No matching memory found (best similarity: %.0f%%, threshold: %.0f%%).", bestSim*100, config.DeduplicateThreshold*100)
	}

	target := s.chunks[bestIdx]
	data, err := os.ReadFile(target.File)
	if err != nil {
		return fmt.Sprintf("Error reading file: %v", err)
	}
	lines := strings.Split(string(data), "\n")
	if target.Line < 0 || target.Line >= len(lines) {
		s.chunks = s.loadAllFromDisk()
		return "Error: index out of sync - rebuilt, please retry."
	}
	lines = append(lines[:target.Line], lines[target.Line+1:]...)
	remaining := strings.TrimSpace(strings.Join(lines, "\n"))
	if remaining == "" {
		os.Remove(target.File)
	} else {
		os.WriteFile(target.File, []byte(strings.Join(lines, "\n")), 0o644)
	}
	s.chunks = append(s.chunks[:bestIdx], s.chunks[bestIdx+1:]...)
	for i := range s.chunks {
		if s.chunks[i].File == target.File && s.chunks[i].Line > target.Line {
			s.chunks[i].Line--
		}
	}

	base := filepath.Base(target.File)
	snippet := target.Text
	if len(snippet) > 80 {
		snippet = snippet[:80] + "..."
	}
	return fmt.Sprintf("Deleted memory from %s (similarity: %.0f%%): %s", base, bestSim*100, snippet)
}

// --- Stats ---

func (s *Store) Stats() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var ec int
	if data, err := os.ReadFile(s.evergreenPath()); err == nil {
		ec = len(data)
	}
	entries, _ := os.ReadDir(s.dailyDir)
	df := len(entries)
	de := len(s.chunks)
	for _, c := range s.chunks {
		if c.File == "" {
			de-- // evergreen entries counted in ec
		}
	}
	return fmt.Sprintf("evergreen: %d chars, daily files: %d, entries: %d", ec, df, de)
}

// --- Evergreen ---

func (s *Store) GetEvergreen() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, err := os.ReadFile(s.evergreenPath())
	if err != nil {
		return ""
	}
	content := string(data)
	if len(content) > config.MaxEvergreenChars {
		content = content[len(content)-config.MaxEvergreenChars:]
	}
	return content
}

func (s *Store) SaveSessionMemory(sessionID, summary string) string {
	return s.Write(fmt.Sprintf("session %s: %s", sessionID, summary), "session")
}

// parseEvergreenCategory extracts an optional [category] prefix from a
// MEMORY.md paragraph. e.g. "[preference] user likes dark mode" →
// ("preference", "user likes dark mode"). Defaults to "fact".
func parseEvergreenCategory(para string) (category, text string) {
	// Try regex match first
	if m := evergreenCategoryRe.FindStringSubmatch(para); m != nil {
		return m[1], strings.TrimSpace(para[len(m[0]):])
	}
	// Fallback: simple [tag] prefix
	if idx := strings.Index(para, "] "); idx > 0 && strings.HasPrefix(para, "[") {
		return para[1:idx], strings.TrimSpace(para[idx+2:])
	}
	return "fact", para
}

var evergreenCategoryRe = regexp.MustCompile(`^\s*\[(preference|lesson|fact|context)\]\s*`)

// --- Disk loading ---

func (s *Store) loadAllFromDisk() []memoryChunk {
	var chunks []memoryChunk

	// MEMORY.md — split by paragraphs (matching original project).
	if data, err := os.ReadFile(s.evergreenPath()); err == nil {
		for _, para := range strings.Split(string(data), "\n\n") {
			para = strings.TrimSpace(para)
			if para == "" {
				continue
			}
			cat, text := parseEvergreenCategory(para)
			chunks = append(chunks, memoryChunk{
				Path: fmt.Sprintf("MEMORY.md [%s]", cat), Text: text,
				Category: cat, File: "",
			})
		}
	}

	// Daily JSONL
	files, _ := filepath.Glob(filepath.Join(s.dailyDir, "*.jsonl"))
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
			if json.Unmarshal([]byte(line), &entry) != nil {
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
			chunks = append(chunks, memoryChunk{
				Path: label, Text: text, Category: cat,
				File: f, Line: idx,
			})
		}
	}
	return chunks
}
