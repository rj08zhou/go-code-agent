package background

import (
	"context"
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/utils"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// BackgroundManager - goroutine execution + notification queue

// BashValidator is a function that validates a bash command.
// Returns (allowed, needConfirm, reason).
type BashValidator func(command string) (bool, bool, string)

type bgTask struct{ Status, Command, Result string }

type BackgroundManager struct {
	mu            sync.Mutex
	tasks         map[string]*bgTask
	queue         []map[string]any
	counter       int
	workdir       string
	bashValidator BashValidator
}

func NewBgMgr(workdir string, validator BashValidator) *BackgroundManager {
	return &BackgroundManager{
		tasks:         map[string]*bgTask{},
		workdir:       workdir,
		bashValidator: validator,
	}
}

func (bg *BackgroundManager) Run(command string, timeout int) string {
	// Security check: use the same allowlist policy as interactive bash.
	if bg.bashValidator != nil {
		allowed, needConfirm, reason := bg.bashValidator(command)
		if !allowed {
			return fmt.Sprintf("Error: Security blocked: %s", reason)
		}
		// Background commands that require confirmation are blocked (non-interactive).
		if needConfirm {
			return fmt.Sprintf("Error: Command requires confirmation (not available in background mode): %s", reason)
		}
	}
	if timeout <= 0 {
		timeout = 120
	}
	bg.mu.Lock()
	bg.counter++
	id := fmt.Sprintf("bg_%d", bg.counter)
	bg.tasks[id] = &bgTask{Status: "running", Command: command}
	bg.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", command)
		cmd.Dir = bg.workdir
		utils.SetNewProcessGroup(cmd)
		output, err := cmd.CombinedOutput()

		status, result := "completed", strings.TrimSpace(string(output))
		if ctx.Err() == context.DeadlineExceeded {
			status, result = "timeout", "Timeout"
		} else if err != nil && result == "" {
			status, result = "error", err.Error()
		}
		if result == "" {
			result = "(no output)"
		}
		result = utils.Truncate(result, infra.MaxOutputLen)

		bg.mu.Lock()
		bg.tasks[id].Status = status
		bg.tasks[id].Result = result
		bg.queue = append(bg.queue, map[string]any{"task_id": id, "status": status, "result": utils.Truncate(result, 500)})
		bg.mu.Unlock()
	}()

	return fmt.Sprintf("Background task %s started: %s", id, utils.Truncate(command, 80))
}

func (bg *BackgroundManager) Check(id string) string {
	bg.mu.Lock()
	defer bg.mu.Unlock()
	if id != "" {
		t := bg.tasks[id]
		if t == nil {
			return "Unknown: " + id
		}
		return fmt.Sprintf("[%s] %s", t.Status, t.Result)
	}
	if len(bg.tasks) == 0 {
		return "No bg tasks."
	}
	var lines []string
	for k, v := range bg.tasks {
		lines = append(lines, fmt.Sprintf("%s: [%s] %s", k, v.Status, utils.Truncate(v.Command, 60)))
	}
	return strings.Join(lines, "\n")
}

func (bg *BackgroundManager) Drain() []map[string]any {
	bg.mu.Lock()
	defer bg.mu.Unlock()
	n := bg.queue
	bg.queue = nil
	return n
}
