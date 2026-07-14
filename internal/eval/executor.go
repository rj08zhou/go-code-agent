package eval

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"go-code-agent/internal/agent"
	"go-code-agent/internal/hitlaudit"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/security"
)

// executor wires the harness to a real or mock agent run for one task.
//
// It builds a throwaway AppContext rooted at workdir, enables
// auto-approval (so the run is non-interactive), installs the provider
// (mock or live), runs the agent loop to completion, and records
// round/tool-call counts plus token usage into res.

// mockModelID is the arbitrary model id used by the mock provider so
// that loop.Run's logging and usage attribution stay sane.
const mockModelID = "mock-scripted"

// liveCallCounter counts LLM calls in live mode (one call == one round
// from the agent's perspective).
var liveCallCounter atomic.Int64

// countingProvider wraps a real llm.Provider: it increments the live
// call counter on every Call/Stream (so the harness can recover round
// counts in live mode) and accumulates real token usage into res.
type countingProvider struct {
	inner llm.Provider
	res   *Result
	mu    sync.Mutex
}

func (c *countingProvider) Name() string { return c.inner.Name() }
func (c *countingProvider) Call(ctx context.Context, p llm.CallParams) (*llm.Completion, error) {
	liveCallCounter.Add(1)
	comp, err := c.inner.Call(ctx, p)
	if err == nil && comp != nil {
		c.recordUsage(comp.Usage)
	}
	return comp, err
}
func (c *countingProvider) Stream(ctx context.Context, p llm.CallParams, sink llm.StreamSink) (*llm.StreamResult, error) {
	liveCallCounter.Add(1)
	sr, err := c.inner.Stream(ctx, p, sink)
	if err == nil && sr != nil {
		c.recordUsage(sr.Usage)
	}
	return sr, err
}
func (c *countingProvider) recordUsage(u llm.Usage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.res.PromptTok += u.PromptTokens
	c.res.CompTok += u.CompletionTokens
	c.res.TotalTok = c.res.PromptTok + c.res.CompTok
}

var _ llm.Provider = (*countingProvider)(nil)

// execute runs one task and fills res in place. A non-nil return is a
// hard error (provider init / session bootstrap) — runOne will skip
// Verify. Loop-level errors (max-rounds / timeout) are soft: they go
// into res.SoftError and Verify still runs.
func (h *Harness) execute(ctx context.Context, t Task, workdir string, res *Result) error {
	// --- choose / install provider -----------------------------------
	var modelID string
	if !h.Live {
		prov := NewScriptedProvider(t.Script, res)
		llm.SetProvider(prov)
		modelID = mockModelID
	} else {
		prov, err := llm.PickProvider(h.Model)
		if err != nil {
			return fmt.Errorf("pick provider for %q: %w", h.Model, err)
		}
		llm.SetProvider(&countingProvider{inner: prov, res: res})
		modelID = h.Model
	}

	// --- build a fresh App rooted at this task's workdir -------------
	agent.App = agent.NewApp(modelID, workdir, workdir, security.DefaultBashPolicy.Validate)
	// Reset approval gates so the run never blocks on a human.
	security.GlobalApproval.SetAutoApproveAll(true)
	hitlaudit.HitlManager.SetEnabled(true)
	hitlaudit.HitlManager.SetMode(hitlaudit.HITLModeAutoApprove)

	// --- bootstrap a throwaway session (written under workdir) -------
	sess, err := agent.App.SessionManager.BootstrapOrCreate(true, "")
	if err != nil {
		return fmt.Errorf("bootstrap session: %w", err)
	}
	agent.App.ActivateSession(sess)

	// --- init tools (registers edit_file, insert_file, bash, mcp...) -
	agent.InitTools()

	// --- run the loop --------------------------------------------------
	messages := []llm.Message{llm.UserMessage(t.Prompt)}
	runCtx, cancel := context.WithTimeout(ctx, h.Timeout)
	defer cancel()

	liveCallCounter.Store(0)
	err = agent.Run(runCtx, &messages)
	if err != nil {
		// max-rounds / ctx timeout / loop error — soft: Verify can
		// still grade the workdir.
		res.SoftError = err.Error()
	}
	// Capture last assistant text for failure triage.
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == llm.RoleAssistant && messages[i].Content != "" {
			res.LastAssistant = messages[i].Content
			break
		}
	}
	// In live mode, infer rounds from the number of LLM calls made.
	if h.Live {
		res.Rounds = int(liveCallCounter.Load())
	}
	return nil
}
