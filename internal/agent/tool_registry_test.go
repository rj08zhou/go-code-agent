package agent

import "testing"

// TestInitTools_RegistryConsistency locks in the invariant ToolSpec /
// registerToolSpec exist to guarantee: every tool advertised to the
// LLM (ToolDefs) has both an executable Handler and an explicit
// security Level, and neither ToolHandlers nor ToolSecurityMap
// contains an entry for a tool that isn't actually advertised.
//
// Before the ToolSpec unification, these three registries were
// maintained by hand in different places and drifted in both
// directions: several tools (spawn_teammate, send_message,
// read_inbox, broadcast, shutdown_request, plan_approval, claim_task,
// check_background, session_save_memory, list_teammates) had a Def +
// Handler but no ToolSecurityMap entry, which checkToolApproval
// treats as "unknown tool" - i.e. permanently unusable. Conversely,
// ToolSecurityMap carried entries (search_content, search_file,
// list_dir, memory_stats, execute_command) for tools this agent never
// registers at all. This test fails loudly if that drift ever comes
// back.
func TestInitTools_RegistryConsistency(t *testing.T) {
	// InitTools mutates package-level state; save/restore so this test
	// can't leak into others if run in the same binary.
	savedApp := App
	savedDefs, savedHandlers, savedSecurity := ToolDefs, ToolHandlers, ToolSecurityMap
	t.Cleanup(func() {
		App = savedApp
		ToolDefs, ToolHandlers, ToolSecurityMap = savedDefs, savedHandlers, savedSecurity
	})

	// Exercise the registry without depending on MCP config/state:
	// an App whose MCPMgr is nil skips the MCP-tool append in InitTools.
	App = &AppContext{}
	ToolDefs = nil
	ToolHandlers = nil
	ToolSecurityMap = map[string]ToolSecurityMeta{}

	InitTools()

	if len(ToolDefs) == 0 {
		t.Fatal("InitTools() produced no tool definitions")
	}

	defNames := make(map[string]bool, len(ToolDefs))
	for _, d := range ToolDefs {
		if defNames[d.Name] {
			t.Errorf("tool %q registered more than once", d.Name)
		}
		defNames[d.Name] = true

		if _, ok := ToolHandlers[d.Name]; !ok {
			t.Errorf("tool %q has a Def but no Handler", d.Name)
		}
		if _, ok := ToolSecurityMap[d.Name]; !ok {
			t.Errorf("tool %q has a Def but no security Level - checkToolApproval would treat it as an unknown tool and permanently block it", d.Name)
		}
	}

	for name := range ToolHandlers {
		if !defNames[name] {
			t.Errorf("tool %q has a Handler but is not advertised in ToolDefs", name)
		}
	}
	for name := range ToolSecurityMap {
		if !defNames[name] {
			t.Errorf("tool %q has a security Level but is not advertised in ToolDefs (dead entry)", name)
		}
	}
}

// TestInitTools_Idempotent ensures repeated InitTools() calls don't
// accumulate tool definitions — each call must fully rebuild from empty.
func TestInitTools_Idempotent(t *testing.T) {
	savedApp := App
	savedDefs, savedHandlers, savedSecurity := ToolDefs, ToolHandlers, ToolSecurityMap
	t.Cleanup(func() {
		App = savedApp
		ToolDefs, ToolHandlers, ToolSecurityMap = savedDefs, savedHandlers, savedSecurity
	})

	App = &AppContext{} // MCPMgr nil => no MCP tools appended
	ToolDefs = nil
	ToolHandlers = nil
	ToolSecurityMap = map[string]ToolSecurityMeta{}

	InitTools()
	firstCount := len(ToolDefs)
	firstNames := map[string]bool{}
	for _, d := range ToolDefs {
		firstNames[d.Name] = true
	}
	if firstCount == 0 {
		t.Fatal("first InitTools() produced no tool definitions")
	}

	// Second call must NOT grow the registries — it rebuilds from empty.
	InitTools()
	secondCount := len(ToolDefs)

	if secondCount != firstCount {
		t.Fatalf("InitTools() is not idempotent: first call produced %d defs, second produced %d (ToolDefs not reset between calls — tool schema would accumulate and inflate per-request tokens)",
			firstCount, secondCount)
	}

	// Names must still be unique after the second call.
	seen := map[string]bool{}
	for _, d := range ToolDefs {
		if seen[d.Name] {
			t.Fatalf("tool %q appears more than once in ToolDefs after repeated InitTools() — registry was not reset", d.Name)
		}
		seen[d.Name] = true
	}

	// Every advertised tool must still resolve to a handler + security level.
	for _, d := range ToolDefs {
		if _, ok := ToolHandlers[d.Name]; !ok {
			t.Errorf("after second InitTools(): tool %q has a Def but no Handler", d.Name)
		}
		if _, ok := ToolSecurityMap[d.Name]; !ok {
			t.Errorf("after second InitTools(): tool %q has a Def but no security Level", d.Name)
		}
	}
}
