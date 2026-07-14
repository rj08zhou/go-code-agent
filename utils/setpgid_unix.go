//go:build linux || darwin || freebsd || netbsd || openbsd

package utils

import (
	"os/exec"
	"syscall"
)

// SetNewProcessGroup starts cmd in its own process group and kills the
// whole group on cancel, so shell-spawned descendants don't survive as orphans.
// Call after constructing the *exec.Cmd and before Start/Run/Output.
func SetNewProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid signals the whole process group.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
