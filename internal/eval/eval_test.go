package eval

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestHarness_MockRun(t *testing.T) {
	h := Harness{
		Tasks: []Task{{
			Name:     "mock-test",
			Category: "mock",
			Script: []ScriptStep{
				{Text: "I will do the task", Done: true},
			},
			Setup: func(workdir string) (string, error) {
				os.WriteFile(workdir+"/test.txt", []byte("hello"), 0o644)
				return "created test.txt", nil
			},
			Verify: func(workdir string) (bool, string) {
				data, err := os.ReadFile(workdir + "/test.txt")
				if err != nil {
					return false, err.Error()
				}
				return string(data) == "hello", "file matches"
			},
		}},
		Verbose: false,
	}

	s := h.Run(context.Background())
	if s.Total != 1 {
		t.Fatalf("expected 1 task, got %d", s.Total)
	}
	if s.Passed != 1 {
		t.Fatalf("expected 1 pass, got %d", s.Passed)
	}
}

func TestHarness_NegativeTest(t *testing.T) {
	h := Harness{
		Tasks: []Task{{
			Name: "negative-test", Category: "negative", ExpectFail: true,
			Script: []ScriptStep{{Done: true}},
			Setup: func(workdir string) (string, error) {
				os.WriteFile(workdir+"/f.txt", []byte("unchanged"), 0o644)
				return "created f.txt", nil
			},
			Verify: func(workdir string) (bool, string) {
				// Return false (failure) — but ExpectFail=true, so this is a pass
				return false, "expected verification failure"
			},
		}},
	}

	s := h.Run(context.Background())
	if s.Passed != 1 {
		t.Fatalf("negative test should pass (failure was expected), got passed=%d", s.Passed)
	}
	if !s.Results[0].Success {
		t.Fatalf("negative test result should be marked success")
	}
}

func TestSummary_Report(t *testing.T) {
	s := Summary{
		Total: 2, Passed: 1, SuccessRate: 50,
		ByCategory: map[string]CategoryStat{
			"bugfix":   {Total: 1, Passed: 1},
			"refactor": {Total: 1, Passed: 0},
		},
		Results: []Result{
			{Name: "test1", Category: "bugfix", Success: true},
			{Name: "test2", Category: "refactor", Success: false, Error: "verify failed"},
		},
	}
	report := s.Report()
	if !contains(report, "Test1") && !contains(report, "test1") {
		t.Fatalf("report should mention test names: %s", report)
	}
	if !contains(report, "PASS") {
		t.Fatalf("report should show PASS: %s", report)
	}
}

func TestSummary_WriteJSON(t *testing.T) {
	s := Summary{Total: 1, Passed: 1, SuccessRate: 100}
	path := t.TempDir() + "/results.json"
	if err := s.WriteJSON(path); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("results.json not created: %v", err)
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
