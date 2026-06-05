package llm

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestRetryAfterFromError_JSON(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want time.Duration
	}{
		{
			name: "retry_after integer seconds",
			err:  errors.New(`429 Too Many Requests {"code":"1302","message":"slow down","retry_after":30}`),
			want: 30 * time.Second,
		},
		{
			name: "retry_after with whitespace",
			err:  errors.New(`{"retry_after":  5}`),
			want: 5 * time.Second,
		},
		{
			name: "retry_after as decimal",
			err:  errors.New(`{"retry_after":2.5}`),
			want: 2500 * time.Millisecond,
		},
		{
			name: "no hint",
			err:  errors.New("connection reset"),
			want: 0,
		},
		{
			name: "nil error",
			err:  nil,
			want: 0,
		},
		{
			name: "retry_after is zero -> ignored",
			err:  errors.New(`{"retry_after":0}`),
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := retryAfterFromError(tc.err)
			if got != tc.want {
				t.Errorf("retryAfterFromError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRetryAfterFromError_HeaderSeconds(t *testing.T) {
	err := errors.New("HTTP/1.1 429 Too Many Requests\nRetry-After: 12\nContent-Length: 0\n")
	got := retryAfterFromError(err)
	want := 12 * time.Second
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestRetryAfterFromError_HeaderHTTPDate(t *testing.T) {
	target := time.Now().Add(20 * time.Second).UTC().Format(time.RFC1123)
	err := fmt.Errorf("429 Too Many Requests\nRetry-After: %s\n", target)
	got := retryAfterFromError(err)
	if got <= 10*time.Second || got > 25*time.Second {
		t.Errorf("HTTP-date Retry-After = %v, want ~20s", got)
	}
}

func TestErrSnippet(t *testing.T) {
	long := make([]byte, 0, 1000)
	for i := 0; i < 1000; i++ {
		long = append(long, 'x')
	}
	long = append(long, []byte(`{"code":"1302"}`)...)
	err := errors.New(string(long))
	got := errSnippet(err, 80)
	if len(got) > 80+5 { // " ... " is 5 chars
		t.Errorf("snippet too long: %d chars", len(got))
	}
	// Both head (xxx...) and tail (1302) must be visible.
	if got[:5] != "xxxxx" {
		t.Errorf("head missing: %q", got[:10])
	}
	if !contains(got, "1302") {
		t.Errorf("tail missing: %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestLimiterAcquireRelease verifies the concurrency cap actually
// blocks a third caller until a slot is freed. We override the
// singleton with a fresh limiter to keep the test self-contained.
func TestLimiterAcquireRelease(t *testing.T) {
	l := &llmLimiter{
		sem: make(chan struct{}, 2),
	}
	ctx := t.Context()

	r1, err := l.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	r2, err := l.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}

	// 3rd Acquire must block while sem is full.
	done := make(chan struct{})
	go func() {
		r3, err := l.Acquire(ctx)
		if err == nil {
			r3()
		}
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("Acquire(3) returned before slot freed")
	case <-time.After(50 * time.Millisecond):
	}

	r1() // release one slot, the blocked goroutine should proceed.
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Acquire(3) did not unblock after release")
	}
	r2()
}

// TestLimiterCancellation: Acquire respects ctx cancellation while
// waiting for a concurrency slot, and the returned release is a no-op.
func TestLimiterCancellation(t *testing.T) {
	l := &llmLimiter{sem: make(chan struct{}, 1)}
	ctx := t.Context()

	r1, _ := l.Acquire(ctx)
	defer r1()

	cancelCtx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	r2, err := l.Acquire(cancelCtx)
	if err == nil {
		r2()
		t.Fatal("expected ctx error, got nil")
	}
	// Calling the no-op release must not panic and must not free a slot
	// (the slot was never taken).
	r2()
}

// TestLimiterNilSafe: Acquire on a nil limiter is a no-op success.
// This guards the code path where LLM_MAX_QPS=0 and LLM_MAX_CONCURRENCY=0.
func TestLimiterNilSafe(t *testing.T) {
	var l *llmLimiter
	r, err := l.Acquire(t.Context())
	if err != nil {
		t.Fatalf("nil limiter Acquire: %v", err)
	}
	r() // must not panic
}
