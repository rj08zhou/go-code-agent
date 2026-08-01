package agent

import (
	"strings"
	"testing"

	"go-code-agent/internal/config"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/tool"
)

func newEnv(role string) *toolCallEnv {
	turn := newTurnState()
	return &toolCallEnv{role: role, turn: &turn, key: ""}
}

func TestReadConvergenceInterceptor_ExploreHardBlock(t *testing.T) {
	h := readConvergenceInterceptor{}
	env := newEnv("explore")
	tc := llm.ToolCall{Name: "read_file", Arguments: `{"path":"a.go"}`}

	for i := 1; i <= 2; i++ {
		env.key = tc.Name + "\x00" + tc.Arguments
		br := h.Before(env, tc)
		if br.decision != beforeContinue {
			t.Fatalf("call %d: decision=%v", i, br.decision)
		}
		if i == 2 && (len(br.nudges) != 1 || !strings.Contains(br.nudges[0], "convergence-nudge")) {
			t.Fatalf("call 2 want nudge, got %#v", br.nudges)
		}
	}
	br := h.Before(env, tc)
	if br.decision != beforeDenyEarly {
		t.Fatalf("call 3: decision=%v", br.decision)
	}
	if !strings.Contains(br.result.Output, "blocked") {
		t.Fatalf("result = %q", br.result.Output)
	}
}

func TestReadConvergenceInterceptor_LeadNudgeOnly(t *testing.T) {
	h := readConvergenceInterceptor{}
	env := newEnv("lead")
	tc := llm.ToolCall{Name: "read_file", Arguments: `{"path":"a.go"}`}
	for i := 0; i < 3; i++ {
		br := h.Before(env, tc)
		if br.decision != beforeContinue {
			t.Fatalf("call %d denied: %v", i+1, br.decision)
		}
		if i == 2 && len(br.nudges) != 1 {
			t.Fatalf("3rd lead read should nudge, got %#v", br.nudges)
		}
	}
}

func TestPostExploreInterceptor_BlocksReadsAndRepoWalk(t *testing.T) {
	h := postExploreInterceptor{}
	env := newEnv("lead")
	env.turn.explore.Succeeded = true
	env.turn.explore.ReadsAfter = config.MaxReadsAfterExplore

	br := h.Before(env, llm.ToolCall{Name: "read_file", Arguments: `{"path":"x.go"}`})
	if br.decision != beforeDenyEarly || !strings.Contains(br.result.Output, "post-explore read budget") {
		t.Fatalf("read block: %#v", br)
	}

	env.turn.explore.ReadsAfter = 0
	br = h.Before(env, llm.ToolCall{Name: "bash", Arguments: `{"command":"find . -name '*.go'"}`})
	if br.decision != beforeDenyEarly || !strings.Contains(br.result.Output, "repo walk") {
		t.Fatalf("bash block: %#v %q", br.decision, br.result.Output)
	}

	env.role = "explore"
	br = h.Before(env, llm.ToolCall{Name: "read_file", Arguments: `{"path":"x.go"}`})
	if br.decision != beforeContinue {
		t.Fatal("explore role must not be post-explore gated")
	}
}

func TestRepeatCallInterceptor(t *testing.T) {
	h := repeatCallInterceptor{}
	env := newEnv("lead")
	tc := llm.ToolCall{Name: "noop", Arguments: `{}`}
	env.key = tc.Name + "\x00" + tc.Arguments

	for i := 1; i <= config.MaxRepeatedToolCalls; i++ {
		env.turn.tools.CallCounts[env.key] = i
		if br := h.Before(env, tc); br.decision != beforeContinue {
			t.Fatalf("count=%d should continue", i)
		}
	}
	env.turn.tools.CallCounts[env.key] = config.MaxRepeatedToolCalls + 1
	br := h.Before(env, tc)
	if br.decision != beforeOverride || !strings.Contains(br.result.Output, "repeated tool call blocked") {
		t.Fatalf("got %#v", br)
	}
}

func TestExploreBudgetInterceptor(t *testing.T) {
	h := exploreBudgetInterceptor{}
	env := newEnv("lead")
	tc := llm.ToolCall{Name: "explore", Arguments: `{"prompt":"x"}`}

	for i := 1; i <= config.MaxExploreDelegations; i++ {
		br := h.Before(env, tc)
		if br.decision != beforeContinue {
			t.Fatalf("delegation %d blocked", i)
		}
		if env.turn.explore.Delegations != i {
			t.Fatalf("delegations=%d want %d", env.turn.explore.Delegations, i)
		}
	}
	br := h.Before(env, tc)
	if br.decision != beforeOverride || !strings.Contains(br.result.Output, "explore delegation budget") {
		t.Fatalf("got %#v", br)
	}
}

func TestFailureTrackerInterceptor(t *testing.T) {
	h := failureTrackerInterceptor{}
	env := newEnv("lead")
	tc := llm.ToolCall{Name: "bash"}

	h.After(env, tc, tool.Failed("boom"))
	if env.turn.failure.Consecutive != 1 || env.turn.failure.LastTool != "bash" {
		t.Fatalf("after fail: %+v", env.turn)
	}
	h.After(env, tc, tool.Failed("boom2"))
	if env.turn.failure.Consecutive != 2 {
		t.Fatalf("consecutive=%d", env.turn.failure.Consecutive)
	}
	h.After(env, tc, tool.Succeeded("ok"))
	if env.turn.failure.Consecutive != 0 || env.turn.failure.LastTool != "" || env.turn.failure.RoundsSinceComplete != 1 {
		t.Fatalf("after success: fails=%d last=%q rounds=%d",
			env.turn.failure.Consecutive, env.turn.failure.LastTool, env.turn.failure.RoundsSinceComplete)
	}
}

func TestTurnFlagsInterceptor(t *testing.T) {
	h := turnFlagsInterceptor{}
	env := newEnv("lead")

	ar := h.After(env, llm.ToolCall{Name: "explore", Arguments: `{}`}, tool.Succeeded("summary"))
	if !env.turn.explore.Used || !env.turn.explore.Succeeded || len(ar.nudges) != 1 {
		t.Fatalf("explore flags: used=%v ok=%v nudges=%v", env.turn.explore.Used, env.turn.explore.Succeeded, ar.nudges)
	}
	h.After(env, llm.ToolCall{Name: "read_file", Arguments: `{"path":"a.go"}`}, tool.Succeeded("x"))
	if env.turn.explore.ReadsAfter != 1 {
		t.Fatalf("readsAfterExplore=%d", env.turn.explore.ReadsAfter)
	}

	ar = h.After(env, llm.ToolCall{Name: "compress", Arguments: `{}`}, tool.Succeeded("ok"))
	if !ar.manualCompress {
		t.Fatal("expected manualCompress")
	}

	h.After(env, llm.ToolCall{
		Name:      "TodoWrite",
		Arguments: `{"items":[{"content":"a","status":"pending"}]}`,
	}, tool.Succeeded("ok"))
	if env.turn.planning.RoundsWithoutTodo != 0 || !env.turn.planning.HasOpenItems || !env.turn.planning.PlanEstablished {
		t.Fatalf("todo flags: rounds=%d open=%v established=%v",
			env.turn.planning.RoundsWithoutTodo, env.turn.planning.HasOpenItems, env.turn.planning.PlanEstablished)
	}
}

func TestTurnFlagsInterceptorRejectsEmptyOrCompletedTodoAsPlan(t *testing.T) {
	h := turnFlagsInterceptor{}
	for _, args := range []string{
		`{"items":[]}`,
		`{"items":[{"content":"done","status":"completed","activeForm":"finishing"}]}`,
	} {
		env := newEnv("lead")
		h.After(env, llm.ToolCall{Name: "TodoWrite", Arguments: args}, tool.Succeeded("ok"))
		if env.turn.planning.PlanEstablished {
			t.Errorf("TodoWrite %s established an empty plan", args)
		}
	}
}

func TestTurnFlagsInterceptorEstablishesPlanOnlyAfterSuccessfulMutation(t *testing.T) {
	h := turnFlagsInterceptor{}
	planArgs := `{"items":[{"content":"a","status":"pending"}]}`

	for _, tc := range []llm.ToolCall{
		{Name: "TodoWrite", Arguments: planArgs},
		{Name: "task_create", Arguments: `{"title":"a"}`},
	} {
		env := newEnv("lead")
		h.After(env, tc, tool.Failed("no"))
		if env.turn.planning.PlanEstablished {
			t.Errorf("failed %s established a plan", tc.Name)
		}
		h.After(env, tc, tool.Succeeded("ok"))
		if !env.turn.planning.PlanEstablished {
			t.Errorf("successful %s did not establish a plan", tc.Name)
		}
	}

	for _, name := range []string{"task_list", "task_dag", "task_ready"} {
		env := newEnv("lead")
		h.After(env, llm.ToolCall{Name: name, Arguments: `{}`}, tool.Succeeded("ok"))
		if env.turn.planning.PlanEstablished {
			t.Errorf("query tool %s established a plan", name)
		}
	}
}
