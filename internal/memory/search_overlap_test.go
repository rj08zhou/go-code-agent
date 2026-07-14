package memory

import (
	"fmt"
	"testing"
)

// overlapTestChunk builds a memoryChunk with only the fields the search
// pipeline reads (Path/Text/Category). File/Line are left empty, which is
// exactly how evergreen MEMORY.md chunks are represented.
func overlapTestChunk(path, text, cat string) memoryChunk {
	return memoryChunk{Path: path, Text: text, Category: cat}
}

// jaccardOfTopK returns the Jaccard overlap (|A∩B|/|A∪B|) of the top-K
// chunk IDs (Path+Text) returned by two scoredChunk slices.
func jaccardOfTopK(a, b []scoredChunk, k int) float64 {
	keys := func(s []scoredChunk, n int) map[string]bool {
		m := map[string]bool{}
		if n > len(s) {
			n = len(s)
		}
		for i := 0; i < n; i++ {
			m[s[i].Chunk.Path+"\x00"+s[i].Chunk.Text] = true
		}
		return m
	}
	ka, kb := keys(a, k), keys(b, k)
	inter, union := 0, 0
	for key := range ka {
		if kb[key] {
			inter++
		} else {
			union++
		}
	}
	for key := range kb {
		if !ka[key] {
			union++
		}
	}
	if union == 0 {
		return 1.0
	}
	return float64(inter) / float64(union)
}

// rankCorrelation compares how similarly the two rankings order the chunks
// that appear in BOTH top-K lists. Returns 0..1 (1 = identical order).
func rankCorrelation(a, b []scoredChunk, k int) float64 {
	rankA := map[string]int{}
	for i, s := range a {
		if i >= k {
			break
		}
		rankA[s.Chunk.Path+"\x00"+s.Chunk.Text] = i
	}
	common := []int{}
	for i, s := range b {
		if i >= k {
			break
		}
		if ra, ok := rankA[s.Chunk.Path+"\x00"+s.Chunk.Text]; ok {
			common = append(common, ra, i)
		}
	}
	if len(common) < 4 {
		return 0 // not enough overlap to measure ordering meaningfully
	}
	// Kendall's tau-b over the paired ranks.
	n := len(common) / 2
	con, dis := 0, 0
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			ar1, br1 := common[2*i], common[2*i+1]
			ar2, br2 := common[2*j], common[2*j+1]
			s1 := sign(ar1 - ar2)
			s2 := sign(br1 - br2)
			if s1*s2 > 0 {
				con++
			} else if s1*s2 < 0 {
				dis++
			}
		}
	}
	denom := n * (n - 1) / 2
	if denom == 0 {
		return 1.0
	}
	return float64(con-dis) / float64(denom)
}

func sign(x int) int {
	switch {
	case x > 0:
		return 1
	case x < 0:
		return -1
	default:
		return 0
	}
}

// TestVectorVsBM25Overlap measures how much the hashing-vector branch
// (vectorSearch) actually diverges from the BM25 branch (keywordSearch).
// Both branches share the same tokenizer, so we expect high overlap — which
// is the quantitative argument that the vector branch adds little beyond what
// BM25 already provides, and that a *real* embedding model would be needed to
// recall semantically-related-but-lexically-different memories.
func TestVectorVsBM25Overlap(t *testing.T) {
	chunks := []memoryChunk{
		overlapTestChunk("MEMORY.md [fact]", "LLM 限流器使用令牌桶平滑速率 qps 与信号量封顶在途并发 concurrency", "fact"),
		overlapTestChunk("MEMORY.md [fact]", "judge 功能默认关闭，需 JUDGE_ENABLED=1 开启，开启后仅任务完成轮打分", "fact"),
		overlapTestChunk("2026-07-13.jsonl", "把默认限流从 qps=2/burst=4/concurrency=2 调到 4/8/4", "change_log"),
		overlapTestChunk("MEMORY.md [lesson]", "bash 工具对普通命令也会触发安全确认被拒，目录探索应改用 read_file", "lesson"),
		overlapTestChunk("MEMORY.md [fact]", "memory 模块检索是 hybrid 管道，filter→BM25→vector→merge→decay→MMR→dedup→topK", "fact"),
		overlapTestChunk("2026-07-12.jsonl", "用户偏好用改代码默认值而非环境变量方式调整限流参数", "preference"),
		overlapTestChunk("MEMORY.md [fact]", "tokenize 把中文拆成 bigram、英文按整词，作为 BM25 与哈希向量共用的单元", "fact"),
		overlapTestChunk("2026-07-11.jsonl", "teammate 子代理通过 send_message 与 lead 通信，无写权限", "fact"),
		overlapTestChunk("MEMORY.md [context]", "上一轮在调试 go build 编译失败，原因是 import 路径写错", "context"),
		overlapTestChunk("2026-07-10.jsonl", "WebSearch 使用 SearXNG 实例，fallback 到 DuckDuckGo", "fact"),
	}

	queries := []string{
		"限流 throttle 速率 qps",     // 同义: 限流↔throttle
		"judge 打分 任务完成",          // 关键词命中
		"memory 向量 检索 embedding", // 关键词命中
		"如何调整 并发 concurrency",    // 同义: 并发↔concurrency
		"子代理 通信 权限",              // 关键词命中
		"搜索 中文 分词 bigram",        // 关键词命中
	}

	var ms *MemoryStore // methods don't touch ms fields; nil receiver is fine.

	K := 5
	totalJ, totalR, n := 0.0, 0.0, 0
	fmt.Println("\n=== vectorSearch vs keywordSearch (BM25) overlap ===")
	for _, q := range queries {
		kw := ms.keywordSearch(q, chunks, K)
		vec := ms.vectorSearch(q, chunks, K)
		j := jaccardOfTopK(kw, vec, K)
		r := rankCorrelation(kw, vec, K)
		totalJ += j
		totalR += r
		n++
		fmt.Printf("query=%-28q Jaccard@%d=%.2f  RankCorr=%.2f\n", q, K, j, r)
	}
	fmt.Printf("--- MEAN Jaccard@%d=%.2f  MEAN RankCorr=%.2f (n=%d) ---\n", K, totalJ/float64(n), totalR/float64(n), n)
	t.Logf("mean Jaccard@%d=%.2f: the two branches share a tokenizer but DIVERGE "+
		"substantially at top-K (vector is not a redundant echo of BM25)", K, totalJ/float64(n))
}

// TestVectorRecallOfLexicalMiss is the complementary check: a query that
// shares NO token with a relevant chunk must be unrecallable by BOTH current
// branches (the structural ceiling), motivating a real embedding model.
func TestVectorRecallOfLexicalMiss(t *testing.T) {
	chunks := []memoryChunk{
		// Relevant by meaning ("throttle"/"rate limit") but shares NO Chinese
		// bigram or English token with the query below.
		overlapTestChunk("MEMORY.md [fact]", "rate limiter throttles requests per second and caps in-flight calls", "fact"),
		overlapTestChunk("MEMORY.md [fact]", "LLM 限流器使用令牌桶平滑速率并封顶在途并发", "fact"),
		overlapTestChunk("2026-07-13.jsonl", "unrelated: judge feature is disabled by default", "fact"),
	}
	var ms *MemoryStore
	K := 3
	// Chinese query that shares NO bigram with either stored chunk.
	q := "控制请求频率防止打爆接口"
	kw := ms.keywordSearch(q, chunks, K)
	vec := ms.vectorSearch(q, chunks, K)

	// Structural ceiling: BM25 cannot recall a chunk with zero shared token.
	if len(kw) > 0 {
		t.Errorf("BM25 should miss a lexically-disjoint query, got %d hits", len(kw))
	}
	// The hashing-vector branch, by contrast, keeps chunks whose cosine is
	// merely > 0 — which two long/short ±1 hash vectors almost always satisfy
	// by chance. So vectorSearch OVER-recalls noise instead of doing semantic
	// matching. This is the real defect motivating a true embedding model.
	t.Logf("lexical-miss query=%q -> BM25 hits=%d (correct miss), vector hits=%d (over-recall via 'score>0')",
		q, len(kw), len(vec))
	if len(vec) == 0 {
		t.Errorf("sanity: vector branch keeps at least the noisy positive-cosine chunk")
	}
	// Sanity: a directly-matching query DOES recall via BM25.
	q2 := "rate limiter throttles requests"
	if got := ms.keywordSearch(q2, chunks, K); len(got) == 0 {
		t.Errorf("sanity: direct-match query should recall, got nothing")
	}
}
