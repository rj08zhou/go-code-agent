package logging

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
)

// ANSI color codes — kept exported because some callers (and tests)
// reach for them directly. New code should NOT format with these
// manually; emit through Print* helpers and the ConsoleHandler decides
// whether to apply color based on the destination writer.
const (
	ColorReset   = "\033[0m"
	ColorGreen   = "\033[32m"
	ColorYellow  = "\033[33m"
	ColorCyan    = "\033[36m"
	ColorRed     = "\033[31m"
	ColorWhite   = "\033[97m"
	ColorBold    = "\033[1m"
	ColorDim     = "\033[90m"
	ColorMagenta = "\033[35m"
)

// ---- logger plumbing ----------------------------------------------------
//
// The package keeps two slog handlers: one for the terminal (always on,
// color-aware via ConsoleHandler), one optional file handler installed
// by the cmd layer once a session is bootstrapped. They are combined
// through a MultiHandler so every Print* call hits both sinks.
//
// The active logger is held in an atomic.Pointer so swapping sinks
// (e.g. on session switch) is lock-free for hot path reads.

var (
	consoleHandler slog.Handler = NewConsoleHandler(os.Stdout)
	fileHandler    atomic.Pointer[slog.Handler]
	loggerPtr      atomic.Pointer[slog.Logger]
	rebuildMu      sync.Mutex
)

func init() {
	rebuildLogger()
}

// rebuildLogger reconstructs the active slog.Logger from the current
// console + file handler pair and atomically publishes it.
func rebuildLogger() {
	rebuildMu.Lock()
	defer rebuildMu.Unlock()

	handlers := []slog.Handler{consoleHandler}
	if fh := fileHandler.Load(); fh != nil {
		handlers = append(handlers, *fh)
	}
	logger := slog.New(NewMultiHandler(handlers...))
	loggerPtr.Store(logger)
}

// logger returns the currently-installed slog.Logger.
func logger() *slog.Logger {
	return loggerPtr.Load()
}

// SetFileHandler installs (or clears, when h is nil) the slog handler
// receiving every Print* call in addition to the console. Typical use
// is slog.NewJSONHandler(file, nil) so records become JSONL.
//
// The cmd layer calls this once per session bootstrap; subsequent
// session switches re-call it with a handler bound to the new file.
func SetFileHandler(h slog.Handler) {
	if h == nil {
		fileHandler.Store(nil)
	} else {
		fileHandler.Store(&h)
	}
	rebuildLogger()
}

// ---- streaming agent buffer --------------------------------------------
// PrintAgentBegin/Delta/End run sequentially during streaming. We
// buffer the full text so we can emit one slog record on End — the
// console already saw the live characters via direct stdout writes.
var agentBuf []byte

// ---- Print* helpers ----------------------------------------------------

// Kind* constants are the single source of truth for the "kind" attr
// stamped on every slog record by the Print* functions below. KnownKinds
// mirrors them in the preferred display order for consumers building a
// "by kind" summary. Every Print* below uses one of these constants
// directly, and KnownKinds is derived from the same set, so the two
// can no longer disagree.
const (
	KindAgent    = "agent"
	KindTool     = "tool"
	KindTeamTool = "team_tool"
	KindSubTool  = "sub_tool"
	KindSystem   = "system"
	KindError    = "error"
	KindDecision = "decision"
)

// KnownKinds lists every Kind* constant in preferred display order.
var KnownKinds = []string{
	KindAgent, KindTool, KindTeamTool, KindSubTool, KindSystem, KindError, KindDecision,
}

// PrintAgentBegin starts streaming output: emit the green ANSI prefix
// directly and reset the buffer. ConsoleHandler is bypassed because
// per-token rendering is incompatible with slog's record-oriented model.
func PrintAgentBegin() {
	fmt.Print(ColorGreen)
	agentBuf = nil
}

// PrintAgentDelta prints one streaming chunk verbatim and accumulates
// it for the End-time record.
func PrintAgentDelta(delta string) {
	fmt.Print(delta)
	agentBuf = append(agentBuf, []byte(delta)...)
}

// PrintAgentEnd closes the streaming run with reset+newline and emits
// one slog record carrying the full text. The "streaming"=true attr
// tells ConsoleHandler to skip terminal output (live chars already
// printed); only the file handler will persist it.
func PrintAgentEnd() {
	fmt.Println(ColorReset)
	full := string(agentBuf)
	logger().LogAttrs(context.Background(), slog.LevelInfo, full,
		slog.String("kind", KindAgent),
		slog.Bool("streaming", true))
	agentBuf = nil
}

// PrintThinkingHeader emits a single dim "深度思考" section marker
// before the model's reasoning text begins streaming. This gives the
// user a visual partition between tool output and the next round of
// reasoning, without changing the color of the reasoning text itself.
func PrintThinkingHeader() {
	fmt.Printf("%s💭 深度思考%s\n", ColorDim, ColorReset)
}

// PrintTool prints a tool execution result (truncated for terminal
// readability — full text still hits the file handler via the message).
func PrintTool(name, output string) {
	logger().LogAttrs(context.Background(), slog.LevelInfo, name+": "+output,
		slog.String("kind", KindTool),
		slog.String("name", name),
		slog.String("output", output))
}

// PrintTeamTool prints a teammate's tool execution result.
func PrintTeamTool(teammate, name, output string) {
	logger().LogAttrs(context.Background(), slog.LevelInfo, "["+teammate+"] "+name+": "+output,
		slog.String("kind", KindTeamTool),
		slog.String("teammate", teammate),
		slog.String("name", name),
		slog.String("output", output))
}

// PrintSubTool prints a subagent's tool execution result.
func PrintSubTool(name, output string) {
	logger().LogAttrs(context.Background(), slog.LevelInfo, "[subagent] "+name+": "+output,
		slog.String("kind", KindSubTool),
		slog.String("name", name),
		slog.String("output", output))
}

// PrintSystem prints an informational system message.
func PrintSystem(msg string) {
	logger().LogAttrs(context.Background(), slog.LevelInfo, msg,
		slog.String("kind", KindSystem))
}

// PrintError prints an error message. We keep stderr as the visible
// destination by writing directly there in addition to the slog path,
// because ConsoleHandler points at stdout and shell redirection
// expectations (2>err.log) must keep working.
func PrintError(msg string) {
	fmt.Fprintf(os.Stderr, "%sError: %s%s\n", ColorRed, msg, ColorReset)
	// Mark streaming=true so ConsoleHandler does not double-print to
	// stdout; only the file handler records the structured line.
	logger().LogAttrs(context.Background(), slog.LevelError, msg,
		slog.String("kind", KindError),
		slog.Bool("streaming", true))
}

// ---- decisions ---------------------------------------------------------

var decisionSink func(kind, summary string)

// SetDecisionSink installs a callback invoked for every PrintDecision
// call (in addition to the regular slog path). Used by the cmd layer
// to persist decisions.jsonl for replay.
func SetDecisionSink(fn func(kind, summary string)) {
	decisionSink = fn
}

// PrintDecision prints an autonomous-decision event. kind is a
// finer-grained tag (e.g. "plan", "judge", ...) scoped to decisions
// specifically - see cmd/agent/decisions.go's Decision* constants for
// the values this package's callers actually pass.
func PrintDecision(kind, summary string) {
	logger().LogAttrs(context.Background(), slog.LevelInfo, summary,
		slog.String("kind", KindDecision),
		slog.String("tag", kind))
	if decisionSink != nil {
		decisionSink(kind, summary)
	}
}

// ---- helpers -----------------------------------------------------------
