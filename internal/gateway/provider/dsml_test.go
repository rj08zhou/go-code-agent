package provider

import (
	"testing"
)

func TestParseDSMLToolCalls(t *testing.T) {
	input := "前置说明\n<｜DSML｜tool_calls>\n" +
		"<｜DSML｜invoke name=\"search_file\">\n" +
		"<｜DSML｜parameter name=\"pattern\" string=\"true\">*_test.go</｜DSML｜parameter>\n" +
		"<｜DSML｜parameter name=\"path\" string=\"true\">internal/agent</｜DSML｜parameter>\n" +
		"</｜DSML｜invoke>\n" +
		"</｜DSML｜tool_calls>\n后置说明"

	clean, calls, parsed := parseDSMLToolCalls(input)
	if !parsed {
		t.Fatal("expected DSML response to be parsed")
	}
	if clean != "前置说明\n\n后置说明" {
		t.Fatalf("clean content = %q", clean)
	}
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if calls[0].ID != "dsml_call_0" || calls[0].Name != "search_file" {
		t.Fatalf("unexpected call metadata: %+v", calls[0])
	}
	if calls[0].Arguments != `{"path":"internal/agent","pattern":"*_test.go"}` && calls[0].Arguments != `{"pattern":"*_test.go","path":"internal/agent"}` {
		t.Fatalf("unexpected arguments: %s", calls[0].Arguments)
	}
}

func TestParseDSMLToolCallsLeavesIncompleteContentUntouched(t *testing.T) {
	input := "<｜DSML｜tool_calls><｜DSML｜invoke name=\"read_file\">"
	clean, calls, parsed := parseDSMLToolCalls(input)
	if parsed || len(calls) != 0 || clean != input {
		t.Fatalf("incomplete DSML should remain untouched: clean=%q calls=%v parsed=%v", clean, calls, parsed)
	}
}
