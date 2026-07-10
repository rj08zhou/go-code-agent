package utils

import (
	"os"
	"os/exec"
	"syscall"
)

func JoinWorkdir(workdir, rel string) string {
	if workdir == "" {
		return rel
	}
	if rel == "" {
		return workdir
	}
	return workdir + string(os.PathSeparator) + rel
}

func Truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// SetNewProcessGroup starts cmd in its own process group and kills the
// whole group on cancel, so shell-spawned descendants don't survive as orphans.
// Call after constructing the *exec.Cmd and before Start/Run/Output.
func SetNewProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// A negative pid signals the whole process group (see
		// kill(2)). Setpgid:true with no Pgid set makes the child its
		// own group leader, so its pgid equals its pid.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
