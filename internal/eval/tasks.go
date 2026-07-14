package eval

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Tasks is the baseline suite: representative coding tasks spanning the
// categories the agent is meant to handle. Each task has a scripted
// mock path (so it can run offline in CI) and a machine-checkable
// Verify. In --live mode the same Verify grades the real LLM run.
//
// Tasks marked ExpectFail are negative cases: they exercise the
// harness's failure-recording path and are considered passing when
// Verify returns false.
var Tasks = []Task{
	// ---------------------------------------------------------------
	// API endpoint
	// ---------------------------------------------------------------
	{
		Name:     "add-health-endpoint",
		Category: "api",
		Setup: func(w string) (string, error) {
			src := `package server

import "net/http"

func New() *http.ServeMux {
	m := http.NewServeMux()
	return m
}
`
			return "", os.WriteFile(filepath.Join(w, "server.go"), []byte(src), 0o644)
		},
		Prompt: "Add a GET /health endpoint to server.go that returns 200 OK with body \"ok\".",
		Script: []ScriptStep{
			{Text: "I'll add the health handler.", ToolCalls: []MockToolCall{
				{Name: "insert_file", Args: `{"path":"server.go","after_line":5,"content":"\tm.HandleFunc(\"/health\", func(w http.ResponseWriter, r *http.Request) {\n\t\tw.WriteHeader(200)\n\t\tw.Write([]byte(\"ok\"))\n\t})"}`},
			}},
			{Text: "Done.", Done: true},
		},
		Verify: func(w string) (bool, string) {
			b, err := os.ReadFile(filepath.Join(w, "server.go"))
			if err != nil {
				return false, "server.go missing"
			}
			s := string(b)
			if !strings.Contains(s, `/health`) || !strings.Contains(s, "200") || !strings.Contains(s, "ok") {
				return false, "health endpoint not present"
			}
			return true, ""
		},
	},
	{
		Name:     "add-post-endpoint",
		Category: "api",
		Setup: func(w string) (string, error) {
			src := `package api

import (
	"io"
	"net/http"
)

func Router() *http.ServeMux {
	m := http.NewServeMux()
	return m
}
`
			if err := writeGoMod(w); err != nil {
				return "", err
			}
			return "", os.WriteFile(filepath.Join(w, "api.go"), []byte(src), 0o644)
		},
		Prompt: "Add a POST /echo endpoint that writes back the request body.",
		Script: []ScriptStep{
			{Text: "Add echo handler.", ToolCalls: []MockToolCall{
				{Name: "insert_file", Args: `{"path":"api.go","after_line":9,"content":"\tm.HandleFunc(\"/echo\", func(w http.ResponseWriter, r *http.Request) {\n\t\tbody, _ := io.ReadAll(r.Body)\n\t\tw.Write(body)\n\t})"}`},
			}},
			{Text: "Done.", Done: true},
		},
		Verify: func(w string) (bool, string) {
			b, _ := os.ReadFile(filepath.Join(w, "api.go"))
			s := string(b)
			if !strings.Contains(s, `/echo`) || !strings.Contains(s, "ReadAll") {
				return false, "echo endpoint missing"
			}
			out, err := runGoBuild(w)
			if err != nil {
				return false, "build: " + out
			}
			return true, ""
		},
	},

	// ---------------------------------------------------------------
	// Bug fix
	// ---------------------------------------------------------------
	{
		Name:     "fix-off-by-one",
		Category: "bugfix",
		Setup: func(w string) (string, error) {
			src := `package calc

func SumN(n int) int {
	total := 0
	for i := 1; i < n; i++ { // bug: should be <= n
		total += i
	}
	return total
}
`
			return "", os.WriteFile(filepath.Join(w, "calc.go"), []byte(src), 0o644)
		},
		Prompt: "SumN(5) returns 10 but should return 15. Fix the off-by-one.",
		Script: []ScriptStep{
			{Text: "Fix the loop bound.", ToolCalls: []MockToolCall{
				{Name: "edit_file", Args: `{"path":"calc.go","old_text":"for i := 1; i < n; i++ { // bug: should be <= n","new_text":"for i := 1; i <= n; i++ {"}`},
			}},
			{Text: "Done.", Done: true},
		},
		Verify: func(w string) (bool, string) {
			b, _ := os.ReadFile(filepath.Join(w, "calc.go"))
			s := string(b)
			if strings.Contains(s, "i < n") {
				return false, "loop bound still wrong"
			}
			if !strings.Contains(s, "i <= n") {
				return false, "not fixed"
			}
			return true, ""
		},
	},
	{
		Name:     "fix-nil-check",
		Category: "bugfix",
		Setup: func(w string) (string, error) {
			src := `package user

type User struct{ Name string }

func Greet(u *User) string {
	return "hi " + u.Name // panics when u is nil
}
`
			return "", os.WriteFile(filepath.Join(w, "user.go"), []byte(src), 0o644)
		},
		Prompt: "Greet(nil) panics. Add a nil check returning \"hi guest\".",
		Script: []ScriptStep{
			{Text: "Guard nil.", ToolCalls: []MockToolCall{
				{Name: "edit_file", Args: `{"path":"user.go","old_text":"func Greet(u *User) string {\n\treturn \"hi \" + u.Name","new_text":"func Greet(u *User) string {\n\tif u == nil {\n\t\treturn \"hi guest\"\n\t}\n\treturn \"hi \" + u.Name"}`},
			}},
			{Text: "Done.", Done: true},
		},
		Verify: func(w string) (bool, string) {
			b, _ := os.ReadFile(filepath.Join(w, "user.go"))
			s := string(b)
			if !strings.Contains(s, "u == nil") || !strings.Contains(s, "hi guest") {
				return false, "nil guard missing"
			}
			return true, ""
		},
	},

	// ---------------------------------------------------------------
	// Refactor
	// ---------------------------------------------------------------
	{
		Name:     "extract-constant",
		Category: "refactor",
		Setup: func(w string) (string, error) {
			src := `package cfg

func Timeout() int { return 30 }
func Retry() int  { return 30 }
`
			if err := writeGoMod(w); err != nil {
				return "", err
			}
			return "", os.WriteFile(filepath.Join(w, "cfg.go"), []byte(src), 0o644)
		},
		Prompt: "Extract the magic number 30 into a named constant DefaultTimeout and use it in both functions.",
		Script: []ScriptStep{
			{Text: "Introduce constant via replace_all.", ToolCalls: []MockToolCall{
				{Name: "edit_file", Args: `{"path":"cfg.go","old_text":"30","new_text":"DefaultTimeout","replace_all":true}`},
			}},
			{Text: "Add the const declaration.", ToolCalls: []MockToolCall{
				{Name: "insert_file", Args: `{"path":"cfg.go","after_line":1,"content":"const DefaultTimeout = 30"}`},
			}},
			{Text: "Done.", Done: true},
		},
		Verify: func(w string) (bool, string) {
			b, _ := os.ReadFile(filepath.Join(w, "cfg.go"))
			s := string(b)
			if !strings.Contains(s, "const DefaultTimeout") || !strings.Contains(s, "DefaultTimeout") {
				return false, "constant not introduced"
			}
			out, err := runGoBuild(w)
			if err != nil {
				return false, "build: " + out
			}
			return true, ""
		},
	},
	{
		Name:     "rename-symbol",
		Category: "refactor",
		Setup: func(w string) (string, error) {
			src := `package app

func DoThing() string { return fooHelper() }

func fooHelper() string { return "x" }
`
			return "", os.WriteFile(filepath.Join(w, "app.go"), []byte(src), 0o644)
		},
		Prompt: "Rename fooHelper to helper everywhere (2 occurrences).",
		Script: []ScriptStep{
			{Text: "Replace all occurrences.", ToolCalls: []MockToolCall{
				{Name: "edit_file", Args: `{"path":"app.go","old_text":"fooHelper","new_text":"helper","replace_all":true}`},
			}},
			{Text: "Done.", Done: true},
		},
		Verify: func(w string) (bool, string) {
			b, _ := os.ReadFile(filepath.Join(w, "app.go"))
			s := string(b)
			if strings.Contains(s, "fooHelper") {
				return false, "old name still present"
			}
			if !strings.Contains(s, "helper") {
				return false, "new name absent"
			}
			return true, ""
		},
	},
	{
		Name:     "move-function-to-new-file",
		Category: "refactor",
		Setup: func(w string) (string, error) {
			src := `package app

func Add(a, b int) int { return a + b }

func Calc() int { return Add(1, 2) }
`
			if err := writeGoMod(w); err != nil {
				return "", err
			}
			return "", os.WriteFile(filepath.Join(w, "app.go"), []byte(src), 0o644)
		},
		Prompt: "Move the Add function from app.go into a new file math.go, keeping the package compilable.",
		Script: []ScriptStep{
			{Text: "Create math.go with Add.", ToolCalls: []MockToolCall{
				{Name: "write_file", Args: `{"path":"math.go","content":"package app\n\nfunc Add(a, b int) int { return a + b }\n"}`},
			}},
			{Text: "Remove Add from app.go.", ToolCalls: []MockToolCall{
				{Name: "edit_file", Args: `{"path":"app.go","old_text":"func Add(a, b int) int { return a + b }\n\n","new_text":""}`},
			}},
			{Text: "Done.", Done: true},
		},
		Verify: func(w string) (bool, string) {
			ab, _ := os.ReadFile(filepath.Join(w, "app.go"))
			as := string(ab)
			if strings.Contains(as, "func Add") {
				return false, "Add still defined in app.go"
			}
			if !strings.Contains(as, "Add(1, 2)") {
				return false, "Calc call to Add was removed"
			}
			mb, err := os.ReadFile(filepath.Join(w, "math.go"))
			if err != nil {
				return false, "math.go not created"
			}
			if !strings.Contains(string(mb), "func Add(a, b int) int") {
				return false, "Add not in math.go"
			}
			out, err := runGoBuild(w)
			if err != nil {
				return false, "build: " + out
			}
			return true, ""
		},
	},

	// ---------------------------------------------------------------
	// Edit primitives (replace_all / insert coverage)
	// ---------------------------------------------------------------
	{
		Name:     "replace-all-occurrences",
		Category: "edit",
		Setup: func(w string) (string, error) {
			src := "a=1\na=2\na=3\n"
			return "", os.WriteFile(filepath.Join(w, "v.txt"), []byte(src), 0o644)
		},
		Prompt: "Replace every 'a' with 'x' in v.txt.",
		Script: []ScriptStep{
			{Text: "replace_all.", ToolCalls: []MockToolCall{
				{Name: "edit_file", Args: `{"path":"v.txt","old_text":"a","new_text":"x","replace_all":true}`},
			}},
			{Text: "Done.", Done: true},
		},
		Verify: func(w string) (bool, string) {
			b, _ := os.ReadFile(filepath.Join(w, "v.txt"))
			s := string(b)
			if strings.Contains(s, "a=") {
				return false, "not all replaced"
			}
			if !strings.Contains(s, "x=1") || !strings.Contains(s, "x=3") {
				return false, "replacement malformed"
			}
			return true, ""
		},
	},
	{
		Name:     "insert-at-line",
		Category: "edit",
		Setup: func(w string) (string, error) {
			src := "line1\nline2\nline3\n"
			return "", os.WriteFile(filepath.Join(w, "l.txt"), []byte(src), 0o644)
		},
		Prompt: "Insert 'INSERTED' after line 2 of l.txt.",
		Script: []ScriptStep{
			{Text: "Insert after line 2.", ToolCalls: []MockToolCall{
				{Name: "insert_file", Args: `{"path":"l.txt","after_line":2,"content":"INSERTED"}`},
			}},
			{Text: "Done.", Done: true},
		},
		Verify: func(w string) (bool, string) {
			b, _ := os.ReadFile(filepath.Join(w, "l.txt"))
			s := string(b)
			want := "line1\nline2\nINSERTED\nline3\n"
			if s != want {
				return false, fmt.Sprintf("got %q want %q", s, want)
			}
			return true, ""
		},
	},

	// ---------------------------------------------------------------
	// Bash / build verification
	// ---------------------------------------------------------------
	{
		Name:     "build-check",
		Category: "bash",
		Setup: func(w string) (string, error) {
			src := "package main\n\nfunc main() {}\n"
			if err := writeGoMod(w); err != nil {
				return "", err
			}
			return "", os.WriteFile(filepath.Join(w, "main.go"), []byte(src), 0o644)
		},
		Prompt: "Verify the package compiles with `go build ./...`.",
		Script: []ScriptStep{
			{Text: "Running build.", ToolCalls: []MockToolCall{
				{Name: "bash", Args: `{"command":"go build ./..."}`},
			}},
			{Text: "Done.", Done: true},
		},
		Verify: func(w string) (bool, string) {
			out, err := runGoBuild(w)
			if err != nil {
				return false, "build: " + out
			}
			return true, ""
		},
	},

	// ---------------------------------------------------------------
	// Multi-step (rename + verify)
	// ---------------------------------------------------------------
	{
		Name:     "fix-and-verify",
		Category: "bugfix",
		Setup: func(w string) (string, error) {
			src := `package math

func Abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
`
			if err := writeGoMod(w); err != nil {
				return "", err
			}
			return "", os.WriteFile(filepath.Join(w, "math.go"), []byte(src), 0o644)
		},
		Prompt: "Abs(-5) should be 5 (it is). Add a comment '// returns absolute value' above the function and run go build to confirm.",
		Script: []ScriptStep{
			{Text: "Add comment.", ToolCalls: []MockToolCall{
				{Name: "insert_file", Args: `{"path":"math.go","after_line":1,"content":"// returns absolute value"}`},
			}},
			{Text: "Verify build.", ToolCalls: []MockToolCall{
				{Name: "bash", Args: `{"command":"go build ./..."}`},
			}},
			{Text: "Done.", Done: true},
		},
		Verify: func(w string) (bool, string) {
			b, _ := os.ReadFile(filepath.Join(w, "math.go"))
			if !strings.Contains(string(b), "returns absolute value") {
				return false, "comment missing"
			}
			out, err := runGoBuild(w)
			if err != nil {
				return false, "build: " + out
			}
			return true, ""
		},
	},

	// ---------------------------------------------------------------
	// Go vet / idiomatic
	// ---------------------------------------------------------------
	{
		Name:     "add-error-return",
		Category: "refactor",
		Setup: func(w string) (string, error) {
			src := `package svc

import "os"

func Read() string {
	b, _ := os.ReadFile("x.txt")
	return string(b)
}
`
			if err := writeGoMod(w); err != nil {
				return "", err
			}
			return "", os.WriteFile(filepath.Join(w, "svc.go"), []byte(src), 0o644)
		},
		Prompt: "Change Read() to return (string, error) instead of ignoring the error.",
		Script: []ScriptStep{
			{Text: "Refactor signature.", ToolCalls: []MockToolCall{
				{Name: "edit_file", Args: `{"path":"svc.go","old_text":"func Read() string {\n\tb, _ := os.ReadFile(\"x.txt\")\n\treturn string(b)\n}","new_text":"func Read() (string, error) {\n\tb, err := os.ReadFile(\"x.txt\")\n\tif err != nil {\n\t\treturn \"\", err\n\t}\n\treturn string(b), nil\n}"}`},
			}},
			{Text: "Done.", Done: true},
		},
		Verify: func(w string) (bool, string) {
			b, _ := os.ReadFile(filepath.Join(w, "svc.go"))
			s := string(b)
			if !strings.Contains(s, "Read() (string, error)") || !strings.Contains(s, "return \"\", err") {
				return false, "signature not updated"
			}
			out, err := runGoBuild(w)
			if err != nil {
				return false, "build: " + out
			}
			return true, ""
		},
	},

	// ---------------------------------------------------------------
	// Create new file
	// ---------------------------------------------------------------
	{
		Name:     "create-test-file",
		Category: "api",
		Setup: func(w string) (string, error) {
			return "", writeGoMod(w)
		},
		Prompt: "Create a file util.go with a function Double(n int) int that returns n*2.",
		Script: []ScriptStep{
			{Text: "Write the file.", ToolCalls: []MockToolCall{
				{Name: "write_file", Args: `{"path":"util.go","content":"package eval_task\n\nfunc Double(n int) int { return n * 2 }\n"}`},
			}},
			{Text: "Done.", Done: true},
		},
		Verify: func(w string) (bool, string) {
			b, err := os.ReadFile(filepath.Join(w, "util.go"))
			if err != nil {
				return false, "util.go not created"
			}
			s := string(b)
			if !strings.Contains(s, "func Double(n int) int") || !strings.Contains(s, "n * 2") {
				return false, "Double not defined"
			}
			out, err := runGoBuild(w)
			if err != nil {
				return false, "build: " + out
			}
			return true, ""
		},
	},

	// ---------------------------------------------------------------
	// Negative case: edit_file with non-matching old_text.
	// The tool should refuse; the file stays unchanged; ExpectFail
	// flips the "verify=false" into a pass.
	// ---------------------------------------------------------------
	{
		Name:       "negative-bad-edit",
		Category:   "negative",
		ExpectFail: true,
		Setup: func(w string) (string, error) {
			return "", os.WriteFile(filepath.Join(w, "f.txt"), []byte("hello\n"), 0o644)
		},
		Prompt: "Replace 'world' with 'Go' in f.txt.",
		Script: []ScriptStep{
			{Text: "Replacing.", ToolCalls: []MockToolCall{
				{Name: "edit_file", Args: `{"path":"f.txt","old_text":"world","new_text":"Go"}`},
			}},
			{Text: "Done.", Done: true},
		},
		Verify: func(w string) (bool, string) {
			b, _ := os.ReadFile(filepath.Join(w, "f.txt"))
			s := string(b)
			if s != "hello\n" {
				return true, "file was unexpectedly modified to: " + s
			}
			return false, "file unchanged (edit_file correctly failed)"
		},
	},
}

// runGoBuild runs `go build ./...` in dir and returns combined output
// plus whether it succeeded.
func runGoBuild(dir string) (string, error) {
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// writeGoMod drops a minimal go.mod into dir so `go build` works for
// tasks that compile Go sources.
func writeGoMod(dir string) error {
	return os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module eval_task\n\ngo 1.22\n"), 0o644)
}
