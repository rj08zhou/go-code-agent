package logging

import (
	"context"
	"log/slog"
)

// MultiHandler fans every record out to all wrapped handlers. The
// canonical use is "ConsoleHandler + JSONHandler(file)": one record in,
// one colored line on the terminal AND one structured JSONL line on
// disk, with no duplication at the call site.
//
// Errors from individual handlers are coalesced: the first non-nil
// error is returned, but all handlers are invoked regardless so that a
// transient file-write failure cannot suppress the terminal output.
type MultiHandler struct {
	handlers []slog.Handler
}

// NewMultiHandler wraps zero or more handlers. nil entries are skipped
// so callers can build the slice conditionally (e.g. file handler only
// when a session log path resolves).
func NewMultiHandler(handlers ...slog.Handler) *MultiHandler {
	clean := make([]slog.Handler, 0, len(handlers))
	for _, h := range handlers {
		if h != nil {
			clean = append(clean, h)
		}
	}
	return &MultiHandler{handlers: clean}
}

// Enabled returns true if ANY wrapped handler is enabled at level. We
// over-deliver rather than under-deliver: a record is always considered
// processable as long as one sink wants it.
func (m *MultiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle dispatches r to every wrapped handler whose Enabled() returns
// true for r.Level. Returns the first error encountered, but never
// short-circuits: every interested handler always sees the record.
func (m *MultiHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, h := range m.handlers {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// WithAttrs returns a new MultiHandler whose wrapped handlers each
// carry the additional attrs.
func (m *MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		next[i] = h.WithAttrs(attrs)
	}
	return &MultiHandler{handlers: next}
}

// WithGroup returns a new MultiHandler whose wrapped handlers each
// enter the named group.
func (m *MultiHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		next[i] = h.WithGroup(name)
	}
	return &MultiHandler{handlers: next}
}
