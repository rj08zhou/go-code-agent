package main

import "testing"

func TestIsReadOnlyBash(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want bool
	}{
		// --- should be allowed (read-only) ---
		{"ls", "ls -la", true},
		{"cat", "cat README.md", true},
		{"find", "find . -name *.go", true},
		{"grep", "grep -r foo .", true},
		{"head", "head -n 50 file.log", true},
		{"wc", "wc -l file.log", true},
		{"echo plain", "echo hello", true},
		{"sort plain", "sort file.txt", true},
		{"sed no -i", "sed -n 1,10p file.txt", true},

		// --- should be rejected ---
		{"empty", "", false},
		{"whitespace", "   ", false},
		{"unknown cmd", "make build", false},
		{"rm", "rm -rf foo", false},
		{"redirect out", "ls > out.txt", false},
		{"redirect append", "echo x >> log", false},
		{"redirect in", "cat < input", false},
		{"pipe", "ls | grep foo", false},
		{"semicolon", "ls; rm foo", false},
		{"and", "ls && rm foo", false},
		{"backtick", "echo `whoami`", false},
		{"command sub", "echo $(pwd)", false},
		{"sed -i", "sed -i s/foo/bar/ file.txt", false},
		{"sed -i.bak", "sed -i.bak s/x/y/ a.txt", false},
		{"awk -i inplace", "awk -i inplace {print} file", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsReadOnlyBash(tc.cmd)
			if got != tc.want {
				t.Errorf("IsReadOnlyBash(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}
