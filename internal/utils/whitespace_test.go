package utils

import "testing"

func TestWhitespaceNormalize(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"  a  b  ", "a b"},
		{"a\tb\nc", "a b c"},
		{"already normal", "already normal"},
		{"\r\nfoo\t\tbar\r\n", "foo bar"},
	}
	for _, tc := range cases {
		if got := WhitespaceNormalize(tc.in); got != tc.want {
			t.Errorf("WhitespaceNormalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
