//go:build !windows

package utils

import (
	"os/exec"
	"syscall"
)

// SetProcessGroup sets the Setpgid flag on the command so the child
// process gets its own process group, preventing SIGINT propagation
// from the parent terminal to background child processes.
func SetProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
