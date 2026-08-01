package prompt

import (
	"strings"
	"testing"
)

func TestLoaderEmbedsAllTemplates(t *testing.T) {
	l := NewLoader()
	embedded := EmbeddedCount()
	if embedded == 0 {
		t.Fatal("embed FS has zero .md templates")
	}
	if l.Count() != embedded {
		t.Fatalf("loader count %d != embed count %d; names=%v", l.Count(), embedded, l.Names())
	}
	for _, name := range l.Names() {
		body := l.Load(name)
		if strings.TrimSpace(body) == "" {
			t.Errorf("template %q is empty", name)
		}
	}
}

func TestRequiredTemplatesPresent(t *testing.T) {
	required := []string{
		"system",
		"auto_lesson",
		"session_to_memory",
		"judge_critical",
		"judge_system",
		"planning_required",
		"strategy_change",
		"todo_nag",
		"teammate",
		"explore",
		"web_fetch",
		"dag_required",
		"human_reject",
		"human_modify",
		"compaction",
		"post_explore",
		"response_truncated",
	}
	l := NewLoader()
	for _, name := range required {
		if strings.TrimSpace(l.Load(name)) == "" {
			t.Errorf("required template %q missing or empty", name)
		}
	}
}

func TestMustLoadPanicsOnMissing(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for missing template")
		}
	}()
	NewLoader().MustLoad("does_not_exist")
}
