package task

import (
	"fmt"
	"strings"
	"sync"
)

// TodoItem is the short-lived turn checklist shown by /tasks.
type TodoItem struct {
	Content    string
	Status     string
	ActiveForm string
}

// TodoManager stores the current session's TodoWrite checklist.
// It is intentionally separate from Service: persistent DAG tasks and the
// model-facing checklist are two different task systems.
type TodoManager struct {
	mu    sync.RWMutex
	items []TodoItem
}

func (t *TodoManager) Update(items []map[string]string) (string, error) {
	if len(items) > 20 {
		return "", fmt.Errorf("max 20 todos")
	}
	validated := make([]TodoItem, 0, len(items))
	inProgress := 0
	for i, raw := range items {
		content := strings.TrimSpace(raw["content"])
		status := strings.ToLower(strings.TrimSpace(raw["status"]))
		activeForm := strings.TrimSpace(raw["activeForm"])
		if content == "" {
			return "", fmt.Errorf("item %d: content required", i)
		}
		if status != "pending" && status != "in_progress" && status != "completed" {
			return "", fmt.Errorf("item %d: invalid status", i)
		}
		if activeForm == "" {
			return "", fmt.Errorf("item %d: activeForm required", i)
		}
		if status == "in_progress" {
			inProgress++
		}
		validated = append(validated, TodoItem{Content: content, Status: status, ActiveForm: activeForm})
	}
	if inProgress > 1 {
		return "", fmt.Errorf("only one in_progress allowed")
	}
	t.mu.Lock()
	t.items = validated
	t.mu.Unlock()
	return t.Render(), nil
}

func (t *TodoManager) Render() string {
	t.mu.RLock()
	items := append([]TodoItem(nil), t.items...)
	t.mu.RUnlock()
	if len(items) == 0 {
		return "No todos."
	}
	markers := map[string]string{"completed": "[x]", "in_progress": "[>]", "pending": "[ ]"}
	lines := make([]string, 0, len(items)+1)
	done := 0
	for _, item := range items {
		line := markers[item.Status] + " " + item.Content
		if item.Status == "in_progress" {
			line += " <- " + item.ActiveForm
		}
		lines = append(lines, line)
		if item.Status == "completed" {
			done++
		}
	}
	lines = append(lines, fmt.Sprintf("\n(%d/%d completed)", done, len(items)))
	return strings.Join(lines, "\n")
}

func (t *TodoManager) HasOpenItems() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, item := range t.items {
		if item.Status != "completed" {
			return true
		}
	}
	return false
}
