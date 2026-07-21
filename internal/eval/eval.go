// Package eval provides a regression testing harness for the agent.
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent-refactor/internal/agent"
	"go-code-agent-refactor/internal/application"
	"go-code-agent-refactor/internal/config"
	"go-code-agent-refactor/internal/llm"
	"go-code-agent-refactor/internal/model"
	"go-code-agent-refactor/internal/tool"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ScriptStep is one turn in a mock LLM script.
type ScriptStep struct {
	Text      string         `json:"text,omitempty"`
	ToolCalls []MockToolCall `json:"tool_calls,omitempty"`
	Done      bool           `json:"done,omitempty"`
}

// MockToolCall describes a single tool invocation in a mock turn.
type MockToolCall struct {
	Name string `json:"name"`
	Args string `json:"args"`
}

// Task is one regression test case.
type Task struct {
	Name       string `json:"name"`
	Category   string `json:"category"`
	ExpectFail bool   `json:"expect_fail,omitempty"`
	Setup      func(workdir string) (desc string, err error)
	Prompt     string `json:"prompt"`
	Verify     func(workdir string) (ok bool, detail string)
	Script     []ScriptStep `json:"script,omitempty"`
}

// Result holds the outcome of a single task run.
type Result struct {
	Name       string        `json:"name"`
	Category   string        `json:"category"`
	Success    bool          `json:"success"`
	ExpectFail bool          `json:"expect_fail"`
	Error      string        `json:"error,omitempty"`
	Rounds     int           `json:"rounds"`
	ToolCalls  int           `json:"tool_calls"`
	PromptTok  int64         `json:"prompt_tokens"`
	CompTok    int64         `json:"completion_tokens"`
	TotalTok   int64         `json:"total_tokens"`
	Duration   time.Duration `json:"duration_ms"`
	Detail     string        `json:"detail,omitempty"`
}

// Harness orchestrates task execution and aggregates results.
type Harness struct {
	Tasks   []Task
	Live    bool
	Model   string
	Timeout time.Duration
	Verbose bool
}

// Summary aggregates Harness results.
type Summary struct {
	Total        int                     `json:"total"`
	Passed       int                     `json:"passed"`
	SuccessRate  float64                 `json:"success_rate"`
	AvgRounds    float64                 `json:"avg_rounds"`
	AvgToolCalls float64                 `json:"avg_tool_calls"`
	AvgPromptTok float64                 `json:"avg_prompt_tokens"`
	AvgCompTok   float64                 `json:"avg_completion_tokens"`
	ByCategory   map[string]CategoryStat `json:"by_category"`
	Results      []Result                `json:"results"`
}

type CategoryStat struct {
	Total  int `json:"total"`
	Passed int `json:"passed"`
}

// Run executes all tasks and returns a summary.
func (h *Harness) Run(ctx context.Context) Summary {
	var results []Result
	for _, t := range h.Tasks {
		r := h.runOne(ctx, t)
		results = append(results, r)
	}
	return h.buildSummary(results)
}

func (h *Harness) runOne(ctx context.Context, t Task) (r Result) {
	defer func() {
		if rec := recover(); rec != nil {
			r.Error = fmt.Sprintf("panic: %v", rec)
		}
	}()
	r.Name = t.Name
	r.Category = t.Category
	r.ExpectFail = t.ExpectFail

	return h.runOneSafe(ctx, t)
}

func (h *Harness) runOneSafe(ctx context.Context, t Task) Result {
	r := Result{Name: t.Name, Category: t.Category, ExpectFail: t.ExpectFail}

	workdir, err := os.MkdirTemp("", "eval-"+t.Name+"-*")
	if err != nil {
		r.Error = fmt.Sprintf("tempdir: %v", err)
		return r
	}
	defer os.RemoveAll(workdir)

	// Run setup
	var setupDesc string
	if t.Setup != nil {
		var setupErr error
		setupDesc, setupErr = t.Setup(workdir)
		if setupErr != nil {
			r.Error = fmt.Sprintf("setup: %v", setupErr)
			return r
		}
	}

	started := time.Now()

	// Mock mode: use ScriptExecutor
	if len(t.Script) > 0 {
		r = h.runMocked(ctx, t, workdir)
	} else if h.Live {
		r = h.runLive(ctx, t, workdir)
	} else {
		r.Error = "no script and live=false"
	}

	r.Duration = time.Since(started)

	// Run verify
	if t.Verify != nil && r.Error == "" {
		ok, detail := t.Verify(workdir)
		if t.ExpectFail {
			if ok {
				r.Error = fmt.Sprintf("expected failure but passed: %s", detail)
			} else {
				r.Success = true
			}
		} else {
			r.Success = ok
			if !ok {
				r.Error = fmt.Sprintf("verify: %s", detail)
			}
		}
	} else if r.Error == "" {
		// No Verify step and no error → treat as success
		r.Success = true
	}

	_ = setupDesc
	return r
}

func (h *Harness) runMocked(ctx context.Context, t Task, workdir string) Result {
	r := Result{Name: t.Name, Category: t.Category, ExpectFail: t.ExpectFail}

	// Build scripted provider that replays pre-defined model responses
	sp := &scriptedProvider{script: t.Script}
	gw := model.NewGateway(sp, model.NewRoleThrottle(10))

	// Build tool catalog with basic builtin tools
	catalog := tool.NewToolCatalog()
	catalog.RegisterAll(evalBuiltinTools(workdir))

	exec := tool.NewExecutor(catalog, nil, nil)
	scope := &tool.ToolScope{
		Role:       "lead",
		Workdir:    workdir,
		SessionID:  "eval-" + t.Name,
		AgentID:    "eval-lead",
		CanRead:    true,
		CanWrite:   true,
		CanExecute: true,
	}

	profile := agent.NewLeadProfile("You are a coding agent under eval. Follow instructions precisely.")
	profile.MaxRounds = 30 // shorter for eval
	runner := agent.NewRunner(profile, gw, exec, scope)

	outcome := runner.Run(ctx, []llm.Message{llm.UserMessage(t.Prompt)}, "eval-"+t.Name)
	r.Rounds = outcome.Rounds
	r.ToolCalls = len(outcome.ToolResults)

	if outcome.Error != nil {
		r.Error = outcome.Error.Error()
		return r
	}
	// If script ran to completion without explicit errors, treat as success candidate
	// The Verify step will make the final pass/fail decision
	return r
}

func (h *Harness) runLive(ctx context.Context, t Task, workdir string) Result {
	r := Result{Name: t.Name, Category: t.Category, ExpectFail: t.ExpectFail}

	cfgDir := os.Getenv("HOME") + "/.config"
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		cfgDir = d
	}

	// Initialize application with real config and provider
	config.SetConfig(config.Load())
	app, err := application.New(cfgDir, workdir)
	if err != nil {
		r.Error = fmt.Sprintf("init app: %v", err)
		return r
	}
	defer app.Shutdown(ctx)

	gw := app.Gateway()
	if gw == nil {
		r.Error = "no LLM gateway available (set OPENAI_API_KEY or ANTHROPIC_API_KEY)"
		return r
	}

	catalog := tool.NewToolCatalog()
	catalog.RegisterAll(evalBuiltinTools(workdir))
	exec := tool.NewExecutor(catalog, nil, nil)
	scope := &tool.ToolScope{
		Role:       "lead",
		Workdir:    workdir,
		SessionID:  "eval-live-" + t.Name,
		AgentID:    "eval-lead",
		CanRead:    true,
		CanWrite:   true,
		CanExecute: true,
		CanNetwork: true,
	}

	if h.Model != "" {
		cfg := config.CurrentConfig()
		cfg.ModelID = h.Model
		config.SetConfig(cfg)
	}

	profile := agent.NewLeadProfile("You are a coding agent under eval. Follow instructions precisely.")
	profile.MaxRounds = 30
	runner := agent.NewRunner(profile, gw, exec, scope)
	outcome := runner.Run(ctx, []llm.Message{llm.UserMessage(t.Prompt)}, "eval-live-"+t.Name)
	r.Rounds = outcome.Rounds
	r.ToolCalls = len(outcome.ToolResults)

	if outcome.Error != nil {
		r.Error = outcome.Error.Error()
	}

	return r
}

// scriptedProvider replays a pre-defined script of LLM responses for eval.
type scriptedProvider struct {
	script []ScriptStep
	idx    int
}

func (s *scriptedProvider) Name() string { return "scripted" }

func (s *scriptedProvider) Call(ctx context.Context, params llm.CallParams) (*llm.Completion, error) {
	return s.nextResponse(), nil
}

func (s *scriptedProvider) Stream(ctx context.Context, params llm.CallParams, sink model.StreamSink) (*llm.StreamResult, error) {
	resp := s.nextResponse()
	if sink != nil {
		sink.OnTextDelta(resp.Content)
		sink.OnDone()
	}
	return &llm.StreamResult{
		Content:      resp.Content,
		ToolCalls:    resp.ToolCalls,
		FinishReason: resp.FinishReason,
	}, nil
}

func (s *scriptedProvider) nextResponse() *llm.Completion {
	if s.idx >= len(s.script) {
		return &llm.Completion{FinishReason: "stop"}
	}
	step := s.script[s.idx]
	s.idx++

	tcs := make([]llm.ToolCall, 0, len(step.ToolCalls))
	for i, tc := range step.ToolCalls {
		tcs = append(tcs, llm.ToolCall{
			ID:        fmt.Sprintf("call_%d_%d", s.idx, i),
			Name:      tc.Name,
			Arguments: tc.Args,
		})
	}

	finishReason := "stop"
	if len(tcs) > 0 {
		finishReason = "tool_calls"
	}
	if step.Done {
		finishReason = "stop"
	}

	return &llm.Completion{
		Content:      step.Text,
		ToolCalls:    tcs,
		FinishReason: finishReason,
	}
}

// evalBuiltinTools returns a minimal tool set for eval testing.
// Tools are real (not mocked) so that capability/approval checks are exercised.
func evalBuiltinTools(workdir string) []tool.ToolDefinition {
	return []tool.ToolDefinition{
		{
			Name:        "bash",
			Description: "Execute a shell command.",
			RiskLevel:   tool.RiskDanger,
			Effects:     tool.Effects(tool.EffectExecuteProcess),
			Handler: func(scope *tool.ToolScope, args json.RawMessage) tool.Result {
				var a struct {
					Command string `json:"command"`
				}
				json.Unmarshal(args, &a)
				if a.Command == "" {
					return tool.Failed("command is required")
				}
				cmd := exec.CommandContext(context.Background(), "sh", "-c", a.Command)
				cmd.Dir = scope.Workdir
				out, err := cmd.CombinedOutput()
				if err != nil {
					return tool.Succeeded(fmt.Sprintf("exit %d\n%s", cmd.ProcessState.ExitCode(), string(out)))
				}
				return tool.Succeeded(string(out))
			},
		},
		{
			Name:        "write_file",
			Description: "Write content to a file.",
			RiskLevel:   tool.RiskSafe,
			Effects:     tool.Effects(tool.EffectWriteFile),
			Handler: func(scope *tool.ToolScope, args json.RawMessage) tool.Result {
				var a struct {
					Path    string `json:"path"`
					Content string `json:"content"`
				}
				json.Unmarshal(args, &a)
				os.MkdirAll(filepath.Dir(filepath.Join(scope.Workdir, a.Path)), 0755)
				os.WriteFile(filepath.Join(scope.Workdir, a.Path), []byte(a.Content), 0644)
				return tool.Succeeded(fmt.Sprintf("Wrote %s", a.Path))
			},
		},
		{
			Name:        "read_file",
			Description: "Read file contents.",
			RiskLevel:   tool.RiskAuto,
			Effects:     tool.Effects(tool.EffectReadFile),
			Handler: func(scope *tool.ToolScope, args json.RawMessage) tool.Result {
				var a struct {
					Path string `json:"path"`
				}
				json.Unmarshal(args, &a)
				data, err := os.ReadFile(filepath.Join(scope.Workdir, a.Path))
				if err != nil {
					return tool.Failed(fmt.Sprintf("%v", err))
				}
				return tool.Succeeded(string(data))
			},
		},
	}
}

func (h *Harness) buildSummary(results []Result) Summary {
	s := Summary{Total: len(results)}
	byCat := map[string]CategoryStat{}
	totalRounds, totalTC := 0, 0
	totalPTok, totalCTok := int64(0), int64(0)

	var durs []time.Duration
	for _, r := range results {
		isPass := r.Success
		if r.ExpectFail {
			isPass = r.Error == "" || r.Success
		}
		if isPass {
			s.Passed++
		}
		totalRounds += r.Rounds
		totalTC += r.ToolCalls
		totalPTok += r.PromptTok
		totalCTok += r.CompTok
		if r.Duration > 0 {
			durs = append(durs, r.Duration)
		}
		cs := byCat[r.Category]
		cs.Total++
		if isPass {
			cs.Passed++
		}
		byCat[r.Category] = cs
	}

	if s.Total > 0 {
		s.SuccessRate = float64(s.Passed) / float64(s.Total) * 100
		s.AvgRounds = float64(totalRounds) / float64(s.Total)
		s.AvgToolCalls = float64(totalTC) / float64(s.Total)
		s.AvgPromptTok = float64(totalPTok) / float64(s.Total)
		s.AvgCompTok = float64(totalCTok) / float64(s.Total)
	}
	s.ByCategory = byCat
	s.Results = results

	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	if len(durs) > 0 {
		_ = durs // use percentile helper if needed
	}

	return s
}

// Report generates a human-readable summary.
func (s *Summary) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== Eval Results ===\n")
	fmt.Fprintf(&b, "Total: %d  Passed: %d  Rate: %.1f%%\n\n", s.Total, s.Passed, s.SuccessRate)
	fmt.Fprintf(&b, "Avg rounds: %.1f  Avg tool calls: %.1f\n\n", s.AvgRounds, s.AvgToolCalls)

	for cat, cs := range s.ByCategory {
		fmt.Fprintf(&b, "  %-12s %d/%d\n", cat+":", cs.Passed, cs.Total)
	}

	fmt.Fprintln(&b, "\n--- Details ---")
	for _, r := range s.Results {
		mark := "PASS"
		if !r.Success {
			mark = "FAIL"
		}
		if r.ExpectFail {
			mark += " (neg)"
		}
		fmt.Fprintf(&b, "  %s  %-30s", mark, r.Name)
		if r.Error != "" {
			fmt.Fprintf(&b, "  %s", r.Error)
		}
		fmt.Fprintln(&b)
	}
	return b.String()
}

// WriteJSON writes results to a JSON file.
func (s *Summary) WriteJSON(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

var _ = sort.Ints
