package llm

import (
	"context"
	"fmt"
	"go-code-agent/infra"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Process-wide LLM throttle.
//
// Both the main agent loop and every teammate / subagent call into the
// same Provider through llm.Client. When a reflect step fan-outs 3
// verifiers they each start their own goroutine and would otherwise hit
// the gateway in lockstep, almost guaranteeing 429s.
//
// The limiter offers two coupled bounds:
//
//  1. A token-bucket on QPS (rate.Limiter)        - smooths bursts.
//  2. A weighted-semaphore on in-flight calls     - bounds parallelism.
//
// Both are tuned through env vars at startup, with conservative
// defaults from infra/consts.go. Set LLM_MAX_QPS=0 to disable rate
// limiting entirely (e.g. tests, dedicated endpoints).

type llmLimiter struct {
	rl  *rate.Limiter // nil => no QPS bound
	sem chan struct{} // nil => no concurrency bound
}

var (
	limiterOnce sync.Once
	limiter     *llmLimiter
)

// getLimiter lazily builds the singleton from env vars. We do it once
// so changes mid-run (which would race with running goroutines) are
// rejected by construction.
func getLimiter() *llmLimiter {
	limiterOnce.Do(func() {
		qps := infra.Cfg.LLMMaxQPS
		burst := infra.Cfg.LLMMaxBurst
		conc := infra.Cfg.LLMMaxConcurrency

		l := &llmLimiter{}
		if qps > 0 {
			if burst < 1 {
				burst = 1
			}
			l.rl = rate.NewLimiter(rate.Limit(qps), burst)
		}
		if conc > 0 {
			l.sem = make(chan struct{}, conc)
		}
		limiter = l
	})
	return limiter
}

// Acquire blocks until the limiter admits a new LLM call. The returned
// release function MUST be invoked exactly once when the call (and its
// streaming, if any) completes — even on error — to free the slot.
//
// Cancelling ctx during Acquire returns ctx.Err() and a no-op release
// (so callers can defer release() unconditionally).
func (l *llmLimiter) Acquire(ctx context.Context) (release func(), err error) {
	noop := func() {}
	if l == nil {
		return noop, nil
	}
	// 1. token bucket — shapes the QPS curve.
	if l.rl != nil {
		if err := l.rl.Wait(ctx); err != nil {
			return noop, err
		}
	}
	// 2. concurrency cap — bounds peak parallelism. We grab this AFTER
	//    the token so a queued caller that gets cancelled does not
	//    consume a concurrency slot it can't use.
	if l.sem != nil {
		select {
		case l.sem <- struct{}{}:
		case <-ctx.Done():
			return noop, ctx.Err()
		}
		var once sync.Once
		return func() { once.Do(func() { <-l.sem }) }, nil
	}
	return noop, nil
}

// 429 helpers — Retry-After parsing.
//
// Most upstream LLM gateways set Retry-After on their 429 response;
// when present it is far more reliable than client-side exponential
// backoff. We extract it from the raw error message because the
// existing provider plumbing only returns errors as text. Two formats
// are accepted (per RFC 7231 §7.1.3):
//
//   - delta-seconds:  "60"
//   - HTTP-date:      "Wed, 21 Oct 2026 07:28:00 GMT"
//
// The error payload from BigModel/智谱 looks like:
//
//	... 429 Too Many Requests {"code":"1302","message":"...","retry_after":30}
//
// so we additionally parse a JSON-ish "retry_after":<n> in the body.

// retryAfterFromError returns the suggested wait duration encoded in
// err's message, or 0 if none is present / parsable. Callers should
// take max(backoff, retryAfterFromError(err)) so we never retry sooner
// than the gateway told us to.
func retryAfterFromError(err error) time.Duration {
	if err == nil {
		return 0
	}
	msg := err.Error()
	// 1. JSON-ish "retry_after":<n>  (BigModel / some OpenAI-compat gateways)
	if d := parseRetryAfterJSON(msg); d > 0 {
		return d
	}
	// 2. HTTP "Retry-After: <value>"  (header echoed in the error body)
	return parseRetryAfterHeader(msg)
}

func parseRetryAfterJSON(msg string) time.Duration {
	const key = `"retry_after"`
	i := strings.Index(msg, key)
	if i < 0 {
		return 0
	}
	rest := msg[i+len(key):]
	// Skip whitespace and ':'
	for len(rest) > 0 && (rest[0] == ' ' || rest[0] == ':') {
		rest = rest[1:]
	}
	// Read digits / decimals.
	end := 0
	for end < len(rest) && (rest[end] == '.' || (rest[end] >= '0' && rest[end] <= '9')) {
		end++
	}
	if end == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(rest[:end], 64)
	if err != nil || v <= 0 {
		return 0
	}
	return time.Duration(v * float64(time.Second))
}

func parseRetryAfterHeader(msg string) time.Duration {
	const key = "Retry-After:"
	i := strings.Index(msg, key)
	if i < 0 {
		return 0
	}
	rest := strings.TrimSpace(msg[i+len(key):])
	// Cut at the first newline only — HTTP-date values legitimately
	// contain ',' (e.g. "Wed, 21 Oct 2026 07:28:00 GMT") so we cannot
	// use ',' as a terminator.
	end := 0
	for end < len(rest) && rest[end] != '\n' && rest[end] != '\r' {
		end++
	}
	val := strings.TrimSpace(rest[:end])
	if val == "" {
		return 0
	}
	// delta-seconds (integer)
	if n, err := strconv.Atoi(val); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	// HTTP-date — try common formats.
	for _, layout := range []string{time.RFC1123, time.RFC1123Z, time.RFC850, time.ANSIC} {
		if t, err := time.Parse(layout, val); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
			return 0
		}
	}
	// Fallback: net/http's parser handles a few legacy layouts.
	if t, err := http.ParseTime(val); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// DescribeLimiter returns a human-readable summary of the active
// throttle. Useful for startup diagnostics.
func DescribeLimiter() string {
	l := getLimiter()
	if l == nil || (l.rl == nil && l.sem == nil) {
		return "disabled"
	}
	parts := []string{}
	if l.rl != nil {
		parts = append(parts, fmt.Sprintf("qps=%.2f burst=%d", float64(l.rl.Limit()), l.rl.Burst()))
	}
	if l.sem != nil {
		parts = append(parts, fmt.Sprintf("concurrency=%d", cap(l.sem)))
	}
	return strings.Join(parts, " ")
}
