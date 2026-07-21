// Package utils provides general-purpose helpers.
package utils

import "strings"

// Truncate returns s truncated to maxLen chars, appending "...".
func Truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// JoinWorkdir joins path segments with "/".
func JoinWorkdir(segments ...string) string {
	return strings.Join(segments, "/")
}
