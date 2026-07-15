package security

import (
	"bufio"
	"bytes"
	"fmt"
	"go-code-agent/internal/logging"
	"os"
	"os/exec"
	"strings"
)

// ReadLine reads one line of interactive user input for confirmation
// prompts. Overridable by main() to route through the readline instance
// that owns the terminal, so confirmation prompts work in raw mode.
var ReadLine = defaultReadLine

func defaultReadLine() (string, error) {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line), err
}

func GenerateUnifiedDiff(oldContent, newContent, filename string) (string, error) {
	// Create temporary files for old and new content
	oldTmp, err := os.CreateTemp("", "old-*.txt")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file for old content: %v", err)
	}
	defer os.Remove(oldTmp.Name())
	defer oldTmp.Close()

	newTmp, err := os.CreateTemp("", "new-*.txt")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file for new content: %v", err)
	}
	defer os.Remove(newTmp.Name())
	defer newTmp.Close()

	// Write content to temp files
	if _, err := oldTmp.WriteString(oldContent); err != nil {
		return "", fmt.Errorf("failed to write old content: %v", err)
	}
	oldTmp.Close()

	if _, err := newTmp.WriteString(newContent); err != nil {
		return "", fmt.Errorf("failed to write new content: %v", err)
	}
	newTmp.Close()

	// Run diff command to generate unified diff
	cmd := exec.Command("diff", "-u", "--label=original/"+filename, "--label=modified/"+filename, oldTmp.Name(), newTmp.Name())
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	// diff returns exit code 1 when files differ (this is normal)
	err = cmd.Run()
	if err != nil {
		// Exit code 1 means files differ, which is what we want
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
			// Real error (not just "files differ")
			if stderr.Len() > 0 {
				return "", fmt.Errorf("diff command failed: %s", stderr.String())
			}
		}
	}

	diffOutput := out.String()
	if diffOutput == "" {
		return "", nil // No differences
	}

	return diffOutput, nil
}

// colorizeDiff adds ANSI color codes to unified diff output.
func colorizeDiff(diff string) string {
	var colored strings.Builder
	lines := strings.Split(diff, "\n")

	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++"):
			// File headers: bold white
			colored.WriteString(logging.ColorBold + logging.ColorWhite + line + logging.ColorReset + "\n")
		case strings.HasPrefix(line, "@@"):
			// Hunk headers: cyan
			colored.WriteString(logging.ColorCyan + line + logging.ColorReset + "\n")
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			// Added lines: green
			colored.WriteString(logging.ColorGreen + line + logging.ColorReset + "\n")
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			// Removed lines: red
			colored.WriteString(logging.ColorRed + line + logging.ColorReset + "\n")
		default:
			// Context lines: no color
			colored.WriteString(line + "\n")
		}
	}

	return strings.TrimRight(colored.String(), "\n")
}

// diffHunk represents a single hunk in unified diff format.
type diffHunk struct {
	Header string   // @@ -a,b +c,d @@ header line
	Lines  []string // All lines in this hunk (including context, +, -)
	Start  int      // Starting line index in the original diff
	End    int      // Ending line index in the original diff
}

// parseHunks splits a unified diff into individual hunks.
func parseHunks(diff string) []diffHunk {
	var hunks []diffHunk
	lines := strings.Split(diff, "\n")

	var currentHunk *diffHunk
	inHunk := false

	for i, line := range lines {
		if strings.HasPrefix(line, "@@") {
			// Save previous hunk if exists
			if currentHunk != nil {
				currentHunk.End = i - 1
				hunks = append(hunks, *currentHunk)
			}
			// Start new hunk
			currentHunk = &diffHunk{
				Header: line,
				Lines:  []string{line},
				Start:  i,
			}
			inHunk = true
		} else if inHunk {
			currentHunk.Lines = append(currentHunk.Lines, line)
		}
	}

	// Don't forget the last hunk
	if currentHunk != nil {
		currentHunk.End = len(lines) - 1
		hunks = append(hunks, *currentHunk)
	}

	return hunks
}

// PreviewAndConfirm shows the diff to user and asks for confirmation.
// Supports chunk-by-chunk review when there are multiple hunks.
// Returns (finalContent, ok):
//   - ok=true means user accepted (some or all) changes; finalContent is what should be written.
//   - ok=false means user rejected all changes; finalContent is empty.
//
// Parameters oldContent and newContent are needed for partial apply.
func PreviewAndConfirm(path, oldContent, newContent, diff string) (string, bool) {
	fmt.Println()
	fmt.Println("─── Proposed changes to " + path + " ───")
	fmt.Println()

	// Parse hunks
	hunks := parseHunks(diff)

	if len(hunks) == 0 {
		fmt.Println("  ⚠️  No differences found")
		return "", false
	}

	// If only one hunk, use simple confirm (all-or-nothing)
	if len(hunks) == 1 {
		if previewSingleHunk(path, hunks[0], diff) {
			return newContent, true
		}
		return "", false
	}

	// Multiple hunks: chunk-by-chunk review
	return previewChunkByChunk(path, oldContent, newContent, hunks)
}

// previewSingleHunk handles confirmation for a single hunk.
// Returns true if user accepts the change.
func previewSingleHunk(path string, hunk diffHunk, fullDiff string) bool {
	// Show colored diff
	coloredDiff := colorizeDiff(fullDiff)
	fmt.Println(coloredDiff)
	fmt.Println()
	fmt.Print("  [A]pply  [R]eject  [D]iff again  [Q]uit: ")

	line, _ := ReadLine()
	answer := strings.ToLower(strings.TrimSpace(line))

	switch answer {
	case "a", "apply", "y", "yes":
		return true
	case "r", "reject", "n", "no":
		fmt.Println("  ❌ Changes rejected")
		return false
	case "d", "diff":
		return previewSingleHunk(path, hunk, fullDiff) // Show again
	case "q", "quit":
		fmt.Println("  ❌ Changes rejected (quit)")
		return false
	default:
		fmt.Println("  ⚠️  Invalid option, rejecting changes")
		return false
	}
}

// previewChunkByChunk handles chunk-by-chunk confirmation.
// Returns (finalContent, ok) — finalContent is the content to write after partial apply.
func previewChunkByChunk(path, oldContent, newContent string, hunks []diffHunk) (string, bool) {
	fmt.Printf("  Found %d change blocks (hunks). Reviewing chunk by chunk...\n", len(hunks))
	fmt.Println()

	acceptedHunks := make([]bool, len(hunks))
	allAccepted := false
	allRejected := false

	for i := 0; i < len(hunks); i++ {
		hunk := hunks[i]
		if allAccepted {
			acceptedHunks[i] = true
			continue
		}
		if allRejected {
			acceptedHunks[i] = false
			continue
		}

		fmt.Println("────────────────────────────────────────────────────────────")
		fmt.Printf("  Chunk %d/%d:\n", i+1, len(hunks))
		fmt.Println()

		// Show colored hunk
		hunkDiff := strings.Join(hunk.Lines, "\n")
		coloredHunk := colorizeDiff(hunkDiff)
		fmt.Println(coloredHunk)
		fmt.Println()

		fmt.Print("  [A]ccept  [R]eject  a[L]l accept  [N]o all  [D]iff again  [Q]uit: ")

		line, _ := ReadLine()
		answer := strings.ToLower(strings.TrimSpace(line))

		switch answer {
		case "a", "accept", "y", "yes":
			acceptedHunks[i] = true
			fmt.Println("  ✓ Chunk accepted")
		case "r", "reject":
			acceptedHunks[i] = false
			fmt.Println("  ✗ Chunk rejected")
		case "l", "all":
			allAccepted = true
			acceptedHunks[i] = true
			fmt.Println("  ✓ All remaining chunks accepted")
		case "n", "no":
			allRejected = true
			acceptedHunks[i] = false
			fmt.Println("  ✗ All remaining chunks rejected")
		case "d", "diff":
			// Show again - repeat this chunk
			i--
			continue
		case "q", "quit":
			fmt.Println("  ❌ Changes rejected (quit)")
			return "", false
		default:
			fmt.Println("  ⚠️  Invalid option, rejecting chunk")
			acceptedHunks[i] = false
		}
		fmt.Println()
	}

	// Check overall state
	anyAccepted := false
	allAcceptedFinal := true
	for _, accepted := range acceptedHunks {
		if accepted {
			anyAccepted = true
		} else {
			allAcceptedFinal = false
		}
	}

	if !anyAccepted {
		fmt.Println("  ❌ All chunks rejected")
		return "", false
	}

	if allAcceptedFinal {
		fmt.Println("  ✓ All chunks accepted, applying changes...")
		return newContent, true
	}

	// Partial apply: build a filtered patch with only accepted hunks
	fmt.Println()
	fmt.Printf("  Applying %d accepted chunks (skipping %d rejected)...\n",
		countTrue(acceptedHunks), len(acceptedHunks)-countTrue(acceptedHunks))

	finalContent, err := applyAcceptedHunks(oldContent, path, hunks, acceptedHunks)
	if err != nil {
		fmt.Printf("  ⚠️  Partial apply failed: %v\n", err)
		fmt.Print("  Apply ALL changes instead? [y/N]: ")
		ansLine, _ := ReadLine()
		ans := strings.ToLower(strings.TrimSpace(ansLine))
		if ans == "y" || ans == "yes" {
			return newContent, true
		}
		return "", false
	}

	fmt.Println("  ✓ Partial apply succeeded")
	return finalContent, true
}

// countTrue counts the number of true elements in a bool slice.
func countTrue(bs []bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

// applyAcceptedHunks builds a filtered patch containing only the accepted hunks
// and applies it to oldContent using the system 'patch' command.
// Returns the resulting file content.
func applyAcceptedHunks(oldContent, filename string, hunks []diffHunk, accepted []bool) (string, error) {
	if len(hunks) != len(accepted) {
		return "", fmt.Errorf("hunks and accepted slices size mismatch")
	}

	// Build a filtered patch: standard unified-diff header + only accepted hunks
	var patchBuf strings.Builder
	patchBuf.WriteString("--- original/" + filename + "\n")
	patchBuf.WriteString("+++ modified/" + filename + "\n")
	for i, hunk := range hunks {
		if !accepted[i] {
			continue
		}
		// hunk.Lines includes the @@ header at index 0
		for _, ln := range hunk.Lines {
			patchBuf.WriteString(ln)
			patchBuf.WriteString("\n")
		}
	}
	patchContent := patchBuf.String()

	// Write oldContent to a temp file (the file to be patched)
	srcTmp, err := os.CreateTemp("", "patch-src-*.txt")
	if err != nil {
		return "", fmt.Errorf("create source temp file: %v", err)
	}
	defer os.Remove(srcTmp.Name())
	if _, err := srcTmp.WriteString(oldContent); err != nil {
		srcTmp.Close()
		return "", fmt.Errorf("write source temp file: %v", err)
	}
	srcTmp.Close()

	// Write patch to a temp file
	patchTmp, err := os.CreateTemp("", "patch-*.patch")
	if err != nil {
		return "", fmt.Errorf("create patch temp file: %v", err)
	}
	defer os.Remove(patchTmp.Name())
	if _, err := patchTmp.WriteString(patchContent); err != nil {
		patchTmp.Close()
		return "", fmt.Errorf("write patch temp file: %v", err)
	}
	patchTmp.Close()

	// Run: patch <srcFile> <patchFile>
	// Use --no-backup-if-mismatch to avoid creating .orig files
	// Use --fuzz=3 to allow some context fuzziness when accepted hunks shift line numbers
	cmd := exec.Command("patch", "--no-backup-if-mismatch", "--fuzz=3", srcTmp.Name(), patchTmp.Name())
	var stderr bytes.Buffer
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String() + stdout.String())
		if errMsg == "" {
			errMsg = err.Error()
		}
		return "", fmt.Errorf("patch failed: %s", errMsg)
	}

	// Read the patched file content
	patched, err := os.ReadFile(srcTmp.Name())
	if err != nil {
		return "", fmt.Errorf("read patched file: %v", err)
	}
	return string(patched), nil
}

// ShouldPreviewDiff returns false when the operator ran /approve danger.
func ShouldPreviewDiff() bool {
	return !GlobalApproval.IsAutoApproveAll()
}
