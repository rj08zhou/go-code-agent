package hitl

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// applyAcceptedHunks builds a filtered patch and applies it with system patch.
func applyAcceptedHunks(oldContent, filename string, hunks []diffHunk, accepted []bool) (string, error) {
	if len(hunks) != len(accepted) {
		return "", fmt.Errorf("hunks / accepted size mismatch")
	}
	var patchBuf strings.Builder
	patchBuf.WriteString("--- original/" + filename + "\n")
	patchBuf.WriteString("+++ modified/" + filename + "\n")
	for i, h := range hunks {
		if !accepted[i] {
			continue
		}
		for _, line := range h.Lines {
			patchBuf.WriteString(line + "\n")
		}
	}

	srcTmp, _ := os.CreateTemp("", "patch-src-*")
	defer os.Remove(srcTmp.Name())
	srcTmp.WriteString(oldContent)
	srcTmp.Close()

	patchTmp, _ := os.CreateTemp("", "patch-*.patch")
	defer os.Remove(patchTmp.Name())
	patchTmp.WriteString(patchBuf.String())
	patchTmp.Close()

	cmd := exec.Command("patch", "--no-backup-if-mismatch", "--fuzz=3", srcTmp.Name(), patchTmp.Name())
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderrBuf.String())
		if errMsg == "" {
			errMsg = err.Error()
		}
		return "", fmt.Errorf("patch failed: %s", errMsg)
	}
	patched, err := os.ReadFile(srcTmp.Name())
	if err != nil {
		return "", fmt.Errorf("read patched file: %v", err)
	}
	return string(patched), nil
}
