package eval

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DefaultTasks returns the standard 14-task suite.
func DefaultTasks() []Task {
	return []Task{
		// --- api ---
		{
			Name: "add-health-endpoint", Category: "api",
			Setup: func(workdir string) (string, error) {
				os.MkdirAll(workdir, 0o755)
				writeGoMod(workdir, "testserver")
				src := `package main
import "net/http"
func main() {
	mux := http.NewServeMux()
	_ = mux
	http.ListenAndServe(":8080", mux)
}`
				os.WriteFile(filepath.Join(workdir, "main.go"), []byte(src), 0o644)
				return "Go server scaffold with no /health endpoint", nil
			},
			Prompt: "Add a GET /health endpoint that returns HTTP 200 with body 'ok'. The server is in main.go.",
			Verify: func(workdir string) (bool, string) {
				data, err := os.ReadFile(filepath.Join(workdir, "main.go"))
				if err != nil {
					return false, fmt.Sprintf("read main.go: %v", err)
				}
				s := string(data)
				if !strings.Contains(s, "health") || !strings.Contains(s, "ok") {
					return false, "no /health endpoint found"
				}
				return true, "health endpoint present"
			},
		},
		{
			Name: "add-post-endpoint", Category: "api",
			Setup: func(workdir string) (string, error) {
				os.MkdirAll(workdir, 0o755)
				writeGoMod(workdir, "testserver")
				src := `package main
import "net/http"
func main() { _ = http.HandlerFunc(nil); http.ListenAndServe(":8080", nil) }`
				os.WriteFile(filepath.Join(workdir, "main.go"), []byte(src), 0o644)
				return "Go server scaffold without /echo POST", nil
			},
			Prompt: "Add a POST /echo endpoint that reads the request body and writes it back as the response body. Server is in main.go.",
			Verify: func(workdir string) (bool, string) {
				data, err := os.ReadFile(filepath.Join(workdir, "main.go"))
				if err != nil {
					return false, fmt.Sprintf("read: %v", err)
				}
				s := string(data)
				if !strings.Contains(s, "echo") {
					return false, "no /echo endpoint found"
				}
				return true, "echo endpoint present"
			},
		},
		{
			Name: "create-test-file", Category: "api",
			Setup: func(workdir string) (string, error) {
				os.MkdirAll(workdir, 0o755)
				writeGoMod(workdir, "testpkg")
				return "empty Go module", nil
			},
			Prompt: "Create a file util.go with a function Double(n int) int that returns n*2.",
			Verify: func(workdir string) (bool, string) {
				data, err := os.ReadFile(filepath.Join(workdir, "util.go"))
				if err != nil {
					return false, fmt.Sprintf("util.go not found: %v", err)
				}
				s := string(data)
				if !strings.Contains(s, "func Double") {
					return false, "Double function not found"
				}
				return true, "util.go created with Double"
			},
		},

		// --- bugfix ---
		{
			Name: "fix-off-by-one", Category: "bugfix",
			Setup: func(workdir string) (string, error) {
				os.MkdirAll(workdir, 0o755)
				src := `package main
import "fmt"
func main() {
	for i := 0; i < 10; i++ {
		fmt.Println(i)
	}
}`
				os.WriteFile(filepath.Join(workdir, "main.go"), []byte(src), 0o644)
				return "Loop from 0 to < 10 (should include 10)", nil
			},
			Prompt: "Fix the loop in main.go so it includes 10. Currently it's i < 10 but should be i <= 10.",
			Verify: func(workdir string) (bool, string) {
				data, _ := os.ReadFile(filepath.Join(workdir, "main.go"))
				if strings.Contains(string(data), "i <= 10") {
					return true, "loop fixed"
				}
				return false, "loop still has i < 10"
			},
		},
		{
			Name: "fix-nil-check", Category: "bugfix",
			Setup: func(workdir string) (string, error) {
				os.MkdirAll(workdir, 0o755)
				src := `package main
type Person struct { Name string }
func (p *Person) Greet() string {
	return "hi " + p.Name
}`
				os.WriteFile(filepath.Join(workdir, "greet.go"), []byte(src), 0o644)
				return "Greet() has no nil check", nil
			},
			Prompt: "Fix Greet() in greet.go so it returns 'hi guest' when p is nil.",
			Verify: func(workdir string) (bool, string) {
				data, _ := os.ReadFile(filepath.Join(workdir, "greet.go"))
				s := string(data)
				if strings.Contains(s, "p == nil") || strings.Contains(s, "guest") {
					return true, "nil guard added"
				}
				return false, "no nil check"
			},
		},
		{
			Name: "fix-and-verify", Category: "bugfix",
			Setup: func(workdir string) (string, error) {
				os.MkdirAll(workdir, 0o755)
				writeGoMod(workdir, "testbugfix")
				os.WriteFile(filepath.Join(workdir, "main.go"), []byte(`package main
func main() {}`), 0o644)
				return "bare Go module", nil
			},
			Prompt: "In main.go, add the comment '// Package main is the entry point' before the package line, then ensure it compiles with go build.",
			Verify: func(workdir string) (bool, string) {
				data, _ := os.ReadFile(filepath.Join(workdir, "main.go"))
				s := string(data)
				if strings.Contains(s, "Package main is the entry point") {
					if err := runGoBuild(workdir); err != nil {
						return false, fmt.Sprintf("comment added but build fails: %v", err)
					}
					return true, "comment added and builds"
				}
				return false, "comment not found"
			},
		},

		// --- refactor ---
		{
			Name: "extract-constant", Category: "refactor",
			Setup: func(workdir string) (string, error) {
				os.MkdirAll(workdir, 0o755)
				src := `package main
import "time"
func main() {
	time.Sleep(30 * time.Second)
	time.Sleep(30 * time.Second)
}`
				os.WriteFile(filepath.Join(workdir, "main.go"), []byte(src), 0o644)
				return "hard-coded 30-second sleep", nil
			},
			Prompt: "In main.go, extract the magic number 30 into a constant named DefaultTimeout (30 * time.Second) and use it in both Sleep calls.",
			Verify: func(workdir string) (bool, string) {
				data, _ := os.ReadFile(filepath.Join(workdir, "main.go"))
				s := string(data)
				if strings.Contains(s, "DefaultTimeout") && !strings.Contains(s, "30 * time.Second") {
					return true, "constant extracted"
				}
				return false, "DefaultTimeout not found"
			},
		},
		{
			Name: "rename-symbol", Category: "refactor",
			Setup: func(workdir string) (string, error) {
				os.MkdirAll(workdir, 0o755)
				src := `package main
func fooHelper(a, b int) int { return a + b }
func main() { _ = fooHelper(1, 2) }`
				os.WriteFile(filepath.Join(workdir, "main.go"), []byte(src), 0o644)
				return "fooHelper defined and called", nil
			},
			Prompt: "Rename the function fooHelper to helper in main.go.",
			Verify: func(workdir string) (bool, string) {
				data, _ := os.ReadFile(filepath.Join(workdir, "main.go"))
				s := string(data)
				if strings.Contains(s, "helper") && !strings.Contains(s, "fooHelper") {
					return true, "renamed"
				}
				return false, "fooHelper still present"
			},
		},
		{
			Name: "move-function-to-new-file", Category: "refactor",
			Setup: func(workdir string) (string, error) {
				os.MkdirAll(workdir, 0o755)
				src := `package main
import "fmt"
func Add(a, b int) int { return a + b }
func main() { fmt.Println(Add(1, 2)) }`
				os.WriteFile(filepath.Join(workdir, "main.go"), []byte(src), 0o644)
				return "Add() defined in main.go", nil
			},
			Prompt: "Move the Add function from main.go into a new file math.go. Remove it from main.go.",
			Verify: func(workdir string) (bool, string) {
				mainData, _ := os.ReadFile(filepath.Join(workdir, "main.go"))
				mathData, err := os.ReadFile(filepath.Join(workdir, "math.go"))
				if err != nil {
					return false, "math.go not found"
				}
				if strings.Contains(string(mainData), "func Add") {
					return false, "Add still in main.go"
				}
				if !strings.Contains(string(mathData), "func Add") {
					return false, "Add not in math.go"
				}
				return true, "Add moved to math.go"
			},
		},
		{
			Name: "add-error-return", Category: "refactor",
			Setup: func(workdir string) (string, error) {
				os.MkdirAll(workdir, 0o755)
				src := `package main
func Read(path string) string { return "" }`
				os.WriteFile(filepath.Join(workdir, "main.go"), []byte(src), 0o644)
				return "Read returns string only", nil
			},
			Prompt: "Change Read() in main.go to return (string, error) instead of just string. Return nil as the error.",
			Verify: func(workdir string) (bool, string) {
				data, _ := os.ReadFile(filepath.Join(workdir, "main.go"))
				s := string(data)
				if strings.Contains(s, "(string, error)") {
					return true, "return type updated"
				}
				return false, "still returns string only"
			},
		},

		// --- edit ---
		{
			Name: "replace-all-occurrences", Category: "edit",
			Setup: func(workdir string) (string, error) {
				os.MkdirAll(workdir, 0o755)
				os.WriteFile(filepath.Join(workdir, "v.txt"), []byte("aaa\naba\naca"), 0o644)
				return "file with three 'a' characters per line", nil
			},
			Prompt: "Replace all occurrences of 'a' with 'x' in v.txt.",
			Verify: func(workdir string) (bool, string) {
				data, _ := os.ReadFile(filepath.Join(workdir, "v.txt"))
				s := string(data)
				if strings.Contains(s, "a") {
					return false, "'a' still present"
				}
				if strings.Count(s, "x") >= 3 {
					return true, "all 'a' replaced"
				}
				return false, "unexpected content"
			},
		},
		{
			Name: "insert-at-line", Category: "edit",
			Setup: func(workdir string) (string, error) {
				os.MkdirAll(workdir, 0o755)
				os.WriteFile(filepath.Join(workdir, "l.txt"), []byte("line 1\nline 2\nline 3"), 0o644)
				return "3-line file", nil
			},
			Prompt: "Insert the text 'INSERTED' after line 2 in l.txt.",
			Verify: func(workdir string) (bool, string) {
				data, _ := os.ReadFile(filepath.Join(workdir, "l.txt"))
				s := string(data)
				if strings.Contains(s, "INSERTED") && strings.Count(s, "\n") >= 3 {
					return true, "line inserted"
				}
				return false, "INSERTED not found"
			},
		},

		// --- bash ---
		{
			Name: "build-check", Category: "bash",
			Setup: func(workdir string) (string, error) {
				os.MkdirAll(workdir, 0o755)
				writeGoMod(workdir, "testbuild")
				os.WriteFile(filepath.Join(workdir, "main.go"), []byte(`package main
func main() {}`), 0o644)
				return "valid Go module", nil
			},
			Prompt: "Verify this project compiles by running 'go build ./...' in bash.",
			Verify: func(workdir string) (bool, string) {
				err := runGoBuild(workdir)
				if err == nil {
					return true, "build succeeds"
				}
				return false, fmt.Sprintf("build fails: %v", err)
			},
		},

		// --- negative ---
		{
			Name: "negative-bad-edit", Category: "negative", ExpectFail: true,
			Setup: func(workdir string) (string, error) {
				os.MkdirAll(workdir, 0o755)
				os.WriteFile(filepath.Join(workdir, "f.txt"), []byte("hello world"), 0o644)
				return "file containing 'hello world'", nil
			},
			Prompt: "Use edit_file to replace 'goodbye' with 'hello' in f.txt. This should fail because old_text doesn't exist.",
			Verify: func(workdir string) (bool, string) {
				data, _ := os.ReadFile(filepath.Join(workdir, "f.txt"))
				s := string(data)
				if strings.Contains(s, "goodbye") {
					return false, "file was unexpectedly modified"
				}
				return true, "file correctly untouched (edit should have failed)"
			},
		},
	}
}

func runGoBuild(dir string) error {
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, string(out))
	}
	return nil
}

func writeGoMod(workdir, module string) error {
	s := fmt.Sprintf("module %s\n\ngo 1.25\n", module)
	return os.WriteFile(filepath.Join(workdir, "go.mod"), []byte(s), 0o644)
}
