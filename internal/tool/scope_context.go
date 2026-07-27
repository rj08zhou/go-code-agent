package tool

import (
	"context"
	"time"
)

// ScopeContext returns the per-invocation context set by Executor
// (timeout + cancellation). Handlers that perform I/O MUST use this instead of
// context.Background(), or Executor timeouts cannot abort the underlying work.
//
// Falls back to Background only for direct handler calls outside Executor
// (unit tests / REPL helpers that omit ToolScope.Context).
func ScopeContext(scope *ToolScope) context.Context {
	if scope != nil && scope.Context != nil {
		return scope.Context
	}
	return context.Background()
}

// scopeParentContext is the historical name used by builtin handlers.
func scopeParentContext(scope *ToolScope) context.Context { return ScopeContext(scope) }

// timeRemaining reports how much wall time is left on ctx's deadline.
// ok is false when the context has no deadline.
func timeRemaining(ctx context.Context) (time.Duration, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, false
	}
	return time.Until(deadline), true
}
