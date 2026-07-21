package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

type DecisionLog struct {
	path string
	mu   sync.Mutex
}

type decisionEntry struct {
	Time   string `json:"time"`
	Tool   string `json:"tool"`
	Action string `json:"action"`
	Reason string `json:"reason"`
	Round  int    `json:"round"`
}

func NewDecisionLog(dir string) (*DecisionLog, error) {
	return &DecisionLog{path: dir + "/decisions.jsonl"}, nil
}

func (d *DecisionLog) Record(tool, action, reason string, round int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e := decisionEntry{
		Time: time.Now().Format(time.RFC3339), Tool: tool,
		Action: action, Reason: reason, Round: round,
	}
	data, _ := json.Marshal(e)
	f, err := os.OpenFile(d.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(data, '\n'))
}

func (d *DecisionLog) Render() string {
	data, err := os.ReadFile(d.path)
	if err != nil || len(data) == 0 {
		return "No decisions recorded."
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	start := len(lines) - 20
	if start < 0 {
		start = 0
	}
	var result []string
	for _, line := range lines[start:] {
		var e decisionEntry
		if json.Unmarshal([]byte(line), &e) == nil {
			result = append(result, fmt.Sprintf("  %s %-15s %-10s %s", e.Time[:19], e.Tool, e.Action, e.Reason))
		}
	}
	if len(result) == 0 {
		return "No valid decisions."
	}
	return strings.Join(result, "\n")
}
