package tool

import (
	"errors"
	"strings"

	"go-code-agent/internal/utils"
)

var errMutationTextNotFound = errors.New("text not found")

// replaceFileContent computes the content produced by edit_file without
// touching the filesystem. It preserves the handler's whitespace-tolerant
// fallback so previews and execution always derive the same mutation.
func replaceFileContent(content, oldText, newText string, replaceAll bool) (string, error) {
	matchedText := oldText
	if !strings.Contains(content, matchedText) {
		for _, line := range strings.Split(content, "\n") {
			if utils.WhitespaceNormalize(line) == utils.WhitespaceNormalize(matchedText) {
				matchedText = line
				break
			}
		}
	}
	if !strings.Contains(content, matchedText) {
		return "", errMutationTextNotFound
	}
	if replaceAll {
		return strings.ReplaceAll(content, matchedText, newText), nil
	}
	return strings.Replace(content, matchedText, newText, 1), nil
}

// insertFileContent computes the content produced by insert_file without
// touching the filesystem. InsertAt is one-based and is clamped to the file's
// valid insertion range, matching the existing handler behavior.
func insertFileContent(content string, insertAt int, inserted string) string {
	lines := strings.Split(content, "\n")
	index := insertAt - 1
	if index < 0 {
		index = 0
	}
	if index > len(lines) {
		index = len(lines)
	}
	updated := append([]string{}, lines[:index]...)
	updated = append(updated, strings.Split(inserted, "\n")...)
	updated = append(updated, lines[index:]...)
	return strings.Join(updated, "\n")
}
