package llm

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/log"
	"math/rand"
	"strings"
	"time"
)

// Trace ID — correlates all LLM calls belonging to the same user request.

type traceIDKey struct{}

// WithTraceID returns a derived context carrying the given trace ID.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceIDKey{}, traceID)
}

// GetTraceID retrieves the trace ID from ctx. Returns "" if none is set
// (e.g. REPL /usage command calls helpers without a trace).
func GetTraceID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(traceIDKey{}).(string)
	return v
}

// NewTraceID generates a unique trace identifier ("T" + 24 hex chars).
func NewTraceID() string {
	// Prefix: 6 hex chars from current unix seconds (time-ordered).
	now := uint64(time.Now().Unix())
	var prefix [6]byte
	for i := 5; i >= 0; i-- {
		prefix[i] = "0123456789abcdef"[now&0xf]
		now >>= 4
	}
	// Suffix: 6 bytes cryptographic random, encoded as 12 hex chars.
	var rnd [6]byte
	_, _ = crand.Read(rnd[:])
	return "T" + string(prefix[:]) + hex.EncodeToString(rnd[:])
}

// Provider + StreamSink interfaces.

// Provider is the interface every LLM backend implements.
type Provider interface {
	Name() string
	Call(ctx context.Context, params CallParams) (*Completion, error)
	Stream(ctx context.Context, params CallParams, sink StreamSink) (*StreamResult, error)
}

// StreamSink receives streaming events from a provider.
type StreamSink interface {
	// OnTextDelta is called for every text fragment the model emits.
	OnTextDelta(text string)
	// OnDone is called once the stream completes (success or failure).
	OnDone()
}

// RetriableClassifier classifies whether a provider error is retriable.
type RetriableClassifier func(err error) bool

// UsageRecordFunc is the callback for recording LLM usage telemetry.
type UsageRecordFunc func(source, provider, model, traceID string, u Usage, dur time.Duration)

// Package-level state (set once at startup).
var (
	activeProvider       Provider
	retriableClassifiers = map[string]RetriableClassifier{}
	usageRecordFn        UsageRecordFunc
)

// SetProvider configures the active LLM provider. Called once at startup.
func SetProvider(p Provider) {
	activeProvider = p
}

// GetProvider returns the active LLM provider.
func GetProvider() Provider {
	return activeProvider
}

// SetRetriableClassifier registers a provider-specific error classifier.
func SetRetriableClassifier(providerName string, fn RetriableClassifier) {
	retriableClassifiers[providerName] = fn
}

// SetUsageRecorder sets the usage recording callback. Called once at startup.
func SetUsageRecorder(fn UsageRecordFunc) {
	usageRecordFn = fn
}

// LLM client - retry wrapper over the active Provider.

var TransientMsgHints = []string{
	"connection reset", "connection refused", "eof",
	"timeout", "timed out", "temporary failure",
	"too many requests", "rate limit",
	"internal server error", "bad gateway",
	"service unavailable", "gateway timeout",
}

// isRetriableLLMError dispatches to the provider-specific classifier.
func isRetriableLLMError(err error) bool {
	if err == nil {
		return false
	}
	if activeProvider == nil {
		return false
	}
	if fn, ok := retriableClassifiers[activeProvider.Name()]; ok {
		return fn(err)
	}
	return false
}

// backoffDelay returns exponential backoff with ±25% jitter.
// When isRateLimit is true (429 Too Many Requests), uses a longer base delay
// to avoid hammering the API and wasting retry attempts.
func backoffDelay(attempt int, isRateLimit bool) time.Duration {
	base := infra.LlmBaseDelay
	if isRateLimit {
		base = infra.LlmRateLimitDelay
	}
	d := base << attempt
	if d > infra.LlmMaxDelay {
		d = infra.LlmMaxDelay
	}
	jitter := time.Duration(rand.Int63n(int64(d) / 2))
	return d - d/4 + jitter
}

// isRateLimitError checks if an error is a 429 rate-limit error by inspecting
// the error message. This is provider-agnostic.
func isRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "429") || strings.Contains(msg, "too many requests") || strings.Contains(msg, "rate limit")
}

// CallLLMWithRetry runs a non-streaming LLM call with retries.
func CallLLMWithRetry(ctx context.Context, source string, params CallParams) (*Completion, error) {
	if activeProvider == nil {
		return nil, fmt.Errorf("no active LLM provider")
	}
	var lastErr error
	for attempt := 0; attempt <= infra.LlmMaxRetries; attempt++ {
		started := time.Now()
		// Per-attempt deadline. Without this a hung backend (e.g. an
		// OpenAI-compatible gateway holding the SSE socket open without
		// emitting bytes) freezes the whole agent loop with no
		// subprocess and no audit trail.
		attemptCtx, cancel := context.WithTimeout(ctx, infra.LlmCallTimeout)
		resp, err := activeProvider.Call(attemptCtx, params)
		cancel()
		if err == nil {
			traceID := GetTraceID(ctx)
			if usageRecordFn != nil {
				usageRecordFn(source, activeProvider.Name(), params.Model,
					traceID, resp.Usage, time.Since(started))
			}
			return resp, nil
		}
		lastErr = err
		// If the parent ctx is already done, the per-attempt timeout
		// is moot — propagate the parent error and stop retrying.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if !isRetriableLLMError(err) || attempt == infra.LlmMaxRetries {
			return nil, err
		}
		delay := backoffDelay(attempt, isRateLimitError(err))
		log.PrintSystem(fmt.Sprintf("[llm-retry] attempt %d/%d failed: %v - retrying in %s",
			attempt+1, infra.LlmMaxRetries, truncateErr(err), delay.Round(100*time.Millisecond)))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, lastErr
}

// StreamLLMWithRetry runs a streaming LLM call with retries (stdout sink).
func StreamLLMWithRetry(ctx context.Context, source string, params CallParams) (*StreamResult, error) {
	return StreamLLMWithRetrySink(ctx, source, params, NewStdoutStreamSink())
}

// StreamLLMWithRetrySink is the full-control variant for custom sinks.
func StreamLLMWithRetrySink(ctx context.Context, source string, params CallParams, sink StreamSink) (*StreamResult, error) {
	if activeProvider == nil {
		return nil, fmt.Errorf("no active LLM provider")
	}
	var lastErr error
	for attempt := 0; attempt <= infra.LlmMaxRetries; attempt++ {
		started := time.Now()
		// Per-attempt deadline. Streaming calls can hang mid-response
		// when an upstream gateway stops emitting chunks; without this
		// the for-range over Stream.Next() blocks forever.
		attemptCtx, cancel := context.WithTimeout(ctx, infra.LlmCallTimeout)
		sr, err := activeProvider.Stream(attemptCtx, params, sink)
		cancel()
		if err == nil {
			traceID := GetTraceID(ctx)
			if usageRecordFn != nil {
				usageRecordFn(source, activeProvider.Name(), params.Model,
					traceID, sr.Usage, time.Since(started))
			}
			return sr, nil
		}
		lastErr = err
		// Already printed text -> don't retry (double output).
		if sr != nil && sr.Content != "" {
			traceID := GetTraceID(ctx)
			if usageRecordFn != nil {
				usageRecordFn(source, activeProvider.Name(), params.Model,
					traceID, sr.Usage, time.Since(started))
			}
			return sr, err
		}
		// Parent ctx already done -> stop retrying.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if !isRetriableLLMError(err) || attempt == infra.LlmMaxRetries {
			return nil, err
		}
		delay := backoffDelay(attempt, isRateLimitError(err))
		log.PrintSystem(fmt.Sprintf("[llm-retry] attempt %d/%d failed: %v - retrying in %s",
			attempt+1, infra.LlmMaxRetries, truncateErr(err), delay.Round(100*time.Millisecond)))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, lastErr
}

// truncateErr shortens error messages for logging.
func truncateErr(err error) string {
	s := err.Error()
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}


