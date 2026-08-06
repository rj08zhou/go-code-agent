package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-code-agent/internal/llm"
)

func TestReadFileRemapsHostAbsoluteIntoWorktree(t *testing.T) {
	host := t.TempDir()
	wt := t.TempDir()
	rel := filepath.Join("cmd", "agent", "main.go")
	if err := os.MkdirAll(filepath.Join(wt, "cmd", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, rel), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hostAbs := filepath.Join(host, rel)

	catalog := NewToolCatalog()
	catalog.RegisterAll(filesystemReadTools(builtinDeps{}))
	exec := NewExecutor(catalog, nil, nil)
	scope := &ToolScope{
		Role:          "teammate",
		Workdir:       wt,
		SourceWorkdir: host,
		CanRead:       true,
	}
	args, _ := json.Marshal(map[string]any{"path": hostAbs, "limit": 5})
	result := exec.Execute(context.Background(), scope, llm.ToolCall{Name: "read_file", Arguments: string(args)})
	if result.Status != StatusSucceeded {
		t.Fatalf("status=%s output=%q", result.Status, result.Output)
	}
	if !strings.Contains(result.Output, "package main") {
		t.Fatalf("output = %q", result.Output)
	}
}

func TestReadFileStillRejectsOutsideHostAndWorktree(t *testing.T) {
	host := t.TempDir()
	wt := t.TempDir()
	outside := filepath.Join(t.TempDir(), "passwd")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	catalog := NewToolCatalog()
	catalog.RegisterAll(filesystemReadTools(builtinDeps{}))
	exec := NewExecutor(catalog, nil, nil)
	scope := &ToolScope{
		Role:          "teammate",
		Workdir:       wt,
		SourceWorkdir: host,
		CanRead:       true,
	}
	args, _ := json.Marshal(map[string]any{"path": outside})
	result := exec.Execute(context.Background(), scope, llm.ToolCall{Name: "read_file", Arguments: string(args)})
	if result.Status == StatusSucceeded {
		t.Fatal("outside path must fail")
	}
	if !strings.Contains(result.Output, "escapes workdir") {
		t.Fatalf("output = %q", result.Output)
	}
}
