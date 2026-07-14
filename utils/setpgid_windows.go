//go:build windows

package utils

import (
	"os/exec"
)

// SetNewProcessGroup on Windows: just kills the child process itself,
// as Windows has no process groups or POSIX signals.
// Descendant processes (if any) may survive as orphans.
func SetNewProcessGroup(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// On Windows, only the direct child can be killed.
		return cmd.Process.Kill()
	}
}
