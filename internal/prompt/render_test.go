package prompt

import (
	"strings"
	"testing"
)

func TestRenderReplacesAndSortsKeys(t *testing.T) {
	got := Render("a={{a}} b={{b}}", map[string]string{"b": "2", "a": "1"})
	if got != "a=1 b=2" {
		t.Fatalf("got %q", got)
	}
}

func TestRenderPanicsOnMissingData(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "{{missing}}") {
			t.Fatalf("panic = %v", r)
		}
	}()
	_ = Render("hello {{missing}}", map[string]string{"other": "x"})
}

func TestRenderIgnoresPlaceholdersInValues(t *testing.T) {
	// Runtime data (e.g. user-typed HITL feedback) may contain {{...}} tokens;
	// they must pass through verbatim without panicking or re-substitution.
	got := Render("fb: {{feedback}}", map[string]string{
		"feedback": "please fix the {{path}} arg",
		"path":     "SHOULD-NOT-APPEAR",
	})
	want := "fb: please fix the {{path}} arg"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestUnreplacedPlaceholders(t *testing.T) {
	got := UnreplacedPlaceholders("keep {{a}} and {{b}} and {{a}}")
	if len(got) != 2 || got[0] != "{{a}}" || got[1] != "{{b}}" {
		t.Fatalf("got %#v", got)
	}
}

func TestAllTemplatesRenderWithKnownKeys(t *testing.T) {
	// Smoke: every template that declares placeholders can be rendered with
	// dummy values without leaving {{ tokens. Coverage is enforced below:
	// adding a template with placeholders without a case here fails the test.
	cases := map[string]map[string]string{
		"system": {
			"workdir": "/tmp/ws", "skills": "none",
			"env_context": "<env>fixed</env>", "skill_context": "",
		},
		"strategy_change": {"tool": "bash", "count": "3"},
		"dag_required":    {"count": "2"},
		"explore":         {"role": "explore"},
		"web_fetch":       {"role": "web_fetch"},
		"teammate": {
			"name": "alice", "role": "dev", "team": "t", "workdir": "/w",
		},
		"judge_system": {
			"min_score": "7", "original_task": "t",
			"recent_conversation": "c", "tool_results": "",
		},
		"session_to_memory": {"session_history": "hist"},
		"human_reject":      {"tool": "bash", "reason": "no"},
		"human_modify":      {"tool": "bash", "feedback": "edit args"},
	}
	l := NewLoader()

	// Mechanism: walk ALL embedded templates; any template that declares
	// placeholders MUST have a rendering case above. This turns "new template
	// needs a render test" into an enforced rule instead of a convention.
	for _, name := range l.Names() {
		body := l.MustLoad(name)
		placeholders := UnreplacedPlaceholders(body)
		data, covered := cases[name]
		if len(placeholders) == 0 {
			if covered {
				t.Errorf("template %q has no placeholders; remove its stale case", name)
			}
			continue
		}
		if !covered {
			t.Errorf("template %q declares placeholders %v but has no render case", name, placeholders)
			continue
		}
		out := Render(body, data)
		if left := UnreplacedPlaceholders(out); len(left) > 0 {
			t.Errorf("%s left placeholders %v", name, left)
		}
	}
	for name := range cases {
		if l.Load(name) == "" {
			t.Errorf("render case %q refers to a template that no longer exists", name)
		}
	}
}
