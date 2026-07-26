package tool

import (
	"encoding/json"
	"testing"
)

func TestToolCatalogSubsetPreservesOrderAndFilters(t *testing.T) {
	c := NewToolCatalog()
	noop := func(scope *ToolScope, args json.RawMessage) Result { return Succeeded("ok") }
	c.RegisterAll([]ToolDefinition{
		{Name: "bash", Handler: noop},
		{Name: "read_file", Handler: noop},
		{Name: "write_file", Handler: noop},
		{Name: "list_dir", Handler: noop},
		{Name: "delete_file", Handler: noop},
		{Name: "search_file", Handler: noop},
	})

	sub := c.Subset("bash", "read_file", "list_dir", "search_file", "missing_tool")
	defs := sub.LLMToolDefs()
	if len(defs) != 4 {
		t.Fatalf("got %d tools, want 4", len(defs))
	}
	want := []string{"bash", "read_file", "list_dir", "search_file"}
	for i, name := range want {
		if defs[i].Name != name {
			t.Fatalf("order[%d]=%q, want %q", i, defs[i].Name, name)
		}
	}
	if sub.IsKnown("write_file") || sub.IsKnown("delete_file") {
		t.Fatal("subset must not include write/delete tools")
	}
}
