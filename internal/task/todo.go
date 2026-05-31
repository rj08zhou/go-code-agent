package task

import (
	"fmt"
	"strings"
)

// TodoManager

type TodoItem struct {
	Content    string
	Status     string
	ActiveForm string
}

type TodoManager struct {
	items []TodoItem
}

func (t *TodoManager) Update(items []map[string]string) (string, error) {
	if len(items) > 20 {
		return "", fmt.Errorf("max 20 todos")
	}
	var validated []TodoItem
	inProgress := 0
	for i, raw := range items {
		content := trimStr(raw["content"])
		status := toLowerTrim(raw["status"])
		activeForm := trimStr(raw["activeForm"])
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
		validated = append(validated, TodoItem{content, status, activeForm})
	}
	if inProgress > 1 {
		return "", fmt.Errorf("only one in_progress allowed")
	}
	t.items = validated
	return t.Render(), nil
}

func (t *TodoManager) Render() string {
	if len(t.items) == 0 {
		return "No todos."
	}
	markers := map[string]string{"completed": "[x]", "in_progress": "[>]", "pending": "[ ]"}
	var lines []string
	done := 0
	for _, item := range t.items {
		line := markers[item.Status] + " " + item.Content
		if item.Status == "in_progress" {
			line += " <- " + item.ActiveForm
		}
		lines = append(lines, line)
		if item.Status == "completed" {
			done++
		}
	}
	lines = append(lines, fmt.Sprintf("\n(%d/%d completed)", done, len(t.items)))
	return strings.Join(lines, "\n")
}

func (t *TodoManager) HasOpenItems() bool {
	for _, item := range t.items {
		if item.Status != "completed" {
			return true
		}
	}
	return false
}

func trimStr(s string) string     { return strings.TrimSpace(s) }
func toLowerTrim(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
