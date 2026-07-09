package agent

import (
	"context"
	"go-code-agent/infra"
	"os"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// NewJudge / enable toggling
// ----------------------------------------------------------------------------

func TestNewJudge_DefaultsMinScore(t *testing.T) {
	j := NewJudge(true, "gpt-test", 0, nil)
	if j.minScore != 7 {
		t.Errorf("minScore = %d, want default 7 when <= 0 is passed", j.minScore)
	}

	j2 := NewJudge(false, "", 3, nil)
	if j2.minScore != 3 {
		t.Errorf("minScore = %d, want 3 (explicit value should be kept)", j2.minScore)
	}
}

func TestJudge_EnabledToggle(t *testing.T) {
	j := NewJudge(false, "", 7, nil)
	if j.IsEnabled() {
		t.Fatal("expected judge to start disabled")
	}
	j.SetEnabled(true)
	if !j.IsEnabled() {
		t.Fatal("expected judge to be enabled after SetEnabled(true)")
	}
	j.SetEnabled(false)
	if j.IsEnabled() {
		t.Fatal("expected judge to be disabled after SetEnabled(false)")
	}
}

func TestJudge_Verify_DisabledIsNoop(t *testing.T) {
	j := NewJudge(false, "", 7, nil)
	verdict, err := j.Verify(context.Background(), "do something", nil, nil, "main-model")
	if err != nil {
		t.Fatalf("Verify on disabled judge returned error: %v", err)
	}
	if verdict == nil || !verdict.Approved || verdict.Score != 10 {
		t.Errorf("Verify on disabled judge = %+v, want permissive pass-through", verdict)
	}
}

// ----------------------------------------------------------------------------
// Judge configuration parsing (now centralized in infra.Config)
// ----------------------------------------------------------------------------

func withEnv(t *testing.T, kv map[string]string, fn func()) {
	t.Helper()
	for k, v := range kv {
		old, had := os.LookupEnv(k)
		if v == "" {
			os.Unsetenv(k)
		} else {
			os.Setenv(k, v)
		}
		t.Cleanup(func() {
			if had {
				os.Setenv(k, old)
			} else {
				os.Unsetenv(k)
			}
		})
	}
	fn()
}

func TestJudgeConfigFromEnv(t *testing.T) {
	cases := []struct {
		name         string
		env          map[string]string
		wantEnabled  bool
		wantModel    string
		wantMinScore int
	}{
		{
			name:         "all unset -> disabled with infra default min score",
			env:          map[string]string{"JUDGE_ENABLED": "", "JUDGE_MODEL": "", "JUDGE_MIN_SCORE": ""},
			wantEnabled:  false,
			wantModel:    "",
			wantMinScore: 0, // filled from infra.JudgeMinScore below
		},
		{
			name:         "enabled=true (lowercase)",
			env:          map[string]string{"JUDGE_ENABLED": "true", "JUDGE_MODEL": "", "JUDGE_MIN_SCORE": ""},
			wantEnabled:  true,
			wantModel:    "",
			wantMinScore: 0,
		},
		{
			name:        "enabled=1",
			env:         map[string]string{"JUDGE_ENABLED": "1"},
			wantEnabled: true,
		},
		{
			name:         "enabled=yes with explicit model + min score",
			env:          map[string]string{"JUDGE_ENABLED": "yes", "JUDGE_MODEL": "cheap-model", "JUDGE_MIN_SCORE": "5"},
			wantEnabled:  true,
			wantModel:    "cheap-model",
			wantMinScore: 5,
		},
		{
			name:        "invalid min score falls back to infra default",
			env:         map[string]string{"JUDGE_ENABLED": "on", "JUDGE_MIN_SCORE": "not-a-number"},
			wantEnabled: true,
		},
		{
			name:        "garbage enabled value stays disabled",
			env:         map[string]string{"JUDGE_ENABLED": "nope"},
			wantEnabled: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withEnv(t, map[string]string{
				"JUDGE_ENABLED":   tc.env["JUDGE_ENABLED"],
				"JUDGE_MODEL":     tc.env["JUDGE_MODEL"],
				"JUDGE_MIN_SCORE": tc.env["JUDGE_MIN_SCORE"],
			}, func() {
				c := infra.Load()
				if c.JudgeEnabled != tc.wantEnabled {
					t.Errorf("enabled = %v, want %v", c.JudgeEnabled, tc.wantEnabled)
				}
				if tc.wantModel != "" && c.JudgeModel != tc.wantModel {
					t.Errorf("model = %q, want %q", c.JudgeModel, tc.wantModel)
				}
				if tc.wantMinScore != 0 && c.JudgeMinScore != tc.wantMinScore {
					t.Errorf("minScore = %d, want %d", c.JudgeMinScore, tc.wantMinScore)
				}
				if c.JudgeMinScore <= 0 {
					t.Errorf("minScore should never be <= 0, got %d", c.JudgeMinScore)
				}
			})
		})
	}
}

// ----------------------------------------------------------------------------
// parseJudgeResponse
// ----------------------------------------------------------------------------

func TestParseJudgeResponse_Valid(t *testing.T) {
	raw := `Here is my verdict:
{"approved": true, "score": 8, "issues": ["minor formatting"], "suggestions": ["add tests"], "should_retry": false, "reason": "looks good"}
Thanks.`
	v, err := parseJudgeResponse(raw)
	if err != nil {
		t.Fatalf("parseJudgeResponse returned error: %v", err)
	}
	if !v.Approved || v.Score != 8 || v.Reason != "looks good" {
		t.Errorf("unexpected verdict: %+v", v)
	}
	if len(v.Issues) != 1 || v.Issues[0] != "minor formatting" {
		t.Errorf("unexpected issues: %v", v.Issues)
	}
	if len(v.Suggestions) != 1 || v.Suggestions[0] != "add tests" {
		t.Errorf("unexpected suggestions: %v", v.Suggestions)
	}
}

func TestParseJudgeResponse_ScoreClamping(t *testing.T) {
	tooLow := `{"approved": false, "score": -5, "reason": "bad"}`
	v, err := parseJudgeResponse(tooLow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Score != 1 {
		t.Errorf("score = %d, want clamped to 1", v.Score)
	}

	tooHigh := `{"approved": true, "score": 999, "reason": "great"}`
	v2, err := parseJudgeResponse(tooHigh)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v2.Score != 10 {
		t.Errorf("score = %d, want clamped to 10", v2.Score)
	}
}

func TestParseJudgeResponse_NoJSONObject(t *testing.T) {
	_, err := parseJudgeResponse("no braces here at all")
	if err == nil {
		t.Fatal("expected error when response has no JSON object")
	}
}

func TestParseJudgeResponse_MalformedJSON(t *testing.T) {
	_, err := parseJudgeResponse(`{"approved": true, "score": }`)
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "unmarshal failed") {
		t.Errorf("error = %v, want it to mention unmarshal failure", err)
	}
}

// ----------------------------------------------------------------------------
// permissiveVerdict / FormatFeedback
// ----------------------------------------------------------------------------

func TestPermissiveVerdict(t *testing.T) {
	v := permissiveVerdict("infra hiccup")
	if !v.Approved || v.ShouldRetry {
		t.Errorf("permissiveVerdict should approve and never force retry, got %+v", v)
	}
	if v.Reason != "infra hiccup" {
		t.Errorf("Reason = %q, want %q", v.Reason, "infra hiccup")
	}
}

func TestJudgeVerdict_FormatFeedback(t *testing.T) {
	v := &JudgeVerdict{
		Approved:    false,
		Score:       3,
		Reason:      "missed edge case",
		Issues:      []string{"issue A", "issue B"},
		Suggestions: []string{"do X"},
	}
	out := v.FormatFeedback()
	for _, want := range []string{
		"<verification-failed>", "Judge score: 3/10", "missed edge case",
		"issue A", "issue B", "do X", "</verification-failed>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("FormatFeedback output missing %q; got:\n%s", want, out)
		}
	}
}
