package log

import (
	"context"
	"fmt"
	"go-code-agent/utils"
	"io"
	"log/slog"
	"os"
	"sync"
)

// ConsoleHandler is a slog.Handler that renders records to a terminal
// with color based on the "kind" attribute (agent/tool/team_tool/
// sub_tool/system/error/decision), not on slog.Level. This matches the
// project's pre-existing visual language: each message category has a
// dedicated color and prefix.
//
// Records carrying attribute "streaming"=true are skipped: those are the
// post-stream summary written for the file sink only, since the live
// streaming output was already painted character-by-character through
// the dedicated PrintAgentBegin/Delta/End fast path.
type ConsoleHandler struct {
	mu    *sync.Mutex
	out   io.Writer
	color bool
}

// NewConsoleHandler returns a ConsoleHandler writing to w. If w is a
// terminal, ANSI colors are emitted; otherwise output is plain text.
func NewConsoleHandler(w io.Writer) *ConsoleHandler {
	return &ConsoleHandler{
		mu:    &sync.Mutex{},
		out:   w,
		color: isTerminal(w),
	}
}

// isTerminal best-effort detects whether w is an interactive TTY. We
// only check the os.File case; anything else (buffers, pipes captured
// for tests) gets plain text — which is what tests actually want.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// Enabled accepts every level: filtering by category is the caller's
// responsibility (Print* helpers always emit at Info or Error).
func (h *ConsoleHandler) Enabled(_ context.Context, _ slog.Level) bool {
	return true
}

// Handle renders one record. Layout differs per kind to preserve the
// look established by the original Print* helpers.
func (h *ConsoleHandler) Handle(_ context.Context, r slog.Record) error {
	var (
		kind      string
		streaming bool
		name      string
		teammate  string
		output    string
	)
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "kind":
			kind = a.Value.String()
		case "streaming":
			streaming = a.Value.Bool()
		case "name":
			name = a.Value.String()
		case "teammate":
			teammate = a.Value.String()
		case "output":
			output = a.Value.String()
		}
		return true
	})

	// Streaming summaries are file-only; the live characters were
	// printed by the streaming fast path already.
	if streaming {
		return nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	switch kind {
	case "agent":
		fmt.Fprintf(h.out, "%s%s%s\n", h.c(ColorGreen), r.Message, h.c(ColorReset))
	case "tool":
		fmt.Fprintf(h.out, "%s> %s%s: %s\n",
			h.c(ColorYellow), name, h.c(ColorReset), utils.Truncate(output, 200))
	case "team_tool":
		fmt.Fprintf(h.out, "  %s[%s] %s%s: %s\n",
			h.c(ColorDim), teammate, name, h.c(ColorReset), utils.Truncate(output, 120))
	case "sub_tool":
		fmt.Fprintf(h.out, "  %s[subagent] %s%s: %s\n",
			h.c(ColorCyan), name, h.c(ColorReset), utils.Truncate(output, 120))
	case "system":
		fmt.Fprintf(h.out, "%s  %s%s\n", h.c(ColorDim), r.Message, h.c(ColorReset))
	case "error":
		fmt.Fprintf(h.out, "%sError: %s%s\n", h.c(ColorRed), r.Message, h.c(ColorReset))
	case "decision":
		// r.Message carries the summary; the kind tag (plan/context/...)
		// arrives as attr "tag".
		var tag string
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "tag" {
				tag = a.Value.String()
				return false
			}
			return true
		})
		fmt.Fprintf(h.out, "%s%s◆ [%s]%s %s\n",
			h.c(ColorBold), h.c(ColorMagenta), tag, h.c(ColorReset), r.Message)
	default:
		// Unknown kind: print plain so we never lose a message.
		fmt.Fprintln(h.out, r.Message)
	}
	return nil
}

// c returns the ANSI escape if colors are enabled, "" otherwise.
func (h *ConsoleHandler) c(code string) string {
	if h.color {
		return code
	}
	return ""
}

// WithAttrs / WithGroup are unused by Print* helpers but required by
// the slog.Handler contract. We keep both as no-ops since attributes
// flow exclusively per-record through Print* call sites.
func (h *ConsoleHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *ConsoleHandler) WithGroup(_ string) slog.Handler      { return h }
