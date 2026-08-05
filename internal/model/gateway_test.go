package model

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go-code-agent/internal/event"
	"go-code-agent/internal/llm"
)

// --- fake providers for fallback tests ---

type capsProvider struct {
	name       string
	instanceID string
	caps       ProviderCapabilities
	err        error
	callCount  int
	callFn     func(context.Context, llm.CallParams) (*llm.Completion, error)
	streamFn   func(context.Context, llm.CallParams, StreamSink) (*llm.StreamResult, error)
}

func (p *capsProvider) Name() string { return p.name }
func (p *capsProvider) InstanceID() string {
	if p.instanceID != "" {
		return p.instanceID
	}
	return p.name
}
func (p *capsProvider) Capabilities() ProviderCapabilities { return p.caps }
func (p *capsProvider) Call(ctx context.Context, params llm.CallParams) (*llm.Completion, error) {
	p.callCount++
	if p.callFn != nil {
		return p.callFn(ctx, params)
	}
	if p.err != nil {
		return nil, p.err
	}
	return &llm.Completion{Content: "ok", FinishReason: "stop"}, nil
}
func (p *capsProvider) Stream(ctx context.Context, params llm.CallParams, sink StreamSink) (*llm.StreamResult, error) {
	p.callCount++
	if p.streamFn != nil {
		return p.streamFn(ctx, params, sink)
	}
	if p.err != nil {
		return nil, p.err
	}
	sink.OnTextDelta("ok")
	sink.OnDone()
	return &llm.StreamResult{Content: "ok", FinishReason: "stop"}, nil
}

func fastRetryPolicy(maxRetries int, callTimeout time.Duration) retryPolicy {
	return retryPolicy{
		maxRetries:  maxRetries,
		callTimeout: callTimeout,
		delay:       func(int, bool) time.Duration { return 0 },
	}
}

func TestPickProvider_SkipsCapabilityMismatch(t *testing.T) {
	chain := []Provider{
		&capsProvider{name: "gemini", caps: ProviderCapabilities{StructuredOutput: false}},
		&capsProvider{name: "openai", caps: ProviderCapabilities{StructuredOutput: true}},
	}
	picked, idx, err := pickProvider(chain, llm.CallParams{
		StructuredOutput: &llm.StructuredOutput{Name: "test"},
	})
	if err != nil {
		t.Fatalf("expected a provider, got error: %v", err)
	}
	if picked.Name() != "openai" {
		t.Fatalf("picked %s, want openai", picked.Name())
	}
	if idx != 1 {
		t.Fatalf("idx = %d, want 1", idx)
	}
}

func TestPickProvider_NoneSupports(t *testing.T) {
	chain := []Provider{
		&capsProvider{name: "gemini", caps: ProviderCapabilities{StructuredOutput: false}},
	}
	_, _, err := pickProvider(chain, llm.CallParams{
		StructuredOutput: &llm.StructuredOutput{Name: "test"},
	})
	if !errors.Is(err, ErrStructuredOutputUnsupported) {
		t.Fatalf("expected ErrStructuredOutputUnsupported, got %v", err)
	}
}

func TestGateway_CallFallbackOnPermanentError(t *testing.T) {
	primary := &capsProvider{
		name: "primary",
		caps: ProviderCapabilities{StructuredOutput: true, ToolCalling: true, Streaming: true},
		err: NewProviderError(
			"primary", 401, "invalid_api_key", errors.New("authentication failed")),
	}
	fallback := &capsProvider{
		name: "fallback",
		caps: ProviderCapabilities{StructuredOutput: true, ToolCalling: true, Streaming: true},
	}
	gw := NewGateway(primary, NewRoleThrottle(4))
	gw.SetFallbacks("lead", fallback)

	resp, err := gw.Call(context.Background(), "lead", llm.CallParams{Model: "test"})
	if err != nil {
		t.Fatalf("expected fallback to succeed, got: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("content = %q", resp.Content)
	}
}

func TestGateway_StreamFallbackOnPermanentError(t *testing.T) {
	primary := &capsProvider{
		name: "primary",
		caps: ProviderCapabilities{StructuredOutput: true, ToolCalling: true, Streaming: true},
		err:  errors.New("invalid_api_key"),
	}
	fallback := &capsProvider{
		name: "fallback",
		caps: ProviderCapabilities{StructuredOutput: true, ToolCalling: true, Streaming: true},
	}
	gw := NewGateway(primary, NewRoleThrottle(4))
	gw.SetFallbacks("lead", fallback)

	sink := &collectTestSink{}
	sr, err := gw.Stream(context.Background(), "lead", llm.CallParams{Model: "test"}, sink)
	if err != nil {
		t.Fatalf("expected fallback to succeed, got: %v", err)
	}
	if sr.Content != "ok" {
		t.Fatalf("content = %q", sr.Content)
	}
}

func TestGateway_NoFallbackReturnsOriginalError(t *testing.T) {
	primary := &capsProvider{
		name: "primary",
		caps: ProviderCapabilities{StructuredOutput: true, ToolCalling: true, Streaming: true},
		err:  errors.New("401 unauthorized"),
	}
	gw := NewGateway(primary, NewRoleThrottle(4))

	_, err := gw.Call(context.Background(), "lead", llm.CallParams{Model: "test"})
	if err == nil {
		t.Fatal("expected error with no fallback")
	}
}

func TestGatewayInvalidCallResponsesFallback(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(context.Context, llm.CallParams) (*llm.Completion, error)
	}{
		{name: "nil", call: func(context.Context, llm.CallParams) (*llm.Completion, error) {
			return nil, nil
		}},
		{name: "empty", call: func(context.Context, llm.CallParams) (*llm.Completion, error) {
			return &llm.Completion{FinishReason: "stop"}, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			primary := &capsProvider{name: "primary", callFn: tc.call}
			fallback := &capsProvider{name: "fallback"}
			gateway := NewGateway(primary, NewRoleThrottle(4))
			gateway.SetFallbacks("lead", fallback)

			response, err := gateway.Call(context.Background(), "lead", llm.CallParams{Model: "test"})
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			if response == nil || response.Content != "ok" {
				t.Fatalf("fallback response = %#v", response)
			}
			if primary.callCount != 1 || fallback.callCount != 1 {
				t.Fatalf("call counts primary=%d fallback=%d, want 1/1", primary.callCount, fallback.callCount)
			}
		})
	}
}

func TestGateway_TransientErrorFallsBackAfterRetriesExhausted(t *testing.T) {
	primary := &capsProvider{
		name: "primary",
		caps: ProviderCapabilities{StructuredOutput: true, ToolCalling: true, Streaming: true},
		err: NewProviderError(
			"primary", 503, "server_error", errors.New("invalid upstream response")),
	}
	fallback := &capsProvider{
		name: "fallback",
		caps: ProviderCapabilities{StructuredOutput: true, ToolCalling: true, Streaming: true},
	}
	gw := NewGateway(primary, NewRoleThrottle(4))
	gw.retry = fastRetryPolicy(2, time.Second)
	gw.SetFallbacks("lead", fallback)

	resp, err := gw.Call(context.Background(), "lead", llm.CallParams{Model: "test"})
	if err != nil {
		t.Fatalf("expected fallback success, got %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("content = %q", resp.Content)
	}
	if primary.callCount != 3 {
		t.Fatalf("primary callCount = %d, want 3", primary.callCount)
	}
	if fallback.callCount != 1 {
		t.Fatalf("fallback callCount = %d, want 1", fallback.callCount)
	}
}

func TestGateway_AttemptTimeoutRetriesThenFallsBack(t *testing.T) {
	primary := &capsProvider{
		name: "slow-primary",
		caps: ProviderCapabilities{StructuredOutput: true, ToolCalling: true, Streaming: true},
		callFn: func(ctx context.Context, _ llm.CallParams) (*llm.Completion, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	fallback := &capsProvider{
		name: "fallback",
		caps: ProviderCapabilities{StructuredOutput: true, ToolCalling: true, Streaming: true},
	}
	gw := NewGateway(primary, NewRoleThrottle(4))
	gw.retry = fastRetryPolicy(1, 5*time.Millisecond)
	gw.SetFallbacks("lead", fallback)

	resp, err := gw.Call(context.Background(), "lead", llm.CallParams{Model: "test"})
	if err != nil {
		t.Fatalf("attempt timeout must not become a nil/context error: %v", err)
	}
	if resp == nil || resp.Content != "ok" {
		t.Fatalf("unexpected fallback response: %#v", resp)
	}
	if primary.callCount != 2 {
		t.Fatalf("primary callCount = %d, want 2", primary.callCount)
	}
	if fallback.callCount != 1 {
		t.Fatalf("fallback callCount = %d, want 1", fallback.callCount)
	}
}

func TestGateway_ParentCancellationDoesNotFallback(t *testing.T) {
	primary := &capsProvider{
		name: "primary",
		caps: ProviderCapabilities{StructuredOutput: true, ToolCalling: true, Streaming: true},
		callFn: func(ctx context.Context, _ llm.CallParams) (*llm.Completion, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	fallback := &capsProvider{
		name: "fallback",
		caps: ProviderCapabilities{StructuredOutput: true, ToolCalling: true, Streaming: true},
	}
	gw := NewGateway(primary, NewRoleThrottle(4))
	gw.retry = fastRetryPolicy(2, time.Second)
	gw.SetFallbacks("lead", fallback)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := gw.Call(ctx, "lead", llm.CallParams{Model: "test"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if fallback.callCount != 0 {
		t.Fatalf("fallback called after parent cancellation: %d", fallback.callCount)
	}
}

func TestGateway_StreamAttemptTimeoutRetriesThenFallsBack(t *testing.T) {
	primary := &capsProvider{
		name: "slow-primary",
		caps: ProviderCapabilities{StructuredOutput: true, ToolCalling: true, Streaming: true},
		streamFn: func(ctx context.Context, _ llm.CallParams, _ StreamSink) (*llm.StreamResult, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	fallback := &capsProvider{
		name: "fallback",
		caps: ProviderCapabilities{StructuredOutput: true, ToolCalling: true, Streaming: true},
	}
	gw := NewGateway(primary, NewRoleThrottle(4))
	gw.retry = fastRetryPolicy(1, 5*time.Millisecond)
	gw.SetFallbacks("lead", fallback)

	sink := &collectTestSink{}
	result, err := gw.Stream(context.Background(), "lead", llm.CallParams{Model: "test"}, sink)
	if err != nil {
		t.Fatalf("expected stream fallback success, got %v", err)
	}
	if result == nil || result.Content != "ok" {
		t.Fatalf("unexpected stream result: %#v", result)
	}
	if primary.callCount != 2 || fallback.callCount != 1 {
		t.Fatalf("call counts primary=%d fallback=%d, want 2/1", primary.callCount, fallback.callCount)
	}
}

// --- Error classification tests ---

func TestTrackingSink_BasicBehavior(t *testing.T) {
	inner := &collectTestSink{}
	ts := newTrackingSink(inner)

	if ts.emitted {
		t.Fatal("emitted should be false initially")
	}
	ts.OnTextDelta("hello")
	if !ts.emitted {
		t.Fatal("emitted should be true after OnTextDelta")
	}
	if inner.text != "hello" {
		t.Fatalf("inner text = %q", inner.text)
	}
	// OnDone is intercepted: provider calls are no-ops.
	ts.OnDone()
	ts.OnDone()
	if inner.done {
		t.Fatal("inner done should not be called by provider OnDone")
	}
	// Gateway controls OnDone via notifyDone.
	ts.notifyDone()
	if !inner.done {
		t.Fatal("inner done not called after notifyDone")
	}
	if inner.doneCount != 1 {
		t.Fatalf("inner doneCount = %d, want 1", inner.doneCount)
	}
	ts.reset()
	if ts.emitted {
		t.Fatal("emitted not cleared after reset")
	}
}

func TestGatewayInvalidStreamResponseFallsBack(t *testing.T) {
	primary := &capsProvider{
		name: "primary",
		streamFn: func(_ context.Context, _ llm.CallParams, sink StreamSink) (*llm.StreamResult, error) {
			sink.OnTextDelta("")
			sink.OnDone()
			return &llm.StreamResult{FinishReason: "stop"}, nil
		},
	}
	fallback := &capsProvider{name: "fallback"}
	gateway := NewGateway(primary, NewRoleThrottle(4))
	gateway.SetFallbacks("lead", fallback)

	sink := &collectTestSink{}
	result, err := gateway.Stream(context.Background(), "lead", llm.CallParams{Model: "test"}, sink)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if result == nil || result.Content != "ok" {
		t.Fatalf("fallback result = %#v", result)
	}
	if primary.callCount != 1 || fallback.callCount != 1 {
		t.Fatalf("call counts primary=%d fallback=%d, want 1/1", primary.callCount, fallback.callCount)
	}
	if sink.text != "ok" || sink.doneCount != 1 {
		t.Fatalf("sink text=%q done=%d", sink.text, sink.doneCount)
	}
}

func TestGatewayPartialStreamWithNilResultDoesNotRetryOrFallback(t *testing.T) {
	primary := &capsProvider{
		name: "primary",
		streamFn: func(_ context.Context, _ llm.CallParams, sink StreamSink) (*llm.StreamResult, error) {
			sink.OnTextDelta("partial")
			return nil, NewProviderError("primary", 500, "server_error", errors.New("mid-stream failure"))
		},
	}
	fallback := &capsProvider{name: "fallback"}
	gateway := NewGateway(primary, NewRoleThrottle(4))
	gateway.retry = fastRetryPolicy(2, 0)
	gateway.SetFallbacks("lead", fallback)

	sink := &collectTestSink{}
	result, err := gateway.Stream(context.Background(), "lead", llm.CallParams{Model: "test"}, sink)
	if err == nil {
		t.Fatal("expected partial stream error")
	}
	if result != nil {
		t.Fatalf("result = %#v, want nil partial result", result)
	}
	if primary.callCount != 1 || fallback.callCount != 0 {
		t.Fatalf("call counts primary=%d fallback=%d, want 1/0", primary.callCount, fallback.callCount)
	}
	if sink.text != "partial" || sink.doneCount != 1 {
		t.Fatalf("sink text=%q done=%d", sink.text, sink.doneCount)
	}
}

func TestGateway_StreamPartialContentDoesNotFallback(t *testing.T) {
	primary := &capsProvider{
		name: "primary",
		caps: ProviderCapabilities{StructuredOutput: true, ToolCalling: true, Streaming: true},
		streamFn: func(_ context.Context, _ llm.CallParams, sink StreamSink) (*llm.StreamResult, error) {
			sink.OnTextDelta("partial")
			return &llm.StreamResult{Content: "partial"}, NewProviderError(
				"primary", 500, "server_error", errors.New("mid-stream failure"))
		},
	}
	fallback := &capsProvider{
		name: "fallback",
		caps: ProviderCapabilities{StructuredOutput: true, ToolCalling: true, Streaming: true},
	}
	gw := NewGateway(primary, NewRoleThrottle(4))
	gw.retry = fastRetryPolicy(0, 0)
	gw.SetFallbacks("lead", fallback)

	sink := &collectTestSink{}
	result, err := gw.Stream(context.Background(), "lead", llm.CallParams{Model: "test"}, sink)
	if err == nil {
		t.Fatal("expected error from partial stream")
	}
	if result == nil || result.Content != "partial" {
		t.Fatalf("expected partial content, got %#v", result)
	}
	if sink.text != "partial" {
		t.Fatalf("sink text = %q, want %q", sink.text, "partial")
	}
	if fallback.callCount != 0 {
		t.Fatalf("fallback should not be called, count = %d", fallback.callCount)
	}
	if sink.doneCount != 1 {
		t.Fatalf("sink doneCount = %d, want 1", sink.doneCount)
	}
}

func TestGateway_StreamNoContentFallbackAndNoDoubleOnDone(t *testing.T) {
	primary := &capsProvider{
		name: "primary",
		caps: ProviderCapabilities{StructuredOutput: true, ToolCalling: true, Streaming: true},
		streamFn: func(_ context.Context, _ llm.CallParams, sink StreamSink) (*llm.StreamResult, error) {
			sink.OnDone()
			return nil, NewProviderError(
				"primary", 500, "server_error", errors.New("immediate failure"))
		},
	}
	fallback := &capsProvider{
		name: "fallback",
		caps: ProviderCapabilities{StructuredOutput: true, ToolCalling: true, Streaming: true},
	}
	gw := NewGateway(primary, NewRoleThrottle(4))
	gw.retry = fastRetryPolicy(0, 0)
	gw.SetFallbacks("lead", fallback)

	sink := &collectTestSink{}
	result, err := gw.Stream(context.Background(), "lead", llm.CallParams{Model: "test"}, sink)
	if err != nil {
		t.Fatalf("expected fallback success, got %v", err)
	}
	if result == nil || result.Content != "ok" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if sink.doneCount != 1 {
		t.Fatalf("sink doneCount = %d, want 1", sink.doneCount)
	}
}

func TestClassifyError(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		class ErrorClass
	}{
		{"nil", nil, ErrorTransient},
		{"context cancelled", context.Canceled, ErrorContext},
		{"deadline exceeded", context.DeadlineExceeded, ErrorContext},
		{"401", errors.New("401 Unauthorized"), ErrorPermanent},
		{"forbidden", errors.New("forbidden access"), ErrorPermanent},
		{"model not found", errors.New("model not found: gpt-x"), ErrorPermanent},
		{"invalid api key", errors.New("invalid_api_key"), ErrorPermanent},
		{"429 rate limit", errors.New("429 too many requests"), ErrorTransient},
		{"500 internal", errors.New("500 internal server error"), ErrorTransient},
		{"timeout", errors.New("context deadline exceeded (timeout)"), ErrorContext},
		{"network", errors.New("connection reset by peer"), ErrorTransient},
		{
			"typed transient overrides permanent-looking message",
			NewProviderError("test", 503, "server_error", errors.New("invalid request")),
			ErrorTransient,
		},
		{
			"typed permanent overrides transient-looking message",
			NewProviderError("test", 400, "invalid_request_error", errors.New("temporary timeout")),
			ErrorPermanent,
		},
		{
			"insufficient quota overrides HTTP 429",
			NewProviderError("openai", 429, "insufficient_quota", errors.New("quota exhausted")),
			ErrorPermanent,
		},
		{
			"wrapped context remains context",
			NewProviderError("test", 0, "", context.Canceled),
			ErrorContext,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyError(tt.err)
			if got != tt.class {
				t.Fatalf("ClassifyError(%v) = %d, want %d", tt.err, got, tt.class)
			}
		})
	}
}

func TestProviderErrorMetadataSurvivesRetryWrapping(t *testing.T) {
	cause := errors.New("slow down")
	providerErr := NewProviderError("openai", 429, "rate_limit_exceeded", cause)
	wrapped := &RetryExhaustedError{Provider: "openai", Attempts: 3, Err: providerErr}

	if !errors.Is(wrapped, cause) {
		t.Fatal("source error was not retained")
	}
	var extracted *ProviderError
	if !errors.As(wrapped, &extracted) || extracted != providerErr {
		t.Fatalf("ProviderError was not retained through RetryExhaustedError: %#v", extracted)
	}
	if !isRateLimitError(wrapped) {
		t.Fatal("typed rate-limit metadata was not recognized")
	}
}

// --- helpers ---

type collectTestSink struct {
	text      string
	done      bool
	doneCount int
}

func (s *collectTestSink) OnTextDelta(text string)      { s.text += text }
func (s *collectTestSink) OnReasoningDelta(text string) {}
func (s *collectTestSink) OnDone() {
	s.done = true
	s.doneCount++
}

// collectEventSink records emitted events (implements event.Sink).
type collectEventSink struct {
	events []event.Event
}

func (s *collectEventSink) Emit(e event.Event) { s.events = append(s.events, e) }

func (s *collectEventSink) summaries() []string {
	var out []string
	for _, e := range s.events {
		if e.Type != event.ModelRetry {
			continue
		}
		if mp, ok := e.Payload.(map[string]string); ok {
			out = append(out, mp["summary"])
		}
	}
	return out
}

func TestGateway_RetryAndFallbackEmitModelRetryEvents(t *testing.T) {
	calls := 0
	primary := &capsProvider{
		name: "primary",
		caps: ProviderCapabilities{Streaming: true},
		callFn: func(context.Context, llm.CallParams) (*llm.Completion, error) {
			calls++
			if calls == 1 {
				return nil, NewProviderError("primary", 429, "rate_limit_error", errors.New("slow down"))
			}
			return nil, NewProviderError("primary", 401, "invalid_api_key", errors.New("bad key"))
		},
	}
	fallback := &capsProvider{name: "backup", caps: ProviderCapabilities{Streaming: true}}

	rec := &collectEventSink{}
	gw := NewGateway(primary, NewRoleThrottle(4))
	gw.retry = fastRetryPolicy(3, time.Second)
	gw.SetFallbacks("lead", fallback)

	ctx := WithCallObservers(context.Background(), CallObservers{Events: rec})
	if _, err := gw.Call(ctx, "lead", llm.CallParams{Model: "test"}); err != nil {
		t.Fatalf("Call: %v", err)
	}

	got := rec.summaries()
	if len(got) != 2 {
		t.Fatalf("ModelRetry events = %d (%v), want 2 (1 retry + 1 fallback)", len(got), got)
	}
	if !strings.Contains(got[0], "llm retry 1/3") || !strings.Contains(got[0], "provider=primary") {
		t.Fatalf("retry summary = %q", got[0])
	}
	if !strings.Contains(got[1], "provider fallback") || !strings.Contains(got[1], "primary → backup") {
		t.Fatalf("fallback summary = %q", got[1])
	}
}

func TestGateway_CallObserversAreRequestScoped(t *testing.T) {
	calls := 0
	primary := &capsProvider{
		name: "primary",
		caps: ProviderCapabilities{Streaming: true},
		callFn: func(context.Context, llm.CallParams) (*llm.Completion, error) {
			calls++
			switch calls {
			case 1:
				return nil, NewProviderError("primary", 429, "rate_limit_error", errors.New("slow down"))
			case 2:
				return nil, NewProviderError("primary", 401, "invalid_api_key", errors.New("bad key"))
			default:
				return &llm.Completion{Content: "ok", FinishReason: "stop"}, nil
			}
		},
	}
	fallback := &capsProvider{name: "backup", caps: ProviderCapabilities{Streaming: true}}
	gw := NewGateway(primary, NewRoleThrottle(4))
	gw.retry = fastRetryPolicy(3, time.Second)
	gw.SetFallbacks("lead", fallback)

	firstEvents := &collectEventSink{}
	secondEvents := &collectEventSink{}
	firstUsage, secondUsage := 0, 0
	firstCtx := WithCallObservers(context.Background(), CallObservers{
		Events: firstEvents,
		Usage: func(string, string, string, string, llm.Usage, float64) {
			firstUsage++
		},
	})
	secondCtx := WithCallObservers(context.Background(), CallObservers{
		Events: secondEvents,
		Usage: func(string, string, string, string, llm.Usage, float64) {
			secondUsage++
		},
	})

	if _, err := gw.Call(firstCtx, "lead", llm.CallParams{Model: "test"}); err != nil {
		t.Fatalf("first Call: %v", err)
	}
	if firstUsage != 1 {
		t.Fatalf("first usage callbacks = %d, want 1 (fallback success)", firstUsage)
	}
	gotFirst := firstEvents.summaries()
	if len(gotFirst) != 2 {
		t.Fatalf("first ModelRetry events = %d (%v), want 2", len(gotFirst), gotFirst)
	}
	if len(secondEvents.summaries()) != 0 || secondUsage != 0 {
		t.Fatalf("second observers were polluted: events=%v usage=%d", secondEvents.summaries(), secondUsage)
	}

	calls = 0
	if _, err := gw.Call(secondCtx, "lead", llm.CallParams{Model: "test"}); err != nil {
		t.Fatalf("second Call: %v", err)
	}
	if secondUsage != 1 {
		t.Fatalf("second usage callbacks = %d, want 1", secondUsage)
	}
	gotSecond := secondEvents.summaries()
	if len(gotSecond) != 2 {
		t.Fatalf("second ModelRetry events = %d (%v), want 2", len(gotSecond), gotSecond)
	}
	if len(firstEvents.summaries()) != 2 {
		t.Fatalf("first observers changed after second call: %v", firstEvents.summaries())
	}
}
