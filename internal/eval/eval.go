// Package eval provides a lightweight evaluation harness for the
// go-code-agent. It drives the agent loop over a fixed set of tasks,
// each with a machine-checkable completion condition, and aggregates
// outcome metrics (success rate, rounds, tool calls, token usage,
// duration) into a regression baseline.
//
// Two execution backends are supported:
//
//   - Live mode (--live): a real LLM provider is used (requires API
//     credentials in the environment). This is the only way to measure
//     true task-completion quality, but costs tokens and network.
//
//   - Mock mode (default): a scripted provider replays a pre-recorded
//     sequence of LLM responses per task. This runs fully offline,
//     costs nothing, and exercises the entire tool-dispatch / verify
//     pipeline so the harness itself can be developed and CI-tested
//     without burning API budget.
//
// The harness is intentionally minimal: one Go package, no external
// deps, no benchmark framework. Add tasks by appending to the Tasks
// slice (see tasks.go).
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go-code-agent/internal/hitlaudit"
	"go-code-agent/internal/security"
)

// Task is a single evaluation case.
type Task struct {
	// Name is a short stable identifier (used as the report key and
	// JSON field). Keep it slug-like and unique.
	Name string

	// Category groups tasks for subtotals (e.g. "api", "bugfix",
	// "refactor", "mcp", "edit").
	Category string

	// ExpectFail inverts pass/fail: the task passes when Verify returns
	// false. Used for negative cases that exercise the harness's
	// failure-recording path.
	ExpectFail bool

	// Setup runs before the agent loop. It should create any scaffold
	// (files, dirs, git repo) the task needs inside workdir and return
	// an optional human-readable description of the scenario.
	Setup func(workdir string) (desc string, err error)

	// Prompt is the user message handed to the agent.
	Prompt string

	// Verify inspects the post-run workdir and returns (ok, detail).
	// ok=false fails the task; detail explains why (shown in report).
	Verify func(workdir string) (ok bool, detail string)

	// Script is the scripted LLM response sequence used in mock mode.
	// Each ScriptStep either emits assistant text/tool-calls or (for
	// the terminal step) signals task completion. Ignored in live mode.
	Script []ScriptStep
}

// ScriptStep is one round of mock-LLM behaviour.
type ScriptStep struct {
	// Text is the assistant's free-form message for this round.
	Text string
	// ToolCalls is the set of tool invocations the mock assistant makes.
	// An empty slice with Done=true means "finished, no more tools".
	ToolCalls []MockToolCall
	// Done marks the terminal step: the assistant stops calling tools
	// and the loop should finalize.
	Done bool
}

// MockToolCall is a scripted tool invocation returned by the mock LLM.
type MockToolCall struct {
	Name string
	// Args is the raw JSON argument object, e.g. `{"path":"a.go"}`.
	Args string
}

// Result is the recorded outcome of running one task.
type Result struct {
	Name      string
	Category  string
	Scenario  string // Setup-returned description (aids failure triage)
	Success   bool
	Error     string // hard error (setup/provider/init/verify failure)
	SoftError string // non-fatal loop error (max-rounds/timeout)
	Rounds    int
	ToolCalls int
	PromptTok int64
	CompTok   int64
	TotalTok  int64
	Duration  time.Duration
	Detail    string // verify detail / error context

	// Failure triage fields (populated only on failure):
	LastAssistant string   `json:",omitempty"` // last assistant text (truncated)
	ChangedFiles  []string `json:",omitempty"` // workdir file list at verify time
}

// Harness orchestrates a task suite.
type Harness struct {
	Tasks   []Task
	Live    bool
	Model   string
	Timeout time.Duration
	Verbose bool // print per-task progress to stderr

	mu      sync.Mutex
	results []Result
}

// New builds a harness. live=false uses the scripted mock provider.
func New(tasks []Task, live bool, model string) *Harness {
	if model == "" {
		model = "mock"
	}
	return &Harness{Tasks: tasks, Live: live, Model: model, Timeout: 5 * time.Minute}
}

// Run executes all tasks, collecting results. In live mode each task
// spawns the real agent loop; in mock mode the scripted provider drives
// the loop offline.
//
// Global agent state (auto-approval, HITL mode) is saved on entry and
// restored on exit so the harness doesn't leak its non-interactive
// settings into the host process.
func (h *Harness) Run(ctx context.Context) []Result {
	prevAuto := security.GlobalApproval.IsAutoApproveAll()
	prevHitlOn := hitlaudit.HitlManager.IsEnabled()
	prevHitlMode := hitlaudit.HitlManager.Mode()
	defer func() {
		security.GlobalApproval.SetAutoApproveAll(prevAuto)
		hitlaudit.HitlManager.SetEnabled(prevHitlOn)
		hitlaudit.HitlManager.SetMode(prevHitlMode)
	}()

	for i, t := range h.Tasks {
		res := h.runOneSafe(ctx, t)
		h.mu.Lock()
		h.results = append(h.results, res)
		h.mu.Unlock()
		if h.Verbose {
			status := "PASS"
			if !res.Success {
				status = "FAIL"
			}
			fmt.Fprintf(os.Stderr, "[%d/%d] %-24s %s  (%d rounds, %d tok, %v)\n",
				i+1, len(h.Tasks), t.Name, status,
				res.Rounds, res.TotalTok, res.Duration.Round(time.Millisecond))
		}
	}
	return h.results
}

// runOneSafe wraps runOne with a panic guard so one bad task (bad
// Setup, nil Verify, etc.) doesn't abort the whole suite.
func (h *Harness) runOneSafe(ctx context.Context, t Task) (res Result) {
	defer func() {
		if r := recover(); r != nil {
			res = Result{
				Name:     t.Name,
				Category: t.Category,
				Error:    fmt.Sprintf("panic: %v", r),
				Detail:   fmt.Sprintf("panic: %v", r),
			}
		}
	}()
	return h.runOne(ctx, t)
}

func (h *Harness) runOne(ctx context.Context, t Task) Result {
	res := Result{Name: t.Name, Category: t.Category}
	workdir, err := os.MkdirTemp("", "eval-"+t.Name+"-")
	if err != nil {
		res.Error = fmt.Sprintf("mkdir workdir: %v", err)
		return res
	}
	defer os.RemoveAll(workdir)

	if t.Setup != nil {
		desc, serr := t.Setup(workdir)
		res.Scenario = desc
		if serr != nil {
			res.Error = fmt.Sprintf("setup: %v", serr)
			res.Detail = res.Error
			return res
		}
	}

	start := time.Now()
	err = h.execute(ctx, t, workdir, &res)
	res.Duration = time.Since(start)
	// Hard errors (provider init / session bootstrap) are fatal: skip
	// Verify. Soft errors (max-rounds / timeout) are recorded in
	// res.SoftError by execute and still allow Verify to grade.
	if err != nil {
		if res.Error == "" {
			res.Error = err.Error()
		}
		return res
	}

	// Verify always runs (even after a soft error) — the workdir may
	// be correct even if the loop hit max-rounds or timed out.
	ok, detail := t.Verify(workdir)
	if t.ExpectFail {
		ok = !ok
		if ok {
			detail = "expected failure observed"
		} else {
			detail = "expected failure but verify passed: " + detail
		}
	}
	res.Success = ok
	res.Detail = detail
	if !ok {
		if res.Error == "" {
			res.Error = "verify failed: " + detail
		}
		// Collect workdir contents for failure triage (before defer
		// RemoveAll deletes the temp dir).
		res.ChangedFiles = listFiles(workdir)
	} else if res.SoftError != "" {
		res.Detail = "passed (" + res.SoftError + ")"
	}
	return res
}

// Summary aggregates results into a printable/reportable baseline.
type Summary struct {
	Total        int
	Passed       int
	SuccessRate  float64
	AvgRounds    float64
	AvgToolCalls float64
	AvgPromptTok float64
	AvgCompTok   float64
	AvgTotalTok  float64
	P50DurMs     int64
	P95DurMs     int64
	ByCategory   map[string]CategoryStat
	Results      []Result
}

// CategoryStat holds per-category aggregates.
type CategoryStat struct {
	Total  int
	Passed int
	Rate   float64
}

// Summarize computes the baseline report from collected results.
func (h *Harness) Summarize() Summary {
	h.mu.Lock()
	rs := append([]Result(nil), h.results...)
	h.mu.Unlock()

	s := Summary{Results: rs, ByCategory: map[string]CategoryStat{}}
	if len(rs) == 0 {
		return s
	}
	var rounds, tools, pt, ct, tt int64
	var durs []int64
	for _, r := range rs {
		s.Total++
		if r.Success {
			s.Passed++
		}
		rounds += int64(r.Rounds)
		tools += int64(r.ToolCalls)
		pt += r.PromptTok
		ct += r.CompTok
		tt += r.TotalTok
		durs = append(durs, r.Duration.Milliseconds())

		c := s.ByCategory[r.Category]
		c.Total++
		if r.Success {
			c.Passed++
		}
		c.Rate = float64(c.Passed) / float64(c.Total)
		s.ByCategory[r.Category] = c
	}
	s.SuccessRate = float64(s.Passed) / float64(s.Total)
	s.AvgRounds = float64(rounds) / float64(s.Total)
	s.AvgToolCalls = float64(tools) / float64(s.Total)
	s.AvgPromptTok = float64(pt) / float64(s.Total)
	s.AvgCompTok = float64(ct) / float64(s.Total)
	s.AvgTotalTok = float64(tt) / float64(s.Total)
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	s.P50DurMs = percentile(durs, 50)
	s.P95DurMs = percentile(durs, 95)
	return s
}

// Report renders a human-readable baseline table.
func (s Summary) Report() string {
	out := ""
	out += fmt.Sprintf("=== eval baseline: %d tasks, %.0f%% success ===\n",
		s.Total, s.SuccessRate*100)
	out += fmt.Sprintf("avg rounds=%.1f  avg tool_calls=%.1f  avg tokens(p/c/t)=%.0f/%.0f/%.0f\n",
		s.AvgRounds, s.AvgToolCalls, s.AvgPromptTok, s.AvgCompTok, s.AvgTotalTok)
	out += fmt.Sprintf("duration P50=%dms P95=%dms\n", s.P50DurMs, s.P95DurMs)
	out += "--- by category ---\n"
	for cat, c := range s.ByCategory {
		out += fmt.Sprintf("  %-10s %d/%d (%.0f%%)\n", cat, c.Passed, c.Total, c.Rate*100)
	}
	out += "--- per task ---\n"
	for _, r := range s.Results {
		status := "PASS"
		if !r.Success {
			status = "FAIL"
		}
		out += fmt.Sprintf("  [%-4s] %-22s rounds=%-3d tools=%-3d tok=%-6d %s\n",
			status, r.Name, r.Rounds, r.ToolCalls, r.TotalTok, r.Detail)
		if !r.Success {
			if r.LastAssistant != "" {
				out += "          last assistant: " + truncate(r.LastAssistant, 200) + "\n"
			}
			if len(r.ChangedFiles) > 0 {
				out += "          changed files: " + strings.Join(r.ChangedFiles, ", ") + "\n"
			}
		}
	}
	return out
}

// WriteJSON dumps the baseline as JSON to path.
func (s Summary) WriteJSON(path string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// percentile returns the p-th percentile from a pre-sorted slice using
// the nearest-rank method. Adequate for small samples (n>=10); for
// very small n the result is approximate.
func percentile(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := (p * len(sorted)) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// listFiles walks dir and returns sorted relative paths of all regular
// files. Used to populate Result.ChangedFiles on failure.
func listFiles(dir string) []string {
	var files []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			rel = path
		}
		files = append(files, rel)
		return nil
	})
	sort.Strings(files)
	return files
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
