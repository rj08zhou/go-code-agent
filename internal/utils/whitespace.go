package utils

import "strings"

// WhitespaceNormalize collapses runs of whitespace into a single space and
// trims the ends. Used by edit_file's whitespace-tolerant match fallback.
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
