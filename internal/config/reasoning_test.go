package config

import "testing"

func TestLoadDeepSeekConfig(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", " ds-key ")
	t.Setenv("DEEPSEEK_BASE_URL", " https://api.deepseek.com ")
	cfg := Load()
	if cfg.DeepSeekAPIKey != "ds-key" {
		t.Fatalf("DeepSeekAPIKey = %q", cfg.DeepSeekAPIKey)
	}
	if cfg.DeepSeekBaseURL != "https://api.deepseek.com" {
		t.Fatalf("DeepSeekBaseURL = %q", cfg.DeepSeekBaseURL)
	}
}

func TestValidateAcceptsDeepSeekKey(t *testing.T) {
	cfg := &Config{DeepSeekAPIKey: "ds-key"}
	if warns := cfg.Validate(); len(warns) != 0 {
		t.Fatalf("Validate = %v", warns)
	}
}

func TestLoadReasoningConfig(t *testing.T) {
	t.Setenv("REASONING_ENABLED", "true")
	t.Setenv("REASONING_EFFORT", " HIGH ")
	cfg := Load()
	if !cfg.ReasoningEnabled {
		t.Fatal("REASONING_ENABLED was not loaded")
	}
	if cfg.ReasoningEffort != "HIGH" {
		t.Fatalf("ReasoningEffort = %q", cfg.ReasoningEffort)
	}
}

func TestLoadReasoningConfigDefaultsDisabledWithMediumEffort(t *testing.T) {
	t.Setenv("REASONING_ENABLED", "")
	t.Setenv("REASONING_EFFORT", "")
	cfg := Load()
	if cfg.ReasoningEnabled {
		t.Fatal("reasoning must be opt-in")
	}
	if cfg.ReasoningEffort != "medium" {
		t.Fatalf("default ReasoningEffort = %q, want medium", cfg.ReasoningEffort)
	}
}
