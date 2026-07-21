package event

import (
	"fmt"
	"go-code-agent-refactor/internal/llm"
	"go-code-agent-refactor/internal/logging"
	"go-code-agent-refactor/internal/utils"
	"os"
	"sync"
)

// ConsoleSink logs events to stdout for debugging.
type ConsoleSink struct {
	mu sync.Mutex
}

func NewConsoleSink() *ConsoleSink { return &ConsoleSink{} }

func decisionLabel(et EventType) string {
	switch et {
	case PlanningDecision:
		return "planning"
	case ContextDecision:
		return "context"
	case TurnComplete:
		return "turn_complete"
	case MemoryDecision:
		return "memory"
	case JudgeDecision:
		return "judge"
	case ReflectionTriggered:
		return "reflection"
	default:
		return string(et)
	}
}

func decisionColor(et EventType) string {
	switch et {
	case AgentStarted:
		return utils.BoldGreen
	case ModelCalled:
		return utils.BoldYellow
	case ToolStarted, ToolFinished, TurnDecision:
		return utils.Cyan
	case ReflectionTriggered:
		return utils.BoldMagenta
	case PlanningDecision:
		return utils.BoldMagenta
	case ContextDecision:
		return utils.Dim
	case TurnComplete:
		return utils.BoldGreen
	case MemoryDecision:
		return utils.BoldGreen
	case JudgeDecision:
		return utils.BoldYellow
	default:
		return utils.Dim
	}
}

func (s *ConsoleSink) Emit(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Per-turn summary — prominent standalone line.
	if e.Type == TurnDecision {
		mp, _ := e.Payload.(map[string]string)
		fmt.Fprintf(os.Stderr, "%s--- turn %s ---%s tools=%s failures=%s\n",
			utils.BoldCyan, mp["round"], utils.Reset, mp["tool_calls"], mp["failures"])
		return
	}

	label := decisionLabel(e.Type)
	color := decisionColor(e.Type)

	if e.ToolName != "" {
		fmt.Fprintf(os.Stderr, "%s[%s]%s %s%s%s | agent=%s session=%s",
			utils.BoldYellow, e.ToolName, utils.Reset,
			color, e.Type, utils.Reset, e.AgentID, e.SessionID)
	} else {
		fmt.Fprintf(os.Stderr, "%s[%s]%s %s%s%s | agent=%s session=%s",
			utils.Dim, label, utils.Reset,
			color, e.Type, utils.Reset, e.AgentID, e.SessionID)
	}
	if e.Error != "" {
		fmt.Fprintf(os.Stderr, " %serror=%s%s", utils.Red, e.Error, utils.Reset)
	}
	if e.Duration > 0 {
		fmt.Fprintf(os.Stderr, " dur=%.2fs", e.Duration.Seconds())
	}
	if e.Output != "" {
		fmt.Fprintf(os.Stderr, "\n%s  %s%s", utils.Dim, utils.Truncate(e.Output, 2000), utils.Reset)
	}
	// Print payload fields for decision events (matches original verbose output).
	if mp, ok := e.Payload.(map[string]string); ok && len(mp) > 0 {
		// Reflection kind.
		if k := mp["kind"]; k != "" && e.Type == ReflectionTriggered {
			fmt.Fprintf(os.Stderr, " %skind=%s%s", utils.BoldMagenta, k, utils.Reset)
		}
		if s := mp["action"]; s != "" {
			fmt.Fprintf(os.Stderr, " %s%s%s", utils.Dim, s, utils.Reset)
		}
		if s := mp["summary"]; s != "" {
			fmt.Fprintf(os.Stderr, " %s", s)
		}
		if r := mp["rounds"]; r != "" {
			fmt.Fprintf(os.Stderr, " %srounds=%s%s", utils.Dim, r, utils.Reset)
		}
		if cf := mp["consecutive_fails"]; cf != "" {
			fmt.Fprintf(os.Stderr, " %sconsecutive_fails=%s%s", utils.Dim, cf, utils.Reset)
		}
		if p := mp["prompt_count"]; p != "" {
			fmt.Fprintf(os.Stderr, " %sprompts=%s%s", utils.Dim, p, utils.Reset)
		}
		if sc := mp["score"]; sc != "" {
			fmt.Fprintf(os.Stderr, " %sscore=%s%s", utils.Dim, sc, utils.Reset)
		}
		if ap := mp["approved"]; ap != "" {
			fmt.Fprintf(os.Stderr, " %sapproved=%s%s", utils.Dim, ap, utils.Reset)
		}
		if cl := mp["cleared"]; cl != "" {
			fmt.Fprintf(os.Stderr, " %scleared=%s%s", utils.Dim, cl, utils.Reset)
		}
	}
	// Show per-turn accumulated usage.
	if e.Type == TurnComplete && e.Usage != nil && !e.Usage.IsZero() {
		fmt.Fprintf(os.Stderr, " %sin=%d out=%d hit=%d miss=%d hit_rate=%.1f%%%s",
			utils.Dim, e.Usage.PromptTokens, e.Usage.CompletionTokens,
			e.Usage.CachedReadTokens, e.Usage.CacheMissTokens,
			cacheHitRate(*e.Usage), utils.Reset)
	}
	fmt.Fprintln(os.Stderr)
}

// AuditSink writes security-relevant events to the audit log.
type AuditSink struct {
	mu sync.Mutex
}

func NewAuditSink() *AuditSink { return &AuditSink{} }

func (s *AuditSink) Emit(e Event) {
	switch e.Type {
	case ApprovalRequested, ApprovalResolved, ToolFinished:
		s.mu.Lock()
		defer s.mu.Unlock()
		logging.Default().Info(fmt.Sprintf("[audit] %s tool=%s agent=%s session=%s outcome=%s",
			e.Type, e.ToolName, e.AgentID, e.SessionID, e.Outcome))
	}
}

// UsageSink tracks token usage by session/agent.
type UsageSink struct {
	mu sync.Mutex
}

func NewUsageSink() *UsageSink { return &UsageSink{} }

func cacheHitRate(u llm.Usage) float64 {
	input := u.PromptTokens
	if input <= 0 {
		input = u.CachedReadTokens + u.CacheMissTokens
	}
	if input <= 0 {
		return 0
	}
	return float64(u.CachedReadTokens) / float64(input) * 100
}

func (s *UsageSink) Emit(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Per-round detail (every LLM call).
	if e.Type == ModelCalled && e.Usage != nil {
		u := e.Usage
		logging.Default().Info(fmt.Sprintf("[usage] trace=%s agent=%s session=%s model=%s in=%d out=%d total=%d cached_read=%d cache_miss=%d cache_create=%d hit_rate=%.1f%% dur=%.2fs",
			e.TraceID, e.AgentID, e.SessionID, e.ModelID,
			u.PromptTokens, u.CompletionTokens, u.TotalTokens,
			u.CachedReadTokens, u.CacheMissTokens, u.CacheCreateTokens,
			cacheHitRate(*u), e.Duration.Seconds()))
		return
	}

	// Per-turn summary (one user conversation).
	if e.Type == TurnComplete && e.Usage != nil && !e.Usage.IsZero() {
		u := e.Usage
		logging.Default().Info(fmt.Sprintf("[usage:turn] trace=%s agent=%s session=%s %s in=%d out=%d total=%d cached_read=%d cache_miss=%d cache_create=%d hit_rate=%.1f%%",
			e.TraceID, e.AgentID, e.SessionID,
			e.Payload.(map[string]string)["summary"],
			u.PromptTokens, u.CompletionTokens, u.TotalTokens,
			u.CachedReadTokens, u.CacheMissTokens, u.CacheCreateTokens,
			cacheHitRate(*u)))
	}
}

// SessionLogSink appends structured events to a JSONL file for session replay.
type SessionLogSink struct {
	mu sync.Mutex
	f  *os.File
}

func NewSessionLogSink(path string) (*SessionLogSink, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &SessionLogSink{f: f}, nil
}

func (s *SessionLogSink) Emit(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Cap output to prevent session.log blow-up from large file reads.
	if len(e.Output) > 4000 {
		e.Output = e.Output[:4000] + "\n... (truncated for session.log)"
	}
	data, err := e.MarshalJSON()
	if err != nil {
		return
	}
	s.f.Write(append(data, '\n'))
}

func (s *SessionLogSink) Close() error {
	return s.f.Close()
}
