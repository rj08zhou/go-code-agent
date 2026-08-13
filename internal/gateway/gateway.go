// Package gateway provides the LLM gateway abstraction.
package gateway

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"go-code-agent/internal/event"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/logging"
	"strings"
)

// Provider is the interface every LLM backend implements.
type Provider interface {
	Name() string
	Capabilities() ProviderCapabilities
	Call(ctx context.Context, params llm.CallParams) (*llm.Completion, error)
	Stream(ctx context.Context, params llm.CallParams, sink StreamSink) (*llm.StreamResult, error)
}

// ProviderIdentity is optional. Providers that may have multiple configured
// endpoints should implement it so opaque reasoning state cannot cross from
// one compatible API instance to another with the same Name and model ID.
type ProviderIdentity interface {
	InstanceID() string
}

// ProviderInstanceID returns the non-secret identity used to bind opaque
// continuation state. Providers without an explicit identity return empty and
// therefore fail closed for native reasoning; a broad type name is not a safe
// substitute for endpoint identity.
func ProviderInstanceID(p Provider) string {
	if p == nil {
		return ""
	}
	if identified, ok := p.(ProviderIdentity); ok {
		return strings.TrimSpace(identified.InstanceID())
	}
	return ""
}

// StableProviderInstanceID derives a stable, non-secret identity from provider
// type and endpoint. The endpoint itself is never exposed or logged.
func StableProviderInstanceID(name, endpoint string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint == "" {
		endpoint = "<default>"
	}
	sum := sha256.Sum256([]byte(name + "\x00" + endpoint))
	return fmt.Sprintf("%s:%x", name, sum[:16])
}

// ProviderCapabilities declares what a provider supports.
type ProviderCapabilities struct {
	StructuredOutput bool // JSON Schema enforced response
	ToolCalling      bool // function/tool calls in the response
	Streaming        bool // server-sent events streaming
	Reasoning        bool // native reasoning (summary + opaque continuation)
}

// prepareParamsFor adapts params for a specific provider:
//   - if the provider lacks native reasoning, the opt-in is stripped (explicit,
//     logged degradation — never a silent pretend-success);
//   - reasoning continuation state is forwarded only when the provider supports
//     reasoning and its instance ID/model match. State is opaque: it must be
//     dropped, not transformed, so fallback or compatible endpoints never
//     receive foreign blobs.
func prepareParamsFor(p Provider, params llm.CallParams, role string) llm.CallParams {
	caps := p.Capabilities()
	instanceID := ProviderInstanceID(p)
	safeReasoning := caps.Reasoning && instanceID != ""
	if params.Reasoning != nil && !safeReasoning {
		if params.Reasoning.Enabled {
			logging.Default().Warn(fmt.Sprintf(
				"reasoning degraded: role=%s provider=%s lacks reasoning capability or instance identity", role, p.Name()))
		}
		params.Reasoning = nil
	}

	needsStrip := false
	for i := range params.Messages {
		r := params.Messages[i].Reasoning
		if r != nil && r.State != nil && (!safeReasoning || !r.State.Compatible(instanceID, params.Model)) {
			needsStrip = true
			break
		}
	}
	if !needsStrip {
		return params
	}
	msgs := make([]llm.Message, len(params.Messages))
	copy(msgs, params.Messages)
	for i := range msgs {
		r := msgs[i].Reasoning
		if r == nil || r.State == nil || (safeReasoning && r.State.Compatible(instanceID, params.Model)) {
			continue
		}
		stripped := *r
		stripped.State = nil
		msgs[i].Reasoning = &stripped
	}
	params.Messages = msgs
	return params
}

// StreamSink receives streaming text events.
//
// OnReasoningDelta is for displayable thinking/rationale streamed separately
// from the answer. Opaque continuation state must never be sent here.
type StreamSink interface {
	OnTextDelta(text string)
	OnReasoningDelta(text string)
	OnDone()
}

// trackingSink wraps a StreamSink to track whether answer content was emitted
// to the user. It intercepts OnDone from providers (which call it in finalize)
// so that the Gateway — not the provider — controls when OnDone reaches the
// user. This prevents duplicate OnDone across retries and fallbacks.
//
// Reasoning deltas are forwarded but do not set emitted: a thinking-only
// stream may still need the no-content text fallback, and a display-gated
// sink may drop reasoning without the user having seen answer text.
type trackingSink struct {
	inner   StreamSink
	emitted bool
}

func newTrackingSink(inner StreamSink) *trackingSink {
	return &trackingSink{inner: inner}
}

func (s *trackingSink) OnTextDelta(text string) {
	if text != "" {
		s.emitted = true
	}
	s.inner.OnTextDelta(text)
}

func (s *trackingSink) OnReasoningDelta(text string) {
	s.inner.OnReasoningDelta(text)
}

// OnDone is intercepted: providers call it in finalize, but the Gateway
// decides when to forward it to the user via notifyDone.
func (s *trackingSink) OnDone() {}

func (s *trackingSink) notifyDone() {
	s.inner.OnDone()
}

func (s *trackingSink) reset() {
	s.emitted = false
}

// RateLimited is optionally implemented by provider-specific errors.
type RateLimited interface {
	RateLimited() bool
	RetryAfter() int // seconds
}

// Gateway coordinates provider selection, retries, throttling, and fallback.
// It is process-scoped and must not store session-owned observers; those travel
// via CallObservers on the request context.
type Gateway struct {
	provider     Provider
	judgeProv    Provider
	subagentProv Provider
	teamProv     Provider

	// fallbacks maps role → ordered fallback chain (tried after the primary).
	fallbacks map[string][]Provider

	throttle *RoleThrottle
	retry    retryPolicy
}

type UsageRecorder func(source, provider, model, traceID string, u llm.Usage, dur float64)

// CallObservers are request-scoped observers for model calls. They are carried
// by context instead of stored on Gateway because Gateway lives for the whole
// process while event logs and usage files belong to one session.
type CallObservers struct {
	Usage  UsageRecorder
	Events event.Sink
}

type callObserversKey struct{}

// WithCallObservers attaches request-scoped model observers to ctx.
func WithCallObservers(ctx context.Context, observers CallObservers) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, callObserversKey{}, observers)
}

func callObserversFromContext(ctx context.Context) CallObservers {
	if ctx == nil {
		return CallObservers{}
	}
	observers, _ := ctx.Value(callObserversKey{}).(CallObservers)
	return observers
}

// NewGateway creates a Gateway with the main provider and per-role throttling.
func NewGateway(p Provider, throttle *RoleThrottle) *Gateway {
	return &Gateway{
		provider:  p,
		throttle:  throttle,
		fallbacks: make(map[string][]Provider),
		retry:     defaultRetryPolicy(),
	}
}

func (g *Gateway) SetJudgeProvider(p Provider)    { g.judgeProv = p }
func (g *Gateway) SetSubagentProvider(p Provider) { g.subagentProv = p }
func (g *Gateway) SetTeamProvider(p Provider)     { g.teamProv = p }

// ProviderName returns the provider selected as the primary route for a role.
func (g *Gateway) ProviderName(role string) string {
	if g == nil {
		return ""
	}
	p := g.providerFor(role)
	if p == nil {
		return ""
	}
	return p.Name()
}

// ReasoningAvailable reports whether the role's primary provider can safely
// receive native reasoning requests and endpoint-bound continuation state.
func (g *Gateway) ReasoningAvailable(role string) bool {
	if g == nil {
		return false
	}
	p := g.providerFor(role)
	return p != nil && p.Capabilities().Reasoning && ProviderInstanceID(p) != ""
}

// SetFallbacks registers an ordered fallback chain for a role. The primary
// provider is tried first; on a permanent failure the Gateway switches to the
// next fallback that satisfies the request's capability requirements.
func (g *Gateway) SetFallbacks(role string, providers ...Provider) {
	g.fallbacks[role] = providers
}

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

// providersFor returns the primary provider followed by its fallback chain.
func (g *Gateway) providersFor(role string) []Provider {
	primary := g.providerFor(role)
	chain := []Provider{primary}
	chain = append(chain, g.fallbacks[role]...)
	return chain
}

// ErrStructuredOutputUnsupported is returned when no available provider for the
// role supports the requested structured-output contract.
var ErrStructuredOutputUnsupported = fmt.Errorf("no provider available that supports structured output")

// pickProvider selects the first provider in chain whose capabilities satisfy
// the request. It returns the provider and the index (for event reporting).
func pickProvider(chain []Provider, params llm.CallParams) (Provider, int, error) {
	needStructured := params.StructuredOutput != nil
	for i, p := range chain {
		if p == nil {
			continue
		}
		caps := p.Capabilities()
		if needStructured && !caps.StructuredOutput {
			continue
		}
		return p, i, nil
	}
	return nil, 0, ErrStructuredOutputUnsupported
}

// shouldFallback reports whether trying another provider is useful. Immediate
// permanent failures and transient failures that exhausted retries qualify;
// parent-context cancellation never does.
func shouldFallback(err error) bool {
	var exhausted *RetryExhaustedError
	if errors.As(err, &exhausted) {
		return true
	}
	return ClassifyError(err) == ErrorPermanent
}

func nextEligibleProvider(chain []Provider, start int, params llm.CallParams) (Provider, int, bool) {
	if start >= len(chain) {
		return nil, 0, false
	}
	provider, offset, err := pickProvider(chain[start:], params)
	if err != nil {
		return nil, 0, false
	}
	return provider, start + offset, true
}

func logProviderFallback(ctx context.Context, eventSink event.Sink, role string, from, to Provider, err error) {
	reason := "permanent_error"
	var exhausted *RetryExhaustedError
	if errors.As(err, &exhausted) {
		reason = "retries_exhausted"
	}
	summary := fmt.Sprintf("provider fallback: role=%s %s → %s (%s)", role, from.Name(), to.Name(), reason)
	if eventSink == nil {
		logging.Default().Warn(summary)
		return
	}
	eventSink.Emit(event.Event{
		Type:    event.ModelRetry,
		TraceID: GetTraceID(ctx),
		Payload: map[string]string{"summary": summary},
	})
}

// Call executes a non-streaming LLM request with retries and fallback.
func (g *Gateway) Call(ctx context.Context, role string, params llm.CallParams) (*llm.Completion, error) {
	chain := g.providersFor(role)
	provider, idx, err := pickProvider(chain, params)
	if err != nil {
		return nil, err
	}
	observers := callObserversFromContext(ctx)

	for {
		resp, callErr := callWithRetry(ctx, provider, prepareParamsFor(provider, params, role), g.throttle, observers.Usage, observers.Events, role, g.retry)
		if callErr == nil {
			return resp, nil
		}
		if !shouldFallback(callErr) {
			return nil, callErr
		}
		next, nextIdx, ok := nextEligibleProvider(chain, idx+1, params)
		if !ok {
			return nil, callErr
		}
		logProviderFallback(ctx, observers.Events, role, provider, next, callErr)
		provider, idx = next, nextIdx
	}
}

// Stream executes a streaming LLM request with retries and fallback.
func (g *Gateway) Stream(ctx context.Context, role string, params llm.CallParams, sink StreamSink) (*llm.StreamResult, error) {
	chain := g.providersFor(role)
	provider, idx, err := pickProvider(chain, params)
	if err != nil {
		return nil, err
	}
	observers := callObserversFromContext(ctx)

	ts := newTrackingSink(sink)
	for {
		sr, callErr := streamWithRetry(ctx, provider, prepareParamsFor(provider, params, role), ts, g.throttle, observers.Usage, observers.Events, role, g.retry)
		if callErr == nil {
			ts.notifyDone()
			return sr, nil
		}
		if ts.emitted {
			// Content was already shown to the user; can't fall back.
			ts.notifyDone()
			return sr, callErr
		}
		if !shouldFallback(callErr) {
			return nil, callErr
		}
		next, nextIdx, ok := nextEligibleProvider(chain, idx+1, params)
		if !ok {
			return nil, callErr
		}
		logProviderFallback(ctx, observers.Events, role, provider, next, callErr)
		provider, idx = next, nextIdx
		ts.reset()
	}
}
