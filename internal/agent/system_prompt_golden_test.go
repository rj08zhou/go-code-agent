package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-code-agent/internal/prompt"
)

const goldenEnvContext = `<env>
Platform: test/arch
OS Version: TestOS 1.0
Today's date: Monday, Jan 2, 2006
Working directory: /workspace/demo
Is directory a git repo: yes
Current branch: main
Model: test-model
</env>`

func TestSystemPromptGolden(t *testing.T) {
	b := NewSystemPromptBuilder(prompt.NewLoader(), nil, nil)
	got := b.BuildWith(SystemPromptVars{
		Workdir:      "/workspace/demo",
		Skills:       "demo-skill",
		EnvContext:   goldenEnvContext,
		SkillContext: "demo-skill: a fixture skill summary",
	})

	if left := prompt.UnreplacedPlaceholders(got); len(left) > 0 {
		t.Fatalf("unreplaced placeholders: %v", left)
	}
	for _, want := range []string{
		"You are a coding agent at /workspace/demo",
		"Skills: demo-skill",
		"## Output Style",
		"## Coding Conventions",
		"## Safety & Untrusted Content",
		"## Exploration Strategy",
		"## Planning",
		"## Multi-Agent",
		"<example>",
		"Incorrect: `explore` or `list_dir`",
		"Incorrect: `task_create` a 5-node DAG",
		"demo-skill: a fixture skill summary",
		"Model: test-model",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("golden candidate missing %q", want)
		}
	}
	for _, banned := range []string{"(CRITICAL", "NEVER invent", "CRITICAL for parallel"} {
		if strings.Contains(got, banned) {
			t.Errorf("tone inflation remnant %q still present", banned)
		}
	}

	path := filepath.Join("testdata", "system_prompt.golden")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", path)
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run UPDATE_GOLDEN=1 to create): %v", err)
	}
	if got != string(want) {
		t.Fatalf("system prompt drifted from golden\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
