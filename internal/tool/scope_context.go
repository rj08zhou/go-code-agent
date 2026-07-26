package tool

import (
	"context"
	"time"
)

// scopeParentContext returns the invocation context for timeouts/cancellation.
// Executor sets ToolScope.Context to the per-call ctx before invoking handlers.
func scopeParentContext(scope *ToolScope) context.Context {
	if scope != nil && scope.Context != nil {
		return scope.Context
	}
	return context.Background()
}

// timeRemaining reports how much wall time is left on ctx's deadline.
// ok is false when the context has no deadline.
func timeRemaining(ctx context.Context) (time.Duration, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, false
	}
	return time.Until(deadline), true
}
