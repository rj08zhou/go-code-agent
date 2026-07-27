package agent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// buildEnvContext returns a compact <env> block for the system prompt.
// Values are gathered at session start; keep this block cheap and fail-soft.
func buildEnvContext(workdir, modelID string) string {
	var b strings.Builder
	b.WriteString("<env>\n")
	b.WriteString(fmt.Sprintf("Platform: %s/%s\n", runtime.GOOS, runtime.GOARCH))
	if ver := osVersion(); ver != "" {
		b.WriteString("OS Version: " + ver + "\n")
	}
	b.WriteString("Today's date: " + time.Now().Format("Monday, Jan 2, 2006") + "\n")
	b.WriteString("Working directory: " + workdir + "\n")

	isRepo, branch := gitInfo(workdir)
	if isRepo {
		b.WriteString("Is directory a git repo: yes\n")
		if branch != "" {
			b.WriteString("Current branch: " + branch + "\n")
		} else {
			b.WriteString("Current branch: (detached HEAD or unknown)\n")
		}
	} else {
		b.WriteString("Is directory a git repo: no\n")
	}

	if modelID != "" {
		b.WriteString("Model: " + modelID + "\n")
	}
	b.WriteString("</env>")
	return b.String()
}

func osVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "uname", "-sr").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitInfo(workdir string) (isRepo bool, branch string) {
	if workdir == "" {
		return false, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	inside, err := runGit(ctx, workdir, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(inside) != "true" {
		return false, ""
	}

	br, err := runGit(ctx, workdir, "branch", "--show-current")
	if err != nil {
		return true, ""
	}
	return true, strings.TrimSpace(br)
}

func runGit(ctx context.Context, workdir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", workdir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return stdout.String(), nil
}
