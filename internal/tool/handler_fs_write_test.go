package tool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilesystemWriteTools(t *testing.T) {
	defs := filesystemWriteTools(builtinDeps{})
	workdir := t.TempDir()
	scope := &ToolScope{Workdir: workdir, AgentID: "lead"}

	tests := []struct {
		name       string
		tool       string
		args       json.RawMessage
		setup      func(t *testing.T)
		wantStatus Status
		wantSubstr string
		check      func(t *testing.T)
	}{
		{
			name:       "write_file success",
			tool:       "write_file",
			args:       json.RawMessage(`{"path":"a.txt","content":"hello"}`),
			wantStatus: StatusSucceeded,
			wantSubstr: "Wrote 5 bytes",
			check: func(t *testing.T) {
				data, err := os.ReadFile(filepath.Join(workdir, "a.txt"))
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != "hello" {
					t.Fatalf("content = %q", data)
				}
			},
		},
		{
			name:       "write_file missing path",
			tool:       "write_file",
			args:       json.RawMessage(`{"path":"","content":"x"}`),
			wantStatus: StatusFailed,
			wantSubstr: "path is required",
		},
		{
			name:       "write_file path escape",
			tool:       "write_file",
			args:       json.RawMessage(`{"path":"../escape.txt","content":"nope"}`),
			wantStatus: StatusFailed,
			wantSubstr: "escapes workdir",
		},
		{
			name: "edit_file success",
			tool: "edit_file",
			args: json.RawMessage(`{"path":"b.txt","old_text":"foo","new_text":"bar"}`),
			setup: func(t *testing.T) {
				if err := os.WriteFile(filepath.Join(workdir, "b.txt"), []byte("foo baz"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantStatus: StatusSucceeded,
			wantSubstr: "Edited",
			check: func(t *testing.T) {
				data, _ := os.ReadFile(filepath.Join(workdir, "b.txt"))
				if string(data) != "bar baz" {
					t.Fatalf("content = %q", data)
				}
			},
		},
		{
			name: "edit_file text not found",
			tool: "edit_file",
			args: json.RawMessage(`{"path":"c.txt","old_text":"missing","new_text":"x"}`),
			setup: func(t *testing.T) {
				if err := os.WriteFile(filepath.Join(workdir, "c.txt"), []byte("present"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantStatus: StatusFailed,
			wantSubstr: "Text not found",
		},
		{
			name: "delete_file success",
			tool: "delete_file",
			args: json.RawMessage(`{"path":"d.txt"}`),
			setup: func(t *testing.T) {
				if err := os.WriteFile(filepath.Join(workdir, "d.txt"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantStatus: StatusSucceeded,
			wantSubstr: "Deleted",
			check: func(t *testing.T) {
				if _, err := os.Stat(filepath.Join(workdir, "d.txt")); !os.IsNotExist(err) {
					t.Fatalf("file still exists: %v", err)
				}
			},
		},
		{
			name: "insert_file success",
			tool: "insert_file",
			args: json.RawMessage(`{"path":"e.txt","insert_at":2,"content":"middle"}`),
			setup: func(t *testing.T) {
				if err := os.WriteFile(filepath.Join(workdir, "e.txt"), []byte("one\ntwo\nthree"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantStatus: StatusSucceeded,
			wantSubstr: "Inserted at line 2",
			check: func(t *testing.T) {
				data, _ := os.ReadFile(filepath.Join(workdir, "e.txt"))
				if !strings.Contains(string(data), "middle") {
					t.Fatalf("content = %q", data)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup(t)
			}
			tool := mustTool(t, defs, tc.tool)
			got := tool.Handler(scope, tc.args)
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %s, want %s (output=%q)", got.Status, tc.wantStatus, got.Output)
			}
			if tc.wantSubstr != "" && !strings.Contains(got.Output, tc.wantSubstr) {
				t.Fatalf("output %q missing %q", got.Output, tc.wantSubstr)
			}
			if tc.check != nil {
				tc.check(t)
			}
		})
	}
}
