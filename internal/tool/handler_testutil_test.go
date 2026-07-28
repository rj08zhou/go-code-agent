package tool

import (
	"testing"
)

func mustTool(t *testing.T, defs []ToolDefinition, name string) ToolDefinition {
	t.Helper()
	for _, d := range defs {
		if d.Name == name {
			if d.Handler == nil {
				t.Fatalf("tool %q has nil handler", name)
			}
			return d
		}
	}
	t.Fatalf("tool %q not found", name)
	return ToolDefinition{}
}
