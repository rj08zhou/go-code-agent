package security

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
)

// DiffPreview renders unified diffs from already-resolved file contents.
// It does not plan mutations (see tool.MutationPlanner) and does not prompt
// the operator (see PreviewAndConfirm). The workdir field is retained for
// wiring identity; PreviewChange does not read the filesystem.
type DiffPreview struct{ workdir string }

func NewDiffPreview(workdir string) *DiffPreview { return &DiffPreview{workdir: workdir} }

// PreviewChange generates a unified diff from content already resolved by a
// MutationPlanner. It does not re-read the workdir (avoids TOCTOU vs the plan).
func (d *DiffPreview) PreviewChange(path string, oldContent, newContent []byte) (string, error) {
	return generateUnifiedDiff(string(oldContent), string(newContent), path)
}

func generateUnifiedDiff(oldContent, newContent, filename string) (string, error) {
	oldTmp, _ := os.CreateTemp("", "diff-old-*")
	defer os.Remove(oldTmp.Name())
	newTmp, _ := os.CreateTemp("", "diff-new-*")
	defer os.Remove(newTmp.Name())
	oldTmp.WriteString(oldContent)
	oldTmp.Close()
	newTmp.WriteString(newContent)
	newTmp.Close()

	cmd := exec.Command("diff", "-u", "--label", "original/"+filename, "--label", "modified/"+filename, oldTmp.Name(), newTmp.Name())
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
			if buf.Len() > 0 {
				return "", fmt.Errorf("diff failed: %s", buf.String())
			}
		}
	}
	return buf.String(), nil
}
