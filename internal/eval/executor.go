package eval

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Executor runs a Task in a temp workdir using the real agent binary.
// This is a CLI wrapper — it calls `go-code-agent-refactor` in a subprocess.
type Executor struct {
	BinaryPath string
	Workdir    string
}

func NewExecutor(binaryPath string) *Executor {
	return &Executor{BinaryPath: binaryPath}
}

// RunTask executes a single task by spawning the agent as a subprocess.
// The task prompt is written to stdin, and the agent runs until completion.
func (e *Executor) RunTask(task Task) (bool, string, string, error) {
	workdir, err := os.MkdirTemp("", "eval-exec-"+task.Name+"-*")
	if err != nil {
		return false, "", fmt.Sprintf("tempdir: %v", err), err
	}
	defer os.RemoveAll(workdir)

	// Run setup
	if task.Setup != nil {
		_, err := task.Setup(workdir)
		if err != nil {
			return false, workdir, fmt.Sprintf("setup: %v", err), err
		}
	}

	// Spawn agent subprocess with the prompt on stdin
	cmd := exec.Command(e.BinaryPath)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(),
		"LLM_PROVIDER=scripted", // use scripted provider
		"INPUT_MODE=stdin",
	)
	cmd.Stdin = strings.NewReader(task.Prompt + "\n/exit\n")

	output, err := cmd.CombinedOutput()
	_ = output
	outStr := string(output)

	if err != nil {
		return false, workdir, fmt.Sprintf("agent error: %v\n%s", err, outStr), nil
	}

	// Run verify
	if task.Verify != nil {
		ok, detail := task.Verify(workdir)
		return ok, workdir, detail, nil
	}

	return true, workdir, "", nil
}

// FindBinary locates the agent binary in common places.
func FindBinary() string {
	candidates := []string{
		"./go-code-agent-refactor",
		filepath.Join(os.Getenv("GOPATH"), "bin", "go-code-agent-refactor"),
		"/usr/local/bin/go-code-agent-refactor",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			abs, _ := filepath.Abs(p)
			return abs
		}
	}
	// Try go build
	if path, err := exec.LookPath("go"); err == nil {
		return path
	}
	return ""
}

var _ = fmt.Sprintln
