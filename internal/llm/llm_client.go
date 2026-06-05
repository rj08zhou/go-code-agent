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
func isRetriableLLMError(prov Provider, err error) bool {
	if err == nil {
		return false
	}
	if prov == nil {
		return false
	}
	if fn, ok := retriableClassifiers[prov.Name()]; ok {
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

// retryDelay merges the client-side exponential backoff with any
// Retry-After hint from the upstream error. We always wait at least
// the gateway's suggestion (capped at LlmMaxDelay so a hostile or
// buggy server can't pin us forever) so we don't burn another retry
// inside the same throttle window.
func retryDelay(attempt int, err error) (time.Duration, time.Duration) {
	rateLimited := isRateLimitError(err)
	backoff := backoffDelay(attempt, rateLimited)
	hint := retryAfterFromError(err)
	if hint <= 0 {
		return backoff, 0
	}
	if hint > infra.LlmMaxDelay {
		hint = infra.LlmMaxDelay
	}
	if hint > backoff {
		return hint, hint
	}
	return backoff, hint
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

// Client wraps a Provider with the shared retry / timeout / usage
// telemetry orchestration. The Provider interface itself only exposes
// the atomic Call/Stream operations; the retry policy lives here, one
// level up, so every backend shares it instead of reimplementing it.
//
// Use NewClient(nil) to target the active (main) provider selected at
// startup; pass an explicit provider (e.g. llm.JudgeProvider(...)) to
// route a single call to a different backend / endpoint.
type Client struct {
	provider Provider
}

// NewClient wraps p. A nil p falls back to the active (main) provider
// selected at startup, so NewClient(nil).CallWithRetry(...) behaves like
// the old package-level CallLLMWithRetry.
func NewClient(p Provider) *Client {
	if p == nil {
		p = activeProvider
	}
	return &Client{provider: p}
}

// CallWithRetry runs a non-streaming LLM call with retries.
func (c *Client) CallWithRetry(ctx context.Context, source string, params CallParams) (*Completion, error) {
	if c.provider == nil {
		return nil, fmt.Errorf("no active LLM provider")
	}
	lim := getLimiter()
	var lastErr error
	for attempt := 0; attempt <= infra.LlmMaxRetries; attempt++ {
		// Process-wide throttle: bound QPS and parallelism BEFORE we
		// hit the wire. release() runs even on error so a failing
		// attempt still frees its concurrency slot.
		release, acqErr := lim.Acquire(ctx)
		if acqErr != nil {
			return nil, acqErr
		}
		started := time.Now()
		// Per-attempt deadline. Without this a hung backend (e.g. an
		// OpenAI-compatible gateway holding the SSE socket open without
		// emitting bytes) freezes the whole agent loop with no
		// subprocess and no audit trail.
		attemptCtx, cancel := context.WithTimeout(ctx, infra.LlmCallTimeout)
		resp, err := c.provider.Call(attemptCtx, params)
		cancel()
		release()
		if err == nil {
			traceID := GetTraceID(ctx)
			if usageRecordFn != nil {
				usageRecordFn(source, c.provider.Name(), params.Model,
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
		if !isRetriableLLMError(c.provider, err) || attempt == infra.LlmMaxRetries {
			return nil, err
		}
		delay, hint := retryDelay(attempt, err)
		logRetryAttempt(attempt, err, delay, hint)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, lastErr
}

// StreamWithRetry runs a streaming LLM call with retries (stdout sink).
func (c *Client) StreamWithRetry(ctx context.Context, source string, params CallParams) (*StreamResult, error) {
	return c.StreamWithRetrySink(ctx, source, params, NewStdoutStreamSink())
}

// StreamWithRetrySink is the full-control variant for custom sinks.
func (c *Client) StreamWithRetrySink(ctx context.Context, source string, params CallParams, sink StreamSink) (*StreamResult, error) {
	if c.provider == nil {
		return nil, fmt.Errorf("no active LLM provider")
	}
	lim := getLimiter()
	var lastErr error
	for attempt := 0; attempt <= infra.LlmMaxRetries; attempt++ {
		release, acqErr := lim.Acquire(ctx)
		if acqErr != nil {
			return nil, acqErr
		}
		started := time.Now()
		// Per-attempt deadline. Streaming calls can hang mid-response
		// when an upstream gateway stops emitting chunks; without this
		// the for-range over Stream.Next() blocks forever.
		attemptCtx, cancel := context.WithTimeout(ctx, infra.LlmCallTimeout)
		sr, err := c.provider.Stream(attemptCtx, params, sink)
		cancel()
		release()
		if err == nil {
			traceID := GetTraceID(ctx)
			if usageRecordFn != nil {
				usageRecordFn(source, c.provider.Name(), params.Model,
					traceID, sr.Usage, time.Since(started))
			}
			return sr, nil
		}
		lastErr = err
		// Already printed text -> don't retry (double output).
		if sr != nil && sr.Content != "" {
			traceID := GetTraceID(ctx)
			if usageRecordFn != nil {
				usageRecordFn(source, c.provider.Name(), params.Model,
					traceID, sr.Usage, time.Since(started))
			}
			return sr, err
		}
		// Parent ctx already done -> stop retrying.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if !isRetriableLLMError(c.provider, err) || attempt == infra.LlmMaxRetries {
			return nil, err
		}
		delay, hint := retryDelay(attempt, err)
		logRetryAttempt(attempt, err, delay, hint)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, lastErr
}

// logRetryAttempt emits a structured retry diagnostic. Unlike the old
// version which truncated the error and dropped the Retry-After value,
// it now records:
//   - attempt index / cap
//   - the chosen wait duration
//   - any server-supplied Retry-After hint (so 1302/1305 etc. become
//     greppable in session.log)
//   - a longer-but-still-bounded slice of the error message, instead
//     of the previous 120-char head that often cut off the upstream
//     JSON code.
func logRetryAttempt(attempt int, err error, delay, hint time.Duration) {
	body := errSnippet(err, 400)
	if hint > 0 {
		log.PrintSystem(fmt.Sprintf("[llm-retry] attempt %d/%d failed (retry_after=%s, waiting %s): %s",
			attempt+1, infra.LlmMaxRetries, hint.Round(100*time.Millisecond),
			delay.Round(100*time.Millisecond), body))
		return
	}
	log.PrintSystem(fmt.Sprintf("[llm-retry] attempt %d/%d failed (waiting %s): %s",
		attempt+1, infra.LlmMaxRetries, delay.Round(100*time.Millisecond), body))
}

// errSnippet returns a length-bounded view of err's message, preserving
// enough tail for upstream JSON codes (e.g. {"code":"1302",...}) to
// remain visible. Short errors are returned verbatim.
func errSnippet(err error, max int) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) <= max {
		return s
	}
	// Keep both head and tail so we still see "429 Too Many Requests"
	// at the start AND the {"code":...} payload at the end.
	half := (max - 5) / 2
	return s[:half] + " ... " + s[len(s)-half:]
}
