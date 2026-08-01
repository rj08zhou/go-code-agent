package utils

import "testing"

func TestTerminalColorsAllowed(t *testing.T) {
	tests := []struct {
		name                 string
		stdoutTTY, stderrTTY bool
		noColor              bool
		want                 bool
	}{
		{name: "interactive outputs", stdoutTTY: true, stderrTTY: true, want: true},
		{name: "NO_COLOR", stdoutTTY: true, stderrTTY: true, noColor: true},
		{name: "redirected stdout", stderrTTY: true},
		{name: "redirected stderr", stdoutTTY: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := terminalColorsAllowed(tc.stdoutTTY, tc.stderrTTY, tc.noColor); got != tc.want {
				t.Fatalf("terminalColorsAllowed() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNoColorRequestedTreatsEmptyValueAsPresent(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	if !noColorRequested() {
		t.Fatal("empty NO_COLOR value must disable color")
	}
}
