package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"go-code-agent/internal/config"
	"go-code-agent/internal/event"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/logging"
	"strings"
	"sync"
	"time"
)

// --- Trace ID ---

type traceIDKey struct{}

func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceIDKey{}, traceID)
}

func GetTraceID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(traceIDKey{}).(string)
	return v
}

func NewTraceID() string {
	// Use nanosecond timestamp for uniqueness (original uses full unixNano).
	ts := time.Now().UnixNano()
	// Encode timestamp as 12 hex chars (~281 years of uniqueness).
	tsHex := fmt.Sprintf("%012x", uint64(ts))
	// 8 bytes of random → 16 hex chars.
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	return "T-" + tsHex + "-" + hex.EncodeToString(rnd[:])
}

// --- RoleThrottle ---

// RoleThrottle provides per-role concurrency limiting with lead priority.
// Each role has its own token bucket; if a role's bucket is full, the
// call blocks until a token is available or the context expires.
type RoleThrottle struct {
	tokens   map[string]chan struct{}
	capacity map[string]int
	mu       sync.RWMutex
}

// NewRoleThrottle creates a throttle with per-role capacities.
// totalConcurrency is the sum across all roles.
// Lead gets a guaranteed reserve (default 40% of total).
func NewRoleThrottle(totalConcurrency int) *RoleThrottle {
	rt := &RoleThrottle{
		tokens:   make(map[string]chan struct{}),
		capacity: make(map[string]int),
	}

	// Default role allocation:
	// lead: 40%, explore: 30%, teammate: 20%, judge: 10%
	allocations := map[string]float64{
		"lead":     0.4,
		"explore":  0.3,
		"teammate": 0.2,
		"judge":    0.1,
	}
	for role, frac := range allocations {
		cap := int(float64(totalConcurrency) * frac)
		if cap < 1 {
			cap = 1
		}
		rt.tokens[role] = make(chan struct{}, cap)
		rt.capacity[role] = cap
	}
	return rt
}

// Acquire waits for a token in the given role's bucket.
func (t *RoleThrottle) Acquire(ctx context.Context, role string) (release func(), err error) {
	ch, ok := t.tokens[role]
	if !ok {
		// Unknown role: fallback to a shared small bucket
		ch = t.tokens["default"]
		if ch == nil {
			t.mu.Lock()
			ch = make(chan struct{}, 2)
			t.tokens["default"] = ch
			t.mu.Unlock()
		}
	}
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return func() {}, ctx.Err()
	}
}

// Capacity returns the maximum concurrency for a given role.
func (t *RoleThrottle) Capacity(role string) int {
	return t.capacity[role]
}

// --- Error Classification ---

// ErrorClass categorizes a provider error to drive retry vs fallback decisions.
type ErrorClass int

const (
	// ErrorTransient: rate limits, timeouts, 5xx, network blips — retry on same provider.
	ErrorTransient ErrorClass = iota
	// ErrorPermanent: 401/403 auth, 400 bad request, model not found — don't retry, try fallback.
	ErrorPermanent
	// ErrorContext: context cancelled / deadline exceeded — abort immediately.
	ErrorContext
)

// ProviderError is the provider-neutral form of an API or transport failure.
// Retryable is set at the provider boundary so retry decisions do not depend on
// unstable SDK error strings. Err is retained for errors.Is/errors.As.
type ProviderError struct {
	Provider   string
	StatusCode int
	Code       string
	Retryable  bool
	Err        error
}

func NewProviderError(provider string, statusCode int, code string, err error) *ProviderError {
	return &ProviderError{
		Provider:   provider,
		StatusCode: statusCode,
		Code:       code,
		Retryable:  providerFailureRetryable(statusCode, code),
		Err:        err,
	}
}

func (e *ProviderError) Error() string {
	if e == nil {
		return "<nil>"
	}
	label := "provider error"
	if e.Provider != "" {
		label = e.Provider + " provider error"
	}
	details := make([]string, 0, 2)
	if e.StatusCode != 0 {
		details = append(details, fmt.Sprintf("status=%d", e.StatusCode))
	}
	if e.Code != "" {
		details = append(details, "code="+e.Code)
	}
	if len(details) > 0 {
		label += " [" + strings.Join(details, ", ") + "]"
	}
	if e.Err != nil {
		return label + ": " + e.Err.Error()
	}
	return label
}

func (e *ProviderError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func providerFailureRetryable(statusCode int, code string) bool {
	normalizedCode := strings.ToLower(strings.TrimSpace(code))
	switch normalizedCode {
	case "insufficient_quota", "billing_error", "missing_api_key", "invalid_api_key",
		"authentication_error", "permission_error", "invalid_request_error", "not_found_error",
		"invalid_argument", "permission_denied", "unauthenticated", "failed_precondition",
		"invalid_request", "invalid_request_url", "invalid_response", "empty_response":
		return false
	}

	if statusCode >= 400 {
		return statusCode == 408 || statusCode == 409 || statusCode == 425 ||
			statusCode == 429 || statusCode >= 500
	}

	switch normalizedCode {
	case "rate_limit_error", "rate_limit_exceeded", "overloaded_error", "api_error",
		"server_error", "timeout_error", "resource_exhausted", "deadline_exceeded",
		"unavailable", "internal", "aborted", "response_read_error":
		return true
	}

	// No HTTP response and no provider code means a transport-level failure.
	return statusCode == 0 && normalizedCode == ""
}

// permanentHints are a compatibility fallback for providers that have not yet
// adopted ProviderError.
var permanentHints = []string{
	"401", "unauthorized", "forbidden", "403",
	"invalid", "bad request", "400",
	"model not found", "does not exist",
	"quota", "exceeded", "insufficient",
	"api key", "invalid_api_key",
}

// ClassifyError categorizes an error into transient (retry), permanent (fallback),
// or context (abort). Typed provider metadata is authoritative; string matching
// remains only for third-party or test providers returning legacy errors.
func ClassifyError(err error) ErrorClass {
	if err == nil {
		return ErrorTransient
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ErrorContext
	}
	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		if providerErr.Retryable {
			return ErrorTransient
		}
		return ErrorPermanent
	}
	msg := strings.ToLower(err.Error())
	// Context-like errors (string-matched for legacy SDK-wrapped errors).
	if strings.Contains(msg, "context deadline exceeded") || strings.Contains(msg, "context canceled") {
		return ErrorContext
	}
	for _, h := range permanentHints {
		if strings.Contains(msg, h) {
			return ErrorPermanent
		}
	}
	// Everything else (429, 5xx, timeout, network) is transient.
	return ErrorTransient
}

// --- Retry Logic ---

// RetryExhaustedError marks a transient provider failure that persisted through
// every retry. Gateway may safely try the next provider after this error.
type RetryExhaustedError struct {
	Provider string
	Attempts int
	Err      error
}

func (e *RetryExhaustedError) Error() string {
	return fmt.Sprintf("provider %s failed after %d attempts: %v", e.Provider, e.Attempts, e.Err)
}

func (e *RetryExhaustedError) Unwrap() error { return e.Err }

type retryPolicy struct {
	maxRetries  int
	callTimeout time.Duration
	delay       func(attempt int, rateLimited bool) time.Duration
}

func defaultRetryPolicy() retryPolicy {
	return retryPolicy{
		maxRetries:  config.LlmMaxRetries,
		callTimeout: config.LlmCallTimeout,
		delay:       backoffDelay,
	}
}

func isRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		if providerErr.StatusCode == 429 {
			return true
		}
		switch strings.ToLower(strings.TrimSpace(providerErr.Code)) {
		case "rate_limit_error", "rate_limit_exceeded", "too_many_requests", "resource_exhausted":
			return true
		default:
			return false
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "429") || strings.Contains(msg, "too many requests") ||
		strings.Contains(msg, "rate limit") || strings.Contains(msg, "rate_limit")
}

func backoffDelay(attempt int, isRateLimit bool) time.Duration {
	base := config.LlmBaseDelay
	if isRateLimit {
		base = config.LlmRateLimitDelay
	}
	d := base << attempt
	if d > config.LlmMaxDelay {
		d = config.LlmMaxDelay
	}
	return d
}

// classifyAttemptError distinguishes cancellation of the whole request from a
// timeout/cancellation local to one provider attempt. Only the former aborts the
// Gateway; a local timeout is transient and can be retried or fall back.
func classifyAttemptError(parent context.Context, attemptErr, callErr error) ErrorClass {
	if parentErr := parent.Err(); parentErr != nil {
		return ErrorContext
	}
	if errors.Is(attemptErr, context.DeadlineExceeded) || errors.Is(attemptErr, context.Canceled) {
		return ErrorTransient
	}
	class := ClassifyError(callErr)
	if class == ErrorContext {
		return ErrorTransient
	}
	return class
}

func retryDelay(policy retryPolicy, attempt int, err error) time.Duration {
	if policy.delay == nil {
		return 0
	}
	return policy.delay(attempt, isRateLimitError(err))
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func validateCompletion(provider Provider, response *llm.Completion) error {
	if response == nil {
		return invalidModelResponse(provider, "nil completion")
	}
	if strings.TrimSpace(response.Content) == "" && len(response.ToolCalls) == 0 {
		return invalidModelResponse(provider, "completion contained neither text nor tool calls")
	}
	return nil
}

func validateStreamResult(provider Provider, result *llm.StreamResult) error {
	if result == nil {
		return invalidModelResponse(provider, "nil stream result")
	}
	if strings.TrimSpace(result.Content) == "" && len(result.ToolCalls) == 0 {
		return invalidModelResponse(provider, "stream contained neither text nor tool calls")
	}
	return nil
}

func invalidModelResponse(provider Provider, detail string) error {
	name := "unknown"
	if provider != nil && strings.TrimSpace(provider.Name()) != "" {
		name = provider.Name()
	}
	return NewProviderError(name, 0, "empty_response", errors.New(detail))
}

func callWithRetry(ctx context.Context, p Provider, params llm.CallParams, throttle *RoleThrottle, usageFn UsageRecorder, eventSink event.Sink, role string, policy retryPolicy) (*llm.Completion, error) {
	if p == nil {
		return nil, fmt.Errorf("no active LLM provider")
	}
	for attempt := 0; attempt <= policy.maxRetries; attempt++ {
		release, err := throttle.Acquire(ctx, role)
		if err != nil {
			return nil, err
		}
		started := time.Now()
		attemptCtx, cancel := context.WithTimeout(ctx, policy.callTimeout)
		resp, callErr := p.Call(attemptCtx, params)
		attemptErr := attemptCtx.Err() // capture before cancel marks it canceled
		cancel()
		release()
		if callErr == nil {
			callErr = validateCompletion(p, resp)
		}
		if resp != nil && usageFn != nil {
			usageFn(role, p.Name(), params.Model, GetTraceID(ctx), resp.Usage, time.Since(started).Seconds())
		}
		if callErr == nil {
			return resp, nil
		}

		switch classifyAttemptError(ctx, attemptErr, callErr) {
		case ErrorContext:
			return nil, ctx.Err()
		case ErrorPermanent:
			return nil, callErr // Gateway may fall back immediately
		case ErrorTransient:
			if attempt == policy.maxRetries {
				return nil, &RetryExhaustedError{Provider: p.Name(), Attempts: attempt + 1, Err: callErr}
			}
			delay := retryDelay(policy, attempt, callErr)
			logRetryWait(ctx, eventSink, role, p, attempt, policy.maxRetries, delay, callErr)
			if err := waitForRetry(ctx, delay); err != nil {
				return nil, err
			}
		}
	}
	panic("unreachable")
}

// logRetryWait makes backoff visible: without it the user may stare at a
// silent terminal for minutes (rate-limit delays reach 60s per attempt).
// It goes through the event sink (console + session.log); logging is only a
// fallback for wirings without a sink (eval, tests).
func logRetryWait(ctx context.Context, eventSink event.Sink, role string, p Provider, attempt, maxRetries int, delay time.Duration, err error) {
	summary := fmt.Sprintf("llm retry %d/%d: role=%s provider=%s waiting %s (%v)",
		attempt+1, maxRetries, role, p.Name(), delay.Round(time.Second), err)
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

func streamWithRetry(ctx context.Context, p Provider, params llm.CallParams, sink *trackingSink, throttle *RoleThrottle, usageFn UsageRecorder, eventSink event.Sink, role string, policy retryPolicy) (*llm.StreamResult, error) {
	if p == nil {
		return nil, fmt.Errorf("no active LLM provider")
	}
	for attempt := 0; attempt <= policy.maxRetries; attempt++ {
		release, err := throttle.Acquire(ctx, role)
		if err != nil {
			return nil, err
		}
		started := time.Now()
		attemptCtx, cancel := context.WithTimeout(ctx, policy.callTimeout)
		sr, callErr := p.Stream(attemptCtx, params, sink)
		attemptErr := attemptCtx.Err() // capture before cancel marks it canceled
		cancel()
		release()
		if callErr == nil {
			callErr = validateStreamResult(p, sr)
		}
		if sr != nil && usageFn != nil {
			usageFn(role, p.Name(), params.Model, GetTraceID(ctx), sr.Usage, time.Since(started).Seconds())
		}
		if callErr == nil {
			return sr, nil
		}
		if sink.emitted {
			// Partial content was shown to the user; don't retry or fall back.
			return sr, callErr
		}

		switch classifyAttemptError(ctx, attemptErr, callErr) {
		case ErrorContext:
			return nil, ctx.Err()
		case ErrorPermanent:
			return nil, callErr
		case ErrorTransient:
			if attempt == policy.maxRetries {
				return nil, &RetryExhaustedError{Provider: p.Name(), Attempts: attempt + 1, Err: callErr}
			}
			delay := retryDelay(policy, attempt, callErr)
			logRetryWait(ctx, eventSink, role, p, attempt, policy.maxRetries, delay, callErr)
			if err := waitForRetry(ctx, delay); err != nil {
				return nil, err
			}
			sink.reset()
		}
	}
	panic("unreachable")
}
