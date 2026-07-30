package config

import "testing"

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
