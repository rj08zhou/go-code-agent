package security

import (
	"bytes"
	"fmt"
	"go-code-agent-refactor/internal/utils"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// --------------------------------------------------------------------------
// DiffPreview — generates unified diffs for file changes.
// --------------------------------------------------------------------------

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

// --------------------------------------------------------------------------
// Hunk parsing
// --------------------------------------------------------------------------

type diffHunk struct {
	Header string
	Lines  []string
	Start  int
	End    int
}

func parseHunks(diff string) []diffHunk {
	var hunks []diffHunk
	lines := strings.Split(diff, "\n")
	var cur *diffHunk
	inHunk := false
	for i, line := range lines {
		if strings.HasPrefix(line, "@@") {
			if cur != nil {
				cur.End = i - 1
				hunks = append(hunks, *cur)
			}
			cur = &diffHunk{Header: line, Lines: []string{line}, Start: i}
			inHunk = true
		} else if inHunk {
			cur.Lines = append(cur.Lines, line)
		}
	}
	if cur != nil {
		cur.End = len(lines) - 1
		hunks = append(hunks, *cur)
	}
	return hunks
}

// --------------------------------------------------------------------------
// Color
// --------------------------------------------------------------------------

func colorizeDiff(diff string) string {
	var b strings.Builder
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++"):
			fmt.Fprintf(&b, "%s%s%s\n", utils.Bold, line, utils.Reset)
		case strings.HasPrefix(line, "@@"):
			fmt.Fprintf(&b, "%s%s%s\n", utils.Cyan, line, utils.Reset)
		case strings.HasPrefix(line, "+"):
			fmt.Fprintf(&b, "%s%s%s\n", utils.Green, line, utils.Reset)
		case strings.HasPrefix(line, "-"):
			fmt.Fprintf(&b, "%s%s%s\n", utils.Red, line, utils.Reset)
		default:
			b.WriteString(line + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// --------------------------------------------------------------------------
// Preview and confirm — dispatch
// --------------------------------------------------------------------------

// PreviewAndConfirm shows the diff and asks for confirmation.
// Returns (finalContent, ok). ok=true means apply the returned content.
func PreviewAndConfirm(path, oldContent, newContent, diff string) (string, bool) {
	fmt.Println()
	fmt.Println(utils.Bold + "─── Proposed changes to " + path + " ───" + utils.Reset)
	fmt.Println()

	hunks := parseHunks(diff)
	if len(hunks) == 0 {
		fmt.Println("  No differences found")
		return "", false
	}
	if len(hunks) == 1 {
		if previewSingleHunk(path, hunks[0], diff) {
			return newContent, true
		}
		return "", false
	}
	return previewChunkByChunk(path, oldContent, newContent, hunks)
}

func previewSingleHunk(path string, hunk diffHunk, fullDiff string) bool {
	fmt.Println(colorizeDiff(fullDiff))
	fmt.Println()
	line, _ := ReadLine("  [A]pply  [R]eject  [D]iff again  [Q]uit: ")
	ans := strings.ToLower(strings.TrimSpace(line))
	switch ans {
	case "a", "apply", "y", "yes":
		return true
	case "r", "reject", "n", "no":
		fmt.Println("  Changes rejected")
		return false
	case "d", "diff":
		return previewSingleHunk(path, hunk, fullDiff)
	case "q", "quit":
		fmt.Println("  Changes rejected (quit)")
		return false
	default:
		fmt.Println("  Invalid option, rejecting changes")
		return false
	}
}

func previewChunkByChunk(path, oldContent, newContent string, hunks []diffHunk) (string, bool) {
	fmt.Printf("  Found %d change blocks (hunks). Reviewing chunk by chunk...\n", len(hunks))
	fmt.Println()

	accepted := make([]bool, len(hunks))
	allAccepted := false
	allRejected := false

	for i := 0; i < len(hunks); i++ {
		if allAccepted {
			accepted[i] = true
			continue
		}
		if allRejected {
			continue
		}

		fmt.Println(utils.Dim + "────────────────────────────────────────────────────────────" + utils.Reset)
		fmt.Printf("  Chunk %d/%d:\n", i+1, len(hunks))
		fmt.Println()
		fmt.Println(colorizeDiff(strings.Join(hunks[i].Lines, "\n")))
		fmt.Println()

		line, _ := ReadLine("  [A]ccept  [R]eject  a[L]l accept  [N]o all  [D]iff again  [Q]uit: ")
		ans := strings.ToLower(strings.TrimSpace(line))
		switch ans {
		case "a", "accept", "y", "yes":
			accepted[i] = true
			fmt.Println("  Chunk accepted")
		case "r", "reject":
			fmt.Println("  Chunk rejected")
		case "l", "all":
			allAccepted = true
			accepted[i] = true
			fmt.Println("  All remaining chunks accepted")
		case "n", "no":
			allRejected = true
			fmt.Println("  All remaining chunks rejected")
		case "d", "diff":
			i--
			continue
		case "q", "quit":
			fmt.Println("  Changes rejected (quit)")
			return "", false
		default:
			fmt.Println("  Invalid option, rejecting chunk")
		}
		fmt.Println()
	}

	anyAccepted := false
	allAcceptedFinal := true
	for _, a := range accepted {
		if a {
			anyAccepted = true
		} else {
			allAcceptedFinal = false
		}
	}
	if !anyAccepted {
		fmt.Println("  All chunks rejected")
		return "", false
	}
	if allAcceptedFinal {
		fmt.Println("  All chunks accepted, applying changes...")
		return newContent, true
	}

	fmt.Printf("  Applying %d accepted chunks (skipping %d rejected)...\n",
		countTrue(accepted), len(accepted)-countTrue(accepted))
	finalContent, err := applyAcceptedHunks(oldContent, path, hunks, accepted)
	if err != nil {
		fmt.Printf("  Partial apply failed: %v\n", err)
		line, _ := ReadLine("  Apply ALL changes instead? [y/N]: ")
		if strings.ToLower(strings.TrimSpace(line)) == "y" {
			return newContent, true
		}
		return "", false
	}
	fmt.Println("  Partial apply succeeded")
	return finalContent, true
}

func countTrue(bs []bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

// --------------------------------------------------------------------------
// applyAcceptedHunks — build a filtered patch and apply with system `patch`.
// --------------------------------------------------------------------------

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
		for _, ln := range h.Lines {
			patchBuf.WriteString(ln + "\n")
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

// --------------------------------------------------------------------------
// Global toggle
// --------------------------------------------------------------------------

var autoApproveAll bool

func SetAutoApproveAll(v bool) { autoApproveAll = v }

func ShouldPreviewDiff() bool { return !autoApproveAll }
