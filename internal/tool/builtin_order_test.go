package tool

import (
	"testing"
)

// goldenBuiltinOrder locks the BuiltinTools registration order.
// Changing this order can invalidate LLM prompt-prefix caches.
var goldenBuiltinOrder = []string{
	"bash",
	"read_file",
	"write_file",
	"edit_file",
	"delete_file",
	"insert_file",
	"list_dir",
	"search_file",
	"search_content",
	"TodoWrite",
	"task_create",
	"task_list",
	"task_update",
	"task_get",
	"task_add_dep",
	"task_remove_dep",
	"task_ready",
	"task_dag",
	"claim_task",
	"memory_write",
	"memory_search",
	"memory_delete",
	"memory_stats",
	"session_save_memory",
	"background_run",
	"check_background",
	"spawn_teammate",
	"list_teammates",
	"send_message",
	"read_inbox",
	"broadcast",
	"shutdown_request",
	"plan_approval",
	"submit_plan",
	"compress",
	"load_skill",
	"web_fetch",
	"web_search",
	"explore",
}

func TestBuiltinTools_OrderAndCount(t *testing.T) {
	defs := BuiltinTools(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	if len(defs) != len(goldenBuiltinOrder) {
		t.Fatalf("builtin count=%d, want %d", len(defs), len(goldenBuiltinOrder))
	}
	for i, want := range goldenBuiltinOrder {
		if defs[i].Name != want {
			t.Fatalf("builtin[%d]=%q, want %q", i, defs[i].Name, want)
		}
	}
}

func TestBuiltinTools_KeyWiring(t *testing.T) {
	byName := map[string]ToolDefinition{}
	for _, d := range BuiltinTools(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil) {
		byName[d.Name] = d
	}

	cases := []struct {
		name    string
		risk    RiskLevel
		effect  Effect
		preview bool
	}{
		{"bash", RiskDanger, EffectExecuteProcess, false},
		{"write_file", RiskDanger, EffectWriteFile, true},
		{"edit_file", RiskDanger, EffectWriteFile, true},
		{"insert_file", RiskDanger, EffectWriteFile, true},
		{"delete_file", RiskDanger, EffectDeleteFile, true},
		{"read_file", RiskAuto, EffectReadFile, false},
		{"web_fetch", RiskSafe, EffectNetworkAccess, false},
		{"web_search", RiskSafe, EffectNetworkAccess, false},
		{"explore", RiskSafe, 0, false},
		{"plan_approval", RiskAuto, EffectTeamMutation, false},
	}
	for _, tc := range cases {
		d, ok := byName[tc.name]
		if !ok {
			t.Fatalf("missing tool %q", tc.name)
		}
		if d.RiskLevel != tc.risk {
			t.Fatalf("%s RiskLevel=%v, want %v", tc.name, d.RiskLevel, tc.risk)
		}
		if tc.effect != 0 && !d.HasEffect(tc.effect) {
			t.Fatalf("%s missing effect %v", tc.name, tc.effect)
		}
		if (d.Preview != nil) != tc.preview {
			t.Fatalf("%s Preview=%v, want %v", tc.name, d.Preview != nil, tc.preview)
		}
	}
}
