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

// SetNewProcessGroup configures cmd to start in its own new process
// group (pgid == the child's own pid) and installs a Cancel hook that
// kills the WHOLE group - not just the tracked child - when the
// command's context is cancelled or times out.
//
// Why this matters: exec.CommandContext's default cancellation only
// signals cmd.Process, the single process Go started directly
// (typically "sh" for a "sh -c <command>" invocation). Shell
// commands like "npm run dev" have that "sh" exec further child
// processes (npm -> node/vite); those descendants do NOT belong to
// cmd.Process and are therefore NOT killed by the default behavior -
// they survive as orphans after the timeout fires, continuing to
// hold whatever port/resource they opened. That is exactly what then
// requires a manual `lsof`+`kill <pid>` follow-up once the timeout
// message is seen. Putting the child in its own process group and
// killing that group on cancel takes every descendant down with it.
//
// Must be called after constructing the *exec.Cmd (via
// exec.CommandContext) and before Start()/Run()/Output()/
// CombinedOutput().
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
