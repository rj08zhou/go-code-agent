package model

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"go-code-agent-refactor/internal/config"
	"go-code-agent-refactor/internal/llm"
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

// --- Retry Logic ---

var transientHints = []string{
	"connection reset", "connection refused", "eof",
	"timeout", "timed out", "temporary failure",
	"too many requests", "rate limit",
	"internal server error", "bad gateway",
	"service unavailable", "gateway timeout",
}

func isRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "429") || strings.Contains(msg, "too many requests") || strings.Contains(msg, "rate limit")
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

func callWithRetry(ctx context.Context, p Provider, params llm.CallParams, throttle *RoleThrottle, usageFn UsageRecorder, role string) (*llm.Completion, error) {
	if p == nil {
		return nil, fmt.Errorf("no active LLM provider")
	}
	var lastErr error
	for attempt := 0; attempt <= config.LlmMaxRetries; attempt++ {
		release, err := throttle.Acquire(ctx, role)
		if err != nil {
			return nil, err
		}
		started := time.Now()
		attemptCtx, cancel := context.WithTimeout(ctx, config.LlmCallTimeout)
		resp, err := p.Call(attemptCtx, params)
		cancel()
		release()
		if err == nil {
			if usageFn != nil {
				usageFn(role, p.Name(), params.Model, GetTraceID(ctx), resp.Usage, time.Since(started).Seconds())
			}
			return resp, nil
		}
		lastErr = err
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if attempt == config.LlmMaxRetries {
			return nil, err
		}
		delay := backoffDelay(attempt, isRateLimitError(err))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, lastErr
}

func streamWithRetry(ctx context.Context, p Provider, params llm.CallParams, sink StreamSink, throttle *RoleThrottle, usageFn UsageRecorder, role string) (*llm.StreamResult, error) {
	if p == nil {
		return nil, fmt.Errorf("no active LLM provider")
	}
	var lastErr error
	for attempt := 0; attempt <= config.LlmMaxRetries; attempt++ {
		release, err := throttle.Acquire(ctx, role)
		if err != nil {
			return nil, err
		}
		started := time.Now()
		attemptCtx, cancel := context.WithTimeout(ctx, config.LlmCallTimeout)
		sr, err := p.Stream(attemptCtx, params, sink)
		cancel()
		release()
		if err == nil {
			if usageFn != nil {
				usageFn(role, p.Name(), params.Model, GetTraceID(ctx), sr.Usage, time.Since(started).Seconds())
			}
			return sr, nil
		}
		lastErr = err
		if sr != nil && sr.Content != "" {
			if usageFn != nil {
				usageFn(role, p.Name(), params.Model, GetTraceID(ctx), sr.Usage, time.Since(started).Seconds())
			}
			return sr, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if attempt == config.LlmMaxRetries {
			return nil, err
		}
		delay := backoffDelay(attempt, isRateLimitError(err))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, lastErr
}
