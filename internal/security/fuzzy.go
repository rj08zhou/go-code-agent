package security

import "strings"

// FuzzyFind locates a block of text in a file with whitespace-tolerant matching.
// It normalizes all whitespace (spaces/tabs/newlines) to single spaces before comparing.
// Returns the 1-based start line or 0 if not found.
func FuzzyFind(fileContent, searchBlock string) (line int, length int) {
	fileNorm := WhitespaceNormalize(fileContent)
	searchNorm := WhitespaceNormalize(searchBlock)
	idx := strings.Index(fileNorm, searchNorm)
	if idx < 0 {
		return 0, 0
	}
	// Count lines up to idx in the original content
	line = strings.Count(fileContent[:idx], "\n") + 1
	length = strings.Count(searchBlock, "\n") + 1
	return line, length
}

// WhitespaceNormalize collapses all whitespace into single spaces.
func WhitespaceNormalize(s string) string {
	var b strings.Builder
	prevSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		} else {
			b.WriteRune(r)
			prevSpace = false
		}
	}
	return strings.TrimSpace(b.String())
}
