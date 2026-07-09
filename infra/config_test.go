package infra

import "testing"

func TestContextWindowTokens(t *testing.T) {
	// Guard: these assume no CONTEXT_WINDOW_TOKENS override is set in the
	// test environment (Cfg.ContextWindowOverride == 0).
	if Cfg != nil && Cfg.ContextWindowOverride != 0 {
		t.Skip("CONTEXT_WINDOW_TOKENS override set; skipping prefix-inference test")
	}
	cases := map[string]int{
		"claude-opus-4.7":    ContextWindowClaude,
		"claude-3-5-sonnet":  ContextWindowClaude,
		"gpt-4o":             ContextWindowGPT,
		"gpt-4.1-mini":       ContextWindowGPT,
		"o3-mini":            ContextWindowGPT,
		"gemini-2.0-flash":   ContextWindowGemini,
		"some-unknown-model": ContextWindowDefault,
		"":                   ContextWindowDefault,
	}
	for model, want := range cases {
		if got := ContextWindowTokens(model); got != want {
			t.Errorf("ContextWindowTokens(%q) = %d, want %d", model, got, want)
		}
	}
}

func TestCompactionThreshold(t *testing.T) {
	if Cfg != nil && Cfg.ContextWindowOverride != 0 {
		t.Skip("CONTEXT_WINDOW_TOKENS override set; skipping")
	}
	// Claude: 200000 * 0.75 = 150000, below the 300000 cap -> use it.
	if got := CompactionThreshold("claude-opus-4.7"); got != int(float64(ContextWindowClaude)*CompactionThresholdFrac) {
		t.Errorf("claude threshold = %d, want %d", got, int(float64(ContextWindowClaude)*CompactionThresholdFrac))
	}
	// Gemini: 1000000 * 0.75 = 750000, above the 300000 cap -> capped.
	if got := CompactionThreshold("gemini-2.0-flash"); got != TokenThreshold {
		t.Errorf("gemini threshold = %d, want capped %d", got, TokenThreshold)
	}
	// Threshold must always be positive and never exceed the cap.
	for _, m := range []string{"claude-x", "gpt-4o", "gemini-x", "unknown"} {
		got := CompactionThreshold(m)
		if got <= 0 || got > TokenThreshold {
			t.Errorf("CompactionThreshold(%q) = %d, out of (0, %d]", m, got, TokenThreshold)
		}
	}
}
