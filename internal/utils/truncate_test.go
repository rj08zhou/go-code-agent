package utils

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateKeepsMultiByteCharactersIntact(t *testing.T) {
	chinese := strings.Repeat("中文输入支持", 10) // 60 runes, 180 bytes

	got := Truncate(chinese, 20)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated string is not valid UTF-8: %q", got)
	}
	if got != strings.Repeat("中文输入支持", 3)+"中文"+"..." {
		t.Fatalf("got %q, want first 20 runes plus ellipsis", got)
	}
}

func TestTruncateBoundaries(t *testing.T) {
	if got := Truncate("hello", 5); got != "hello" {
		t.Fatalf("ASCII at limit changed: %q", got)
	}
	if got := Truncate("hello world", 5); got != "hello..." {
		t.Fatalf("ASCII truncation changed: %q", got)
	}
	// Byte length exceeds the limit but rune count does not: keep as-is.
	if got := Truncate("你好", 2); got != "你好" {
		t.Fatalf("rune-count boundary changed: %q", got)
	}
	if got := Truncate("", 4); got != "" {
		t.Fatalf("empty input changed: %q", got)
	}
}
