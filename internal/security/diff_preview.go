package security

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// DiffPreview generates unified diffs for workspace file changes.
type DiffPreview struct{ workdir string }

func NewDiffPreview(workdir string) *DiffPreview { return &DiffPreview{workdir: workdir} }

func (d *DiffPreview) Preview(relPath string, newContent []byte) (string, error) {
	fp := filepath.Join(d.workdir, relPath)
	oldContent, err := os.ReadFile(fp)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if os.IsNotExist(err) {
		oldContent = nil
	}
	return generateUnifiedDiff(string(oldContent), string(newContent), relPath)
}

func (d *DiffPreview) PreviewDelete(relPath string) (string, error) {
	fp := filepath.Join(d.workdir, relPath)
	oldContent, err := os.ReadFile(fp)
	if err != nil {
		return "", err
	}
	return generateUnifiedDiff(string(oldContent), "", relPath)
}

// PreviewChange generates a diff from content already resolved in the actual
// ToolScope. It is independent of the workdir attached to this service.
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
