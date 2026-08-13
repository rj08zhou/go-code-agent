package hitl

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadLineFromPreservesSpacesAndSequentialLines(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("please use tabs\ny   \n"))
	var output bytes.Buffer

	feedback, err := readLineFrom(reader, &output, "Feedback: ")
	if err != nil {
		t.Fatal(err)
	}
	if feedback != "please use tabs" {
		t.Fatalf("feedback = %q, want complete line", feedback)
	}

	choice, err := readLineFrom(reader, &output, "Choice: ")
	if err != nil {
		t.Fatal(err)
	}
	if choice != "y   " {
		t.Fatalf("choice = %q, want trailing spaces preserved for caller trimming", choice)
	}
	if got := output.String(); got != "Feedback: Choice: " {
		t.Fatalf("prompts = %q", got)
	}
}

func TestReadLineFromHandlesFinalLineAndEmptyEOF(t *testing.T) {
	var output bytes.Buffer
	line, err := readLineFrom(bufio.NewReader(strings.NewReader("final feedback")), &output, "")
	if err != nil {
		t.Fatalf("final line without newline: %v", err)
	}
	if line != "final feedback" {
		t.Fatalf("line = %q", line)
	}

	_, err = readLineFrom(bufio.NewReader(strings.NewReader("")), &output, "")
	if !errors.Is(err, io.EOF) {
		t.Fatalf("empty input error = %v, want EOF", err)
	}
}

func setReadLineForTest(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	SetReadLine(fn)
	t.Cleanup(ResetReadLine)
}

func TestDiffReviewRejectsAmbiguousMultiWordChoices(t *testing.T) {
	tests := []struct {
		name   string
		answer string
		review func() bool
	}{
		{name: "create whole file", answer: "apply maybe", review: func() bool {
			return previewWholeChange("+new", false)
		}},
		{name: "delete whole file", answer: "delete maybe", review: func() bool {
			return previewWholeChange("-old", true)
		}},
		{name: "single hunk", answer: "yes maybe", review: func() bool {
			return previewSingleHunk("file.txt", diffHunk{}, "diff")
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			setReadLineForTest(t, func(string) (string, error) {
				calls++
				if calls == 1 {
					return test.answer, nil
				}
				return "r", nil
			})
			if test.review() {
				t.Fatalf("ambiguous choice %q approved changes", test.answer)
			}
			if calls < 2 {
				t.Fatalf("ambiguous input calls = %d, want re-prompt then explicit reject", calls)
			}
		})
	}
}

func TestDiffReviewRePromptsOnEmptyOrInvalidInput(t *testing.T) {
	answers := []string{"", "zzz", "a"}
	idx := 0
	setReadLineForTest(t, func(string) (string, error) {
		if idx >= len(answers) {
			return "", io.EOF
		}
		answer := answers[idx]
		idx++
		return answer, nil
	})
	if !previewSingleHunk("file.txt", diffHunk{}, "diff") {
		t.Fatal("empty/invalid input should re-prompt and accept a later A")
	}
	if idx != 3 {
		t.Fatalf("input calls consumed = %d, want 3", idx)
	}
}

func TestDiffReviewFailsClosedWhenInputEnds(t *testing.T) {
	tests := []struct {
		name   string
		review func() bool
	}{
		{name: "create whole file", review: func() bool { return previewWholeChange("+new", false) }},
		{name: "delete whole file", review: func() bool { return previewWholeChange("-old", true) }},
		{name: "single hunk", review: func() bool {
			return previewSingleHunk("file.txt", diffHunk{}, "diff")
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setReadLineForTest(t, func(string) (string, error) { return "", io.EOF })
			if test.review() {
				t.Fatal("input error approved changes")
			}
		})
	}
}

func TestChunkReviewRejectsAmbiguousChoiceAndStopsOnInputError(t *testing.T) {
	hunks := []diffHunk{{Header: "@@ -1 +1 @@", Lines: []string{"-old", "+new"}}}

	t.Run("ambiguous choice re-prompts then rejects", func(t *testing.T) {
		calls := 0
		setReadLineForTest(t, func(string) (string, error) {
			calls++
			if calls == 1 {
				return "all maybe", nil
			}
			return "r", nil
		})
		if _, ok := previewChunkByChunk("file.txt", "old\n", "new\n", hunks); ok {
			t.Fatal("ambiguous apply-all choice approved changes")
		}
		if calls < 2 {
			t.Fatalf("input calls = %d, want re-prompt then reject", calls)
		}
	})

	t.Run("input error stops immediately", func(t *testing.T) {
		calls := 0
		setReadLineForTest(t, func(string) (string, error) {
			calls++
			return "", io.EOF
		})
		manyHunks := append(append([]diffHunk{}, hunks...), hunks...)
		if _, ok := previewChunkByChunk("file.txt", "old\n", "new\n", manyHunks); ok {
			t.Fatal("input error approved changes")
		}
		if calls != 1 {
			t.Fatalf("input calls = %d, want immediate stop after one error", calls)
		}
	})
}

func TestPartialApplyFallbackRequiresExactConfirmation(t *testing.T) {
	t.Run("exact confirmation", func(t *testing.T) {
		setReadLineForTest(t, func(string) (string, error) { return "y", nil })
		if !confirmApplyAllAfterPartialFailure() {
			t.Fatal("exact y confirmation was rejected")
		}
	})

	for _, test := range []struct {
		name   string
		answer string
		err    error
	}{
		{name: "ambiguous confirmation", answer: "y maybe"},
		{name: "input error", err: io.EOF},
	} {
		t.Run(test.name, func(t *testing.T) {
			setReadLineForTest(t, func(string) (string, error) { return test.answer, test.err })
			if confirmApplyAllAfterPartialFailure() {
				t.Fatal("unsafe fallback confirmation was accepted")
			}
		})
	}
}

func TestDiffPreviewChangeUsesProvidedContent(t *testing.T) {
	boundWorkdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(boundWorkdir, "file.txt"), []byte("main workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff, err := NewDiffPreview(boundWorkdir).PreviewChange(
		"file.txt",
		[]byte("teammate worktree\n"),
		[]byte("approved change\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "-teammate worktree") || strings.Contains(diff, "-main workspace") {
		t.Fatalf("diff used bound workdir instead of provided content: %q", diff)
	}
}
