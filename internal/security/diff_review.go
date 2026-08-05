package security

import (
	"fmt"
	"strings"

	"go-code-agent/internal/utils"
)

// diffHunk is one unified-diff hunk used for interactive partial review.
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

// PreviewCreateAndConfirm reviews a newly-created file as one operation.
func PreviewCreateAndConfirm(path, newContent, diff string, consoles ...InteractiveIO) (string, bool) {
	console := resolveInteractiveIO(consoles...)
	printReviewHeader("Proposed new file", path, console)
	if previewWholeChange(diff, false, console) {
		return newContent, true
	}
	return "", false
}

// PreviewDeleteAndConfirm reviews deletion of the entire file as one operation.
func PreviewDeleteAndConfirm(path, diff string, consoles ...InteractiveIO) bool {
	console := resolveInteractiveIO(consoles...)
	printReviewHeader("Proposed deletion of", path, console)
	return previewWholeChange(diff, true, console)
}

// PreviewAndConfirm reviews edits to an existing file, including per-hunk review.
// Returns (finalContent, ok). ok=true means apply the returned content.
func PreviewAndConfirm(path, oldContent, newContent, diff string, consoles ...InteractiveIO) (string, bool) {
	console := resolveInteractiveIO(consoles...)
	printReviewHeader("Proposed changes to", path, console)

	hunks := parseHunks(diff)
	if len(hunks) == 0 {
		console.WriteInteractive("  No differences found\n")
		return "", false
	}
	if len(hunks) == 1 {
		if previewSingleHunk(path, hunks[0], diff, console) {
			return newContent, true
		}
		return "", false
	}
	return previewChunkByChunk(path, oldContent, newContent, hunks, console)
}

func resolveInteractiveIO(consoles ...InteractiveIO) InteractiveIO {
	if len(consoles) > 0 && consoles[0] != nil {
		return consoles[0]
	}
	return DefaultInteractiveIO()
}

func printReviewHeader(action, path string, consoles ...InteractiveIO) {
	console := resolveInteractiveIO(consoles...)
	console.WriteInteractive("\n")
	console.WriteInteractive(fmt.Sprintf("%s─── %s %s ───%s\n", utils.Bold, action, path, utils.Reset))
	console.WriteInteractive("\n")
}

func readReviewChoice(io InteractiveIO, prompt string) (string, error) {
	line, err := io.ReadLine(prompt)
	if err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(line)), nil
}

func rejectOnInputError(consoles ...InteractiveIO) {
	resolveInteractiveIO(consoles...).WriteInteractive("  Input closed or interrupted; rejecting changes\n")
}

func previewWholeChange(diff string, deleting bool, consoles ...InteractiveIO) bool {
	console := resolveInteractiveIO(consoles...)
	display := colorizeDiff(diff)
	if strings.TrimSpace(diff) == "" {
		display = "  (empty file)"
	}
	showDiff := true
	for {
		if showDiff {
			console.WriteInteractive(display + "\n\n")
			showDiff = false
		}
		prompt := "  [A]pply all  [R]eject  [V]iew diff again  [Q]uit: "
		if deleting {
			prompt = "  [D]elete file  [R]eject  [V]iew diff again  [Q]uit: "
		}
		answer, err := readReviewChoice(console, prompt)
		if err != nil {
			rejectOnInputError(console)
			return false
		}
		switch answer {
		case "":
			console.WriteInteractive("  Enter A/R/V/Q (or D when deleting)\n")
			continue
		case "y", "yes":
			return true
		case "a", "apply", "all":
			if !deleting {
				return true
			}
			console.WriteInteractive("  Invalid option; enter D to delete, R to reject, V to view, Q to quit\n")
			continue
		case "d", "delete":
			if deleting {
				return true
			}
			console.WriteInteractive("  Invalid option; enter A to apply, R to reject, V to view, Q to quit\n")
			continue
		case "r", "reject", "n", "no", "q", "quit":
			console.WriteInteractive("  Changes rejected\n")
			return false
		case "v", "view", "diff":
			showDiff = true
			continue
		default:
			console.WriteInteractive("  Invalid option; enter A/R/V/Q (or D when deleting)\n")
			continue
		}
	}
}

func previewSingleHunk(path string, hunk diffHunk, fullDiff string, consoles ...InteractiveIO) bool {
	console := resolveInteractiveIO(consoles...)
	showDiff := true
	for {
		if showDiff {
			console.WriteInteractive(colorizeDiff(fullDiff) + "\n\n")
			showDiff = false
		}
		answer, err := readReviewChoice(console, "  [A]pply  [R]eject  [D]iff again  [Q]uit: ")
		if err != nil {
			rejectOnInputError(console)
			return false
		}
		switch answer {
		case "":
			console.WriteInteractive("  Enter A, R, D, or Q\n")
			continue
		case "a", "apply", "y", "yes":
			return true
		case "r", "reject", "n", "no":
			console.WriteInteractive("  Changes rejected\n")
			return false
		case "d", "diff":
			showDiff = true
			continue
		case "q", "quit":
			console.WriteInteractive("  Changes rejected (quit)\n")
			return false
		default:
			console.WriteInteractive("  Invalid option; enter A, R, D, or Q\n")
			continue
		}
	}
}

func previewChunkByChunk(path, oldContent, newContent string, hunks []diffHunk, consoles ...InteractiveIO) (string, bool) {
	console := resolveInteractiveIO(consoles...)
	console.WriteInteractive(fmt.Sprintf("  Found %d change blocks (hunks). Reviewing chunk by chunk...\n\n", len(hunks)))

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

		console.WriteInteractive(utils.Dim + "────────────────────────────────────────────────────────────" + utils.Reset + "\n")
		console.WriteInteractive(fmt.Sprintf("  Chunk %d/%d:\n\n", i+1, len(hunks)))
		console.WriteInteractive(colorizeDiff(strings.Join(hunks[i].Lines, "\n")) + "\n\n")

		answer, err := readReviewChoice(console, "  [A]ccept  [R]eject  a[L]l accept  [N]o all  [D]iff again  [Q]uit: ")
		if err != nil {
			rejectOnInputError(console)
			return "", false
		}
		switch answer {
		case "":
			console.WriteInteractive("  Enter A, R, L, N, D, or Q\n")
			i--
			continue
		case "a", "accept", "y", "yes":
			accepted[i] = true
			console.WriteInteractive("  Chunk accepted\n")
		case "r", "reject":
			console.WriteInteractive("  Chunk rejected\n")
		case "l", "all":
			allAccepted = true
			accepted[i] = true
			console.WriteInteractive("  All remaining chunks accepted\n")
		case "n", "no":
			allRejected = true
			console.WriteInteractive("  All remaining chunks rejected\n")
		case "d", "diff":
			i--
			continue
		case "q", "quit":
			console.WriteInteractive("  Changes rejected (quit)\n")
			return "", false
		default:
			console.WriteInteractive("  Invalid option; enter A, R, L, N, D, or Q\n")
			i--
			continue
		}
		console.WriteInteractive("\n")
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
		console.WriteInteractive("  All chunks rejected\n")
		return "", false
	}
	if allAcceptedFinal {
		console.WriteInteractive("  All chunks accepted, applying changes...\n")
		return newContent, true
	}

	console.WriteInteractive(fmt.Sprintf("  Applying %d accepted chunks (skipping %d rejected)...\n",
		countTrue(accepted), len(accepted)-countTrue(accepted)))
	finalContent, err := applyAcceptedHunks(oldContent, path, hunks, accepted)
	if err != nil {
		console.WriteInteractive(fmt.Sprintf("  Partial apply failed: %v\n", err))
		if confirmApplyAllAfterPartialFailure(console) {
			return newContent, true
		}
		return "", false
	}
	console.WriteInteractive("  Partial apply succeeded\n")
	return finalContent, true
}

func confirmApplyAllAfterPartialFailure(consoles ...InteractiveIO) bool {
	console := resolveInteractiveIO(consoles...)
	answer, err := readReviewChoice(console, "  Apply ALL changes instead? [y/N]: ")
	if err != nil {
		rejectOnInputError(console)
		return false
	}
	return answer == "y"
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
