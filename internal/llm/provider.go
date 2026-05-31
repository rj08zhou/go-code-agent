package llm

import (
	"errors"
	"fmt"
	"go-code-agent/internal/log"
	"os"
	"strings"
)

// Provider selection and registry.
//
// Current backends: openai (full), anthropic (full), gemini (stub).
// Selection: LLM_PROVIDER env > MODEL_ID prefix inference > fallback to openai.

// stdoutStreamSink prints streaming output to the terminal.
type stdoutStreamSink struct {
	started bool
}

// NewStdoutStreamSink creates a StreamSink that prints to the terminal.
func NewStdoutStreamSink() StreamSink { return &stdoutStreamSink{} }

func (s *stdoutStreamSink) OnTextDelta(text string) {
	if text == "" {
		return
	}
	if !s.started {
		log.PrintAgentBegin()
		s.started = true
	}
	log.PrintAgentDelta(text)
}

func (s *stdoutStreamSink) OnDone() {
	if s.started {
		log.PrintAgentEnd()
		s.started = false
	}
}

// SilentStreamSink discards all stream events.
type SilentStreamSink struct{}

func (SilentStreamSink) OnTextDelta(string) {}
func (SilentStreamSink) OnDone()            {}

// PrefixedStreamSink prints streaming output with a colored prefix.
// Used by subagent and teammate loops to show real-time LLM output
// without conflicting with the main agent's output.
type PrefixedStreamSink struct {
	Prefix  string // e.g. "  [sub]" or "  [alice]"
	Color   string // ANSI color code (e.g. log.ColorCyan)
	started bool
}

func (s *PrefixedStreamSink) OnTextDelta(text string) {
	if text == "" {
		return
	}
	if !s.started {
		fmt.Print(s.Color + s.Prefix + " ")
		s.started = true
	}
	fmt.Print(text)
}

func (s *PrefixedStreamSink) OnDone() {
	if s.started {
		fmt.Println(log.ColorReset)
		s.started = false
	}
}

// providerRegistry is filled by each provider_*.go file's init(). We
// key by lowercase name to be forgiving of env var casing.
var providerRegistry = map[string]Provider{}

// RegisterProvider adds a provider to the registry. Called by each
// provider_*.go's init() function.
func RegisterProvider(p Provider) {
	if p == nil {
		return
	}
	providerRegistry[strings.ToLower(p.Name())] = p
}

// PickProvider chooses the active backend. Called once from main.
func PickProvider(modelID string) (Provider, error) {
	// 1. explicit override wins.
	if name := strings.ToLower(strings.TrimSpace(os.Getenv("LLM_PROVIDER"))); name != "" {
		if p, ok := providerRegistry[name]; ok {
			return p, nil
		}
		return nil, fmt.Errorf("unknown LLM_PROVIDER=%q (registered: %s)",
			name, strings.Join(registeredNames(), ", "))
	}
	// 2. infer from model id.
	m := strings.ToLower(strings.TrimSpace(modelID))
	switch {
	case strings.HasPrefix(m, "claude-") || strings.HasPrefix(m, "claude."):
		if p, ok := providerRegistry["anthropic"]; ok {
			return p, nil
		}
	case strings.HasPrefix(m, "gemini-") || strings.HasPrefix(m, "gemini."):
		if p, ok := providerRegistry["gemini"]; ok {
			return p, nil
		}
	}
	// 3. fallback to openai.
	if p, ok := providerRegistry["openai"]; ok {
		return p, nil
	}
	return nil, errors.New("no LLM provider registered")
}

func registeredNames() []string {
	out := make([]string, 0, len(providerRegistry))
	for n := range providerRegistry {
		out = append(out, n)
	}
	return out
}
