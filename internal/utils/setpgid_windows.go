//go:build windows

package utils

import "os/exec"

func SetProcessGroup(cmd *exec.Cmd) {
	// Windows has no direct setpgid equivalent; use creation flags if needed.
}
