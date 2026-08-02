package event

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"unicode"

	"go-code-agent/internal/llm"
	"go-code-agent/internal/logging"
	"go-code-agent/internal/store"
	"go-code-agent/internal/utils"
)

// ConsoleSink renders concise, user-facing event summaries to stderr.
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

	// Model retry wait / provider fallback — keep the user informed during
	// backoff instead of showing a silent terminal.
	if e.Type == ModelRetry {
		mp, _ := e.Payload.(map[string]string)
		fmt.Fprintf(os.Stderr, "%s[retry]%s %s\n", utils.BoldYellow, utils.Reset, mp["summary"])
		return
	}

	label := decisionLabel(e.Type)
	color := decisionColor(e.Type)
	payload, _ := e.Payload.(map[string]string)
	var preview []string
	previewOmitted := 0

	if e.ToolName != "" {
		action := string(e.Type)
		switch e.Type {
		case ToolStarted:
			action = "started"
		case ToolFinished:
			action = e.Status
			if action == "" {
				action = "finished"
			}
		}
		fmt.Fprintf(os.Stderr, "%s[%s]%s %s%s%s",
			utils.BoldYellow, e.ToolName, utils.Reset, color, action, utils.Reset)
	} else {
		fmt.Fprintf(os.Stderr, "%s[%s]%s", color, label, utils.Reset)
	}
	// The active terminal already identifies the session. Keep agent identity
	// only for concurrent subagents and teammates, where it disambiguates output.
	if e.AgentID != "" && e.AgentID != "lead" {
		fmt.Fprintf(os.Stderr, " agent=%s", e.AgentID)
	}
	if path := payload["path"]; path != "" {
		fmt.Fprintf(os.Stderr, " path=%s", consoleLine(path))
	}
	if pattern := payload["pattern"]; pattern != "" {
		fmt.Fprintf(os.Stderr, " pattern=%q", consoleLine(pattern))
	}
	if offset := payload["offset"]; offset != "" {
		fmt.Fprintf(os.Stderr, " offset=%s", offset)
	}
	if limit := payload["limit"]; limit != "" {
		fmt.Fprintf(os.Stderr, " limit=%s", limit)
	}
	if e.Status != "" && e.Type != ToolFinished {
		fmt.Fprintf(os.Stderr, " status=%s", e.Status)
	}
	if e.Error != "" {
		fmt.Fprintf(os.Stderr, " %serror=%s%s", utils.Red, consoleLine(e.Error), utils.Reset)
	}
	if e.Duration > 0 {
		fmt.Fprintf(os.Stderr, " dur=%.2fs", e.Duration.Seconds())
	}
	if e.Type == ToolFinished {
		switch {
		case e.Status != "succeeded" && e.Output != "":
			if e.Error == "" {
				fmt.Fprintf(os.Stderr, " %serror=%s%s", utils.Red, consoleLine(e.Output), utils.Reset)
			}
		case e.Status == "succeeded" && consoleResultLabel(e.ToolName) != "":
			label := consoleResultLabel(e.ToolName)
			var total int
			if isTaskStateTool(e.ToolName) {
				preview, total = consoleFullToolOutput(e.Output)
			} else {
				preview, total = consoleToolPreview(e.ToolName, e.Output, 8)
				previewOmitted = total - len(preview)
			}
			fmt.Fprintf(os.Stderr, " %s=%d", label, total)
		case e.Status == "succeeded" && e.Output != "" && len(e.Output) <= 240:
			fmt.Fprintf(os.Stderr, " %s", consoleLine(e.Output))
		case e.Status == "succeeded" && e.Output != "":
			fmt.Fprintf(os.Stderr, " output=%dB", len(e.Output))
		}
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
		fmt.Fprintf(os.Stderr, " %sin=%d out=%d reasoning=%d hit=%d miss=%d hit_rate=%.1f%%%s",
			utils.Dim, e.Usage.PromptTokens, e.Usage.CompletionTokens,
			e.Usage.ReasoningTokens, e.Usage.CachedReadTokens, e.Usage.CacheMissTokens,
			cacheHitRate(*e.Usage), utils.Reset)
	}
	fmt.Fprintln(os.Stderr)
	for _, line := range preview {
		fmt.Fprintf(os.Stderr, "%s  %s%s\n", utils.Dim, line, utils.Reset)
	}
	if previewOmitted > 0 {
		fmt.Fprintf(os.Stderr, "%s  ... %d more lines%s\n", utils.Dim, previewOmitted, utils.Reset)
	}
}

func consoleLine(text string) string {
	return utils.Truncate(consoleText(strings.Join(strings.Fields(text), " ")), 240)
}

func consoleResultLabel(toolName string) string {
	switch toolName {
	case "read_file":
		return "lines"
	case "list_dir":
		return "entries"
	case "search_file", "search_content":
		return "matches"
	case "TodoWrite":
		return "items"
	case "task_dag":
		return "nodes"
	case "task_create", "task_list", "task_update", "task_get",
		"task_ready", "claim_task", "task_add_dep", "task_remove_dep":
		return "result"
	default:
		return ""
	}
}

func isTaskStateTool(toolName string) bool {
	switch toolName {
	case "TodoWrite", "task_dag", "task_create", "task_list", "task_update",
		"task_get", "task_ready", "claim_task", "task_add_dep", "task_remove_dep":
		return true
	default:
		return false
	}
}

func consoleFullToolOutput(text string) ([]string, int) {
	text = strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if text == "" {
		return nil, 0
	}
	lines := strings.Split(text, "\n")
	output := make([]string, 0, len(lines))
	for _, line := range lines {
		output = append(output, consoleText(line))
	}
	return output, len(lines)
}

func consoleToolPreview(toolName, text string, maxLines int) ([]string, int) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" ||
		(toolName == "search_file" && trimmed == "No files matched.") ||
		(toolName == "search_content" && trimmed == "No matches found.") {
		return nil, 0
	}
	return consolePreview(text, maxLines)
}

func consolePreview(text string, maxLines int) ([]string, int) {
	text = strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if text == "" {
		return nil, 0
	}
	lines := strings.Split(text, "\n")
	limit := min(len(lines), maxLines)
	preview := make([]string, 0, limit)
	for _, line := range lines[:limit] {
		preview = append(preview, utils.Truncate(consoleText(line), 180))
	}
	return preview, len(lines)
}

func consoleText(text string) string {
	text = strings.ReplaceAll(text, "\t", "    ")
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
}

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

// SessionLogSink appends structured events to a JSONL file for session replay.
type SessionLogSink struct {
	mu   sync.Mutex
	path string
	f    *os.File
}

func NewSessionLogSink(path string) (*SessionLogSink, error) {
	s := &SessionLogSink{path: path}
	if err := s.reopen(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SessionLogSink) reopen() error {
	f, err := store.OpenPrivateAppend(s.path)
	if err != nil {
		return err
	}
	if s.f != nil {
		_ = s.f.Close()
	}
	s.f = f
	return nil
}

func (s *SessionLogSink) Emit(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Keep structured task state complete so session replay can recover the
	// current checklist and DAG. Other tool output remains capped to avoid
	// allowing large file reads to grow session.log without bound.
	if !isTaskStateTool(e.ToolName) && len(e.Output) > 4000 {
		e.Output = e.Output[:4000] + "\n... (truncated for session.log)"
	}
	data, err := e.MarshalJSON()
	if err != nil {
		logging.Default().Error(fmt.Sprintf("session log marshal failed for %s: %v", e.Type, err))
		return
	}
	line := append(data, '\n')
	// Dir wipe leaves an open FD writing to an unlinked inode — detect via Stat.
	if s.f == nil {
		if err := s.reopen(); err != nil {
			logging.Default().Error(fmt.Sprintf("session log reopen failed: %v", err))
			return
		}
	} else if _, err := os.Stat(s.path); err != nil {
		if err := s.reopen(); err != nil {
			logging.Default().Error(fmt.Sprintf("session log reopen failed: %v", err))
			return
		}
	}
	if _, writeErr := s.f.Write(line); writeErr != nil {
		if err := s.reopen(); err != nil {
			logging.Default().Error(fmt.Sprintf("session log write failed: %v; reopen failed: %v", writeErr, err))
			return
		}
		if _, err := s.f.Write(line); err != nil {
			logging.Default().Error(fmt.Sprintf("session log retry write failed: %v", err))
		}
	}
}

func (s *SessionLogSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}
