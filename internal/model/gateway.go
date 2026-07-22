// Package model provides the LLM gateway abstraction.
package model

import (
	"context"
	"go-code-agent/internal/llm"
)

// Provider is the interface every LLM backend implements.
type Provider interface {
	Name() string
	Call(ctx context.Context, params llm.CallParams) (*llm.Completion, error)
	Stream(ctx context.Context, params llm.CallParams, sink StreamSink) (*llm.StreamResult, error)
}

// StreamSink receives streaming text events.
type StreamSink interface {
	OnTextDelta(text string)
	OnDone()
}

// RateLimited is optionally implemented by provider-specific errors.
type RateLimited interface {
	RateLimited() bool
	RetryAfter() int // seconds
}

// Gateway is the unified model access point, holding the active provider.
type Gateway struct {
	provider     Provider
	judgeProv    Provider
	subagentProv Provider
	teamProv     Provider

	usageFn  UsageRecorder
	throttle *RoleThrottle
}

type UsageRecorder func(source, provider, model, traceID string, u llm.Usage, dur float64)

// NewGateway creates a Gateway with the main provider and per-role throttling.
func NewGateway(p Provider, throttle *RoleThrottle) *Gateway {
	return &Gateway{provider: p, throttle: throttle}
}

func (g *Gateway) SetJudgeProvider(p Provider)       { g.judgeProv = p }
func (g *Gateway) SetSubagentProvider(p Provider)    { g.subagentProv = p }
func (g *Gateway) SetTeamProvider(p Provider)        { g.teamProv = p }
func (g *Gateway) SetUsageRecorder(fn UsageRecorder) { g.usageFn = fn }

func (g *Gateway) providerFor(role string) Provider {
	switch role {
	case "judge":
		if g.judgeProv != nil {
			return g.judgeProv
		}
	case "subagent":
		if g.subagentProv != nil {
			return g.subagentProv
		}
	case "teammate":
		if g.teamProv != nil {
			return g.teamProv
		}
	}
	return g.provider
}

// Call executes a non-streaming LLM request with retries.
func (g *Gateway) Call(ctx context.Context, role string, params llm.CallParams) (*llm.Completion, error) {
	p := g.providerFor(role)
	return callWithRetry(ctx, p, params, g.throttle, g.usageFn, role)
}

// Stream executes a streaming LLM request with retries.
func (g *Gateway) Stream(ctx context.Context, role string, params llm.CallParams, sink StreamSink) (*llm.StreamResult, error) {
	p := g.providerFor(role)
	return streamWithRetry(ctx, p, params, sink, g.throttle, g.usageFn, role)
}
