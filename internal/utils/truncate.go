// Package utils provides general-purpose helpers.
package utils

import "strings"

// Truncate returns s truncated to maxLen characters (runes), appending "...".
// Cutting by runes keeps multi-byte UTF-8 text (e.g. Chinese) intact.
func Truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s // byte length bounds rune length; nothing to cut
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}

// JoinWorkdir joins path segments with "/".
func JoinWorkdir(segments ...string) string {
	return strings.Join(segments, "/")
}
