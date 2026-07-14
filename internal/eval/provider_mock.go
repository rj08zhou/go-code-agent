package eval

import (
	"context"
	"fmt"
	"sync"

	"go-code-agent/internal/llm"
)

// ScriptedProvider is a deterministic, offline stand-in for a real LLM.
// It replays a fixed sequence of ScriptSteps per task, so the whole
// agent loop (tool dispatch, tool results fed back, finalize) runs
// without any network or API cost. It also records rounds and tool
// calls into the task Result.
//
// Behaviour contract (mirrors a real assistant):
//   - Each Stream() call consumes the next ScriptStep.
//   - If the step has ToolCalls, they are returned and the loop will
//     execute them, then call Stream() again (next step).
//   - If the step has Done=true (typically with no ToolCalls), the
//     assistant signals it is finished; loop.finalizeTurn ends the run.
//   - If the script is exhausted before Done, we return a terminal
//     empty message so the loop finalizes rather than panicking.
type ScriptedProvider struct {
	steps []ScriptStep
	res   *Result

	mu     sync.Mutex
	idx    int
	rounds int
	tools  int
}

// NewScriptedProvider builds a provider that replays steps, writing
// round/tool-call tallies into res.
func NewScriptedProvider(steps []ScriptStep, res *Result) *ScriptedProvider {
	return &ScriptedProvider{steps: steps, res: res}
}

func (p *ScriptedProvider) Name() string { return "eval-scripted" }

// Stream implements llm.Provider. The harness only needs non-streaming
// semantics; we accumulate into a StreamResult and report it.
func (p *ScriptedProvider) Stream(ctx context.Context, params llm.CallParams, sink llm.StreamSink) (*llm.StreamResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Count this as one model round.
	p.rounds++
	// approximate token usage from the request size so the baseline has
	// non-zero numbers even offline.
	approx := int64(llm.EstimateTokens(params.Messages))

	var step ScriptStep
	if p.idx < len(p.steps) {
		step = p.steps[p.idx]
		p.idx++
	} else {
		// Script exhausted: terminate gracefully.
		step = ScriptStep{Done: true}
	}

	tcs := make([]llm.ToolCall, 0, len(step.ToolCalls))
	for i, tc := range step.ToolCalls {
		p.tools++
		tcs = append(tcs, llm.ToolCall{
			ID:        fmt.Sprintf("eval-tc-%d", p.tools),
			Name:      tc.Name,
			Arguments: tc.Args,
		})
		_ = i
	}

	// Echo any streamed text to the sink (harmless no-op sink in tests).
	if sink != nil {
		if step.Text != "" {
			sink.OnTextDelta(step.Text)
		}
		sink.OnDone()
	}

	// Write tallies back to the result.
	p.res.Rounds = p.rounds
	p.res.ToolCalls = p.tools
	p.res.PromptTok += approx
	p.res.CompTok += int64(len(step.Text)/4 + len(tcs)*8)
	p.res.TotalTok = p.res.PromptTok + p.res.CompTok

	finish := "tool_calls"
	if step.Done || len(tcs) == 0 {
		finish = "stop"
	}
	return &llm.StreamResult{
		Content:      step.Text,
		ToolCalls:    tcs,
		FinishReason: finish,
		Usage: llm.Usage{
			PromptTokens:     approx,
			CompletionTokens: int64(len(step.Text)/4 + len(tcs)*8),
			TotalTokens:      approx + int64(len(step.Text)/4+len(tcs)*8),
		},
	}, nil
}

// Call is unused by the agent loop (it uses Stream); provide a minimal
// implementation to satisfy the interface.
func (p *ScriptedProvider) Call(ctx context.Context, params llm.CallParams) (*llm.Completion, error) {
	sr, err := p.Stream(ctx, params, nil)
	if err != nil {
		return nil, err
	}
	return &llm.Completion{
		Content:      sr.Content,
		ToolCalls:    sr.ToolCalls,
		FinishReason: sr.FinishReason,
		Usage:        sr.Usage,
	}, nil
}

var _ llm.Provider = (*ScriptedProvider)(nil)
