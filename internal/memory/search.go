package memory

import (
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var tokenizeRe = regexp.MustCompile(`[a-z0-9]+|[\x{4e00}-\x{9fff}]+`)

func tokenize(text string) []string {
	matches := tokenizeRe.FindAllString(strings.ToLower(text), -1)
	var result []string
	for _, m := range matches {
		if r := []rune(m); len(r) > 0 && r[0] >= 0x4e00 && r[0] <= 0x9fff {
			if len(r) == 1 {
				result = append(result, m)
				continue
			}
			for i := 0; i+1 < len(r); i += 2 {
				if i+2 > len(r) {
					break
				}
				result = append(result, string(r[i:i+2]))
			}
			continue
		}
		result = append(result, m)
	}
	return result
}

const (
	bm25K1              = 1.5
	bm25B               = 0.75
	hybridKeywordWeight = 0.65
	hybridVectorWeight  = 0.35
)

type memoryChunk struct {
	Path     string
	Text     string
	Category string
	File     string
	Line     int
}

type searchResult struct {
	Path    string
	Score   float64
	Snippet string
}

type scoredChunk struct {
	Chunk memoryChunk
	Score float64
}

func categoryWeight(cat string) float64 {
	weights := map[string]float64{
		"preference": 1.5, "lesson": 1.3,
		"change_log": 1.2, "fact": 1.0, "context": 0.8,
	}
	if w, ok := weights[cat]; ok {
		return w
	}
	return 0.7
}

func bm25Score(queryTokens, docTokens []string, df map[string]int, n int, avgdl float64) float64 {
	dl := float64(len(docTokens))
	if dl == 0 || avgdl == 0 {
		return 0
	}
	docTF := make(map[string]int, len(docTokens))
	for _, t := range docTokens {
		docTF[t]++
	}
	score := 0.0
	seenQ := make(map[string]bool)
	for _, q := range queryTokens {
		if seenQ[q] {
			continue
		}
		seenQ[q] = true
		tf := float64(docTF[q])
		if tf == 0 {
			continue
		}
		idf := math.Log(1 + (float64(n)-float64(df[q])+0.5)/(float64(df[q])+0.5))
		norm := tf * (bm25K1 + 1) / (tf + bm25K1*(1-bm25B+bm25B*dl/avgdl))
		score += idf * norm
	}
	return score
}

func hashVector(text string, dim int) []float64 {
	tokens := tokenize(text)
	vec := make([]float64, dim)
	for _, token := range tokens {
		h := fnvHash(token)
		for i := range dim {
			bit := (h >> (uint(i) % 62)) & 1
			if bit == 1 {
				vec[i] += 1.0
			} else {
				vec[i] -= 1.0
			}
		}
	}
	norm := 0.0
	for _, v := range vec {
		norm += v * v
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		norm = 1
	}
	for i := range vec {
		vec[i] /= norm
	}
	return vec
}

func fnvHash(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

func vectorCosine(a, b []float64) float64 {
	dot, na, nb := 0.0, 0.0, 0.0
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	denom := math.Sqrt(na) * math.Sqrt(nb)
	if denom == 0 {
		return 0
	}
	return dot / denom
}

func jaccardSimilarity(a, b []string) float64 {
	setA := make(map[string]bool)
	for _, t := range a {
		setA[t] = true
	}
	setB := make(map[string]bool)
	for _, t := range b {
		setB[t] = true
	}
	inter := 0
	for k := range setA {
		if setB[k] {
			inter++
		}
	}
	union := len(setA) + len(setB) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func (s *Store) keywordSearch(query string, chunks []memoryChunk, topK int) []scoredChunk {
	queryTokens := tokenize(query)
	if len(queryTokens) == 0 {
		return nil
	}
	chunkTokens := make([][]string, len(chunks))
	df := make(map[string]int)
	totalLen := 0
	for i, c := range chunks {
		tokens := tokenize(c.Text)
		chunkTokens[i] = tokens
		totalLen += len(tokens)
		seen := make(map[string]bool)
		for _, t := range tokens {
			if !seen[t] {
				df[t]++
				seen[t] = true
			}
		}
	}
	n := len(chunks)
	if n == 0 {
		return nil
	}
	avgdl := float64(totalLen) / float64(n)
	var scored []scoredChunk
	for i, tokens := range chunkTokens {
		if len(tokens) == 0 {
			continue
		}
		score := bm25Score(queryTokens, tokens, df, n, avgdl)
		if score > 0 {
			scored = append(scored, scoredChunk{Chunk: chunks[i], Score: score})
		}
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	if len(scored) > 0 {
		maxScore := scored[0].Score
		if maxScore > 0 {
			for i := range scored {
				scored[i].Score /= maxScore
			}
		}
	}
	if len(scored) > topK {
		scored = scored[:topK]
	}
	return scored
}

func (s *Store) vectorSearch(query string, chunks []memoryChunk, topK int) []scoredChunk {
	qvec := hashVector(query, 64)
	var scored []scoredChunk
	for _, c := range chunks {
		cvec := hashVector(c.Text, 64)
		score := vectorCosine(qvec, cvec)
		if score > 0 {
			scored = append(scored, scoredChunk{Chunk: c, Score: score})
		}
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	if len(scored) > topK {
		scored = scored[:topK]
	}
	return scored
}

func mergeHybridResults(vectorResults, keywordResults []scoredChunk, vw, tw float64) []scoredChunk {
	merged := make(map[string]*scoredChunk)
	for _, r := range vectorResults {
		key := r.Chunk.Text
		if len(key) > 100 {
			key = key[:100]
		}
		merged[key] = &scoredChunk{Chunk: r.Chunk, Score: r.Score * vw}
	}
	for _, r := range keywordResults {
		key := r.Chunk.Text
		if len(key) > 100 {
			key = key[:100]
		}
		if existing, ok := merged[key]; ok {
			existing.Score += r.Score * tw
		} else {
			merged[key] = &scoredChunk{Chunk: r.Chunk, Score: r.Score * tw}
		}
	}
	var result []scoredChunk
	for _, v := range merged {
		result = append(result, *v)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Score > result[j].Score })
	return result
}

var dateInPathRe = regexp.MustCompile(`(\d{4}-\d{2}-\d{2})`)

func decayRateForCategory(cat string) float64 {
	switch cat {
	case "change_log":
		return 0.003
	case "context":
		return 0.02
	default:
		return 0.01
	}
}

func temporalDecayByCategory(results []scoredChunk) []scoredChunk {
	return temporalDecayByCategoryWithNow(results, time.Now().UTC())
}

func temporalDecayByCategoryWithNow(results []scoredChunk, now time.Time) []scoredChunk {
	for i := range results {
		path := results[i].Chunk.Path
		m := dateInPathRe.FindString(path)
		if m == "" {
			continue
		}
		d, err := time.Parse("2006-01-02", m)
		if err != nil {
			continue
		}
		ageDays := now.Sub(d).Hours() / 24.0
		rate := decayRateForCategory(results[i].Chunk.Category)
		results[i].Score *= math.Exp(-rate * ageDays)
	}
	return results
}

func mmrRerank(results []scoredChunk, lambda float64) []scoredChunk {
	if len(results) <= 1 {
		return results
	}
	tokenized := make([][]string, len(results))
	for i, r := range results {
		tokenized[i] = tokenize(r.Chunk.Text)
	}
	selected := make([]int, 0, len(results))
	remaining := make(map[int]bool)
	for i := range results {
		remaining[i] = true
	}
	var reranked []scoredChunk
	for len(remaining) > 0 {
		bestIdx := -1
		bestMMR := math.Inf(-1)
		for idx := range remaining {
			relevance := results[idx].Score
			maxSim := 0.0
			for _, selIdx := range selected {
				sim := jaccardSimilarity(tokenized[idx], tokenized[selIdx])
				if sim > maxSim {
					maxSim = sim
				}
			}
			mmr := lambda*relevance - (1-lambda)*maxSim
			if mmr > bestMMR {
				bestMMR = mmr
				bestIdx = idx
			}
		}
		selected = append(selected, bestIdx)
		delete(remaining, bestIdx)
		reranked = append(reranked, results[bestIdx])
	}
	return reranked
}

func deduplicateResults(results []scoredChunk, threshold float64) []scoredChunk {
	var deduped []scoredChunk
	dedupedTokens := make([][]string, 0, len(results))
	for _, r := range results {
		rTokens := tokenize(r.Chunk.Text)
		isDup := false
		for _, et := range dedupedTokens {
			if jaccardSimilarity(rTokens, et) >= threshold {
				isDup = true
				break
			}
		}
		if !isDup {
			deduped = append(deduped, r)
			dedupedTokens = append(dedupedTokens, rTokens)
		}
	}
	return deduped
}

func (s *Store) LoadAllChunks() []memoryChunk {
	chunks, _ := s.loadChunks()
	return chunks
}

func (s *Store) loadChunks() ([]memoryChunk, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.chunks) == 0 {
		return nil, nil
	}
	out := make([]memoryChunk, len(s.chunks))
	copy(out, s.chunks)
	return out, nil
}

func (s *Store) HybridSearch(query string, topK int) []searchResult {
	return s.HybridSearchFiltered(query, topK, 0, "")
}

func (s *Store) HybridSearchFiltered(query string, topK int, withinDays int, category string) []searchResult {
	if topK <= 0 {
		topK = 5
	}
	// Fallback to simple sub-string search if no chunks loaded
	allChunks := s.LoadAllChunks()
	if len(allChunks) == 0 {
		return s.simpleSearch(query, topK)
	}

	// Apply withinDays / category filters.
	cutoff := time.Now().AddDate(0, 0, -withinDays)
	var chunks []memoryChunk
	for _, c := range allChunks {
		if category != "" && c.Category != category {
			continue
		}
		if c.File != "" && withinDays > 0 {
			if fileDate, err := time.Parse("2006-01-02", strings.TrimSuffix(filepath.Base(c.File), ".jsonl")); err == nil && fileDate.Before(cutoff) {
				continue
			}
		}
		chunks = append(chunks, c)
	}
	if len(chunks) == 0 {
		return nil
	}

	if query == "" {
		var results []searchResult
		for i, c := range chunks {
			if i >= topK {
				break
			}
			snippet := c.Text
			if len(snippet) > 200 {
				snippet = snippet[:200] + "..."
			}
			results = append(results, searchResult{
				Path: c.Path, Score: 0.5, Snippet: snippet,
			})
		}
		return results
	}

	kw := s.keywordSearch(query, chunks, 10)
	vec := s.vectorSearch(query, chunks, 10)
	merged := mergeHybridResults(vec, kw, hybridVectorWeight, hybridKeywordWeight)

	for i := range merged {
		merged[i].Score *= categoryWeight(merged[i].Chunk.Category)
	}
	decayed := temporalDecayByCategory(merged)
	reranked := mmrRerank(decayed, 0.7)
	reranked = deduplicateResults(reranked, 0.7)

	var results []searchResult
	for i, r := range reranked {
		if i >= topK {
			break
		}
		snippet := r.Chunk.Text
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		score := r.Score
		if score > 1 {
			score = 1
		}
		if score < 0 {
			score = 0
		}
		results = append(results, searchResult{
			Path: r.Chunk.Path, Score: math.Round(score*10000) / 10000, Snippet: snippet,
		})
	}
	return results
}

func (s *Store) simpleSearch(query string, topK int) []searchResult {
	return []searchResult{}
}
