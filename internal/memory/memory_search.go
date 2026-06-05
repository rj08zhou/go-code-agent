package memory

import (
	"go-code-agent/infra"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Memory search - tokenization, TF-IDF, vector, Jaccard, hybrid pipeline

// tokenizeRe extracts contiguous runs of ASCII letters/digits OR CJK
// characters. ASCII tokens are kept whole; CJK runs are further split
// into character bigrams below — Chinese has no inter-word whitespace,
// and bigrams are the standard way to give BM25 / vector search a
// usable unit of meaning without a real segmenter.
var tokenizeRe = regexp.MustCompile(`[a-z0-9]+|[\x{4e00}-\x{9fff}]+`)

func tokenize(text string) []string {
	matches := tokenizeRe.FindAllString(strings.ToLower(text), -1)
	var result []string
	for _, m := range matches {
		// CJK run: emit character bigrams ("你好世界" -> "你好","好世","世界").
		// A single CJK character is kept as-is (no bigram possible).
		if r := []rune(m); r[0] >= 0x4e00 && r[0] <= 0x9fff {
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
		// ASCII / digit token: keep verbatim (incl. single-char tokens
		// like "a","b" — TestTokenize requires these to survive so
		// short queries / acronyms remain searchable).
		result = append(result, m)
	}
	return result
}

//
// BM25 improves on raw TF-IDF + cosine with two critical properties:
//   1. TF saturation via k1: repeating a word gives diminishing returns
//      (10 occurrences ≠ 10× the relevance of 1 occurrence).
//   2. Length normalization via b: short documents are favored over long ones
//      when they contain the query terms at similar density.
//
// Formula (Lucene-style, ensures non-negative IDF):
//   BM25(q, d) = Σ_{t ∈ q}  idf(t) · tf(t,d)·(k1+1) / (tf(t,d) + k1·(1 − b + b·|d|/avgdl))
//   idf(t)    = log(1 + (N − df(t) + 0.5) / (df(t) + 0.5))
//
// Unlike cosine similarity, BM25 scores are unbounded, so `keywordSearch`
// normalizes results to [0, 1] before returning - this keeps the downstream
// MMR / temporal-decay / category-weight pipeline in a consistent scale.

// bm25Score computes BM25 between queryTokens and docTokens.
// df / n describe the corpus; avgdl is the average document length.
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
			continue // each query term contributes once (standard BM25)
		}
		seenQ[q] = true
		tf := float64(docTF[q])
		if tf == 0 {
			continue
		}
		idf := math.Log(1 + (float64(n)-float64(df[q])+0.5)/(float64(df[q])+0.5))
		norm := tf * (infra.Bm25K1 + 1) / (tf + infra.Bm25K1*(1-infra.Bm25B+infra.Bm25B*dl/avgdl))
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

type scoredChunk struct {
	Chunk memoryChunk
	Score float64
}

func (ms *MemoryStore) keywordSearch(query string, chunks []memoryChunk, topK int) []scoredChunk {
	queryTokens := tokenize(query)
	if len(queryTokens) == 0 {
		return nil
	}

	// Pass 1: tokenize all chunks, build document-frequency map, sum total length.
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

	// Pass 2: BM25 score each chunk.
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

	// Normalize to [0, 1] so downstream pipeline (MMR, decay, category weight,
	// hybrid merge with vector cosine) stays in a consistent scale.
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

func (ms *MemoryStore) vectorSearch(query string, chunks []memoryChunk, topK int) []scoredChunk {
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

// decayRateForCategory returns the temporal-decay rate λ such that
//
//	score(t) = score(0) * exp(-λ * ageDays)
//
// Half-life = ln(2) / λ ≈ 0.693 / λ days.
//
// Rationale per category:
//   - context:    volatile, "what I was doing yesterday" → decays fast
//   - change_log: code-change history needs to stay discoverable for weeks
//     so future reviews can spot emergent bugs from combined changes
//   - others:     default mid-life for lesson/fact/preference
func decayRateForCategory(cat string) float64 {
	switch cat {
	case "change_log":
		return 0.003 // half-life ~231 days - preserve change history for cross-PR analysis
	case "context":
		return 0.02 // half-life ~35 days - let transient context fade
	default:
		return 0.01 // half-life ~70 days - default
	}
}

// temporalDecayByCategory applies per-category decay rates.
// Chunks whose Path lacks a date (evergreen MEMORY.md) are not decayed.
func temporalDecayByCategory(results []scoredChunk) []scoredChunk {
	return temporalDecayByCategoryWithNow(results, time.Now().UTC())
}

// temporalDecayByCategoryWithNow applies per-category decay rates using a fixed reference time.
// Useful for testing with deterministic time.
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

// deduplicateResults removes near-duplicate chunks based on Jaccard similarity.
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

// HybridSearch is the basic search entrypoint with no filters.
func (ms *MemoryStore) HybridSearch(query string, topK int) []searchResult {
	return ms.HybridSearchFiltered(query, topK, 0, "")
}

// HybridSearchFiltered runs the full pipeline with optional filters:
//   - withinDays: if > 0, only consider chunks from the last N days (by date in path).
//     Evergreen MEMORY.md has no date and is always included.
//   - category: if non-empty, only consider chunks of this category.
//
// Pipeline: filter → keyword (BM25) → vector → merge → decay → category weight → MMR → dedup → topK.
func (ms *MemoryStore) HybridSearchFiltered(query string, topK int, withinDays int, category string) []searchResult {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	chunks := filterChunks(ms.loadAllChunks(), withinDays, category)
	if len(chunks) == 0 {
		return nil
	}

	// Empty query: return all chunks (useful for "show all memories")
	if query == "" {
		var results []searchResult
		for i, c := range chunks {
			if i >= topK {
				break
			}
			score := categoryWeight(c.Category)
			if score > 1 {
				score = 1
			}
			snippet := c.Text
			if len(snippet) > 200 {
				snippet = snippet[:200] + "..."
			}
			results = append(results, searchResult{
				Path:    c.Path,
				Score:   math.Round(score*10000) / 10000,
				Snippet: snippet,
			})
		}
		return results
	}

	kw := ms.keywordSearch(query, chunks, 10)
	vec := ms.vectorSearch(query, chunks, 10)
	merged := mergeHybridResults(vec, kw, infra.HybridVectorWeight, infra.HybridKeywordWeight)

	for i := range merged {
		merged[i].Score *= categoryWeight(merged[i].Chunk.Category)
	}

	decayed := temporalDecayByCategory(merged)
	reranked := mmrRerank(decayed, 0.7)
	reranked = deduplicateResults(reranked, infra.DeduplicateThreshold)

	var results []searchResult
	for i, r := range reranked {
		if i >= topK {
			break
		}
		snippet := r.Chunk.Text
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		// Clip to [0,1]. The pipeline multiplies by category weights
		// (up to 1.5) which can push a normalised BM25 score above 1;
		// callers treat Score as a [0,1] confidence value.
		score := r.Score
		if score > 1 {
			score = 1
		}
		if score < 0 {
			score = 0
		}
		results = append(results, searchResult{
			Path:    r.Chunk.Path,
			Score:   math.Round(score*10000) / 10000,
			Snippet: snippet,
		})
	}
	return results
}

// filterChunks applies within-days and category filters. An empty filter value means "no filter".
// Evergreen chunks (MEMORY.md) are always kept regardless of date filter, but still respect category.
func filterChunks(chunks []memoryChunk, withinDays int, category string) []memoryChunk {
	if withinDays <= 0 && category == "" {
		return chunks
	}
	var cutoff time.Time
	if withinDays > 0 {
		cutoff = time.Now().UTC().AddDate(0, 0, -withinDays)
	}
	out := make([]memoryChunk, 0, len(chunks))
	for _, c := range chunks {
		if category != "" && c.Category != category {
			continue
		}
		if withinDays > 0 {
			// Evergreen (MEMORY.md...) chunks have no date in path - always include.
			if m := dateInPathRe.FindString(c.Path); m != "" {
				if d, err := time.Parse("2006-01-02", m); err == nil && d.Before(cutoff) {
					continue
				}
			}
		}
		out = append(out, c)
	}
	return out
}
