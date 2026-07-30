package agent

import (
	"fmt"
	"go-code-agent/internal/config"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/prompt"
	"go-code-agent/internal/tool"
)

// beforeDecision controls how executeToolBatch reacts to a Before result.
type beforeDecision int

const (
	beforeContinue beforeDecision = iota
	// beforeDenyEarly skips execution and the full after-hook path
	// (only failure counters + ToolFinished are recorded).
	beforeDenyEarly
	// beforeOverride skips execution but still runs after-hooks
	// (repeat/explore-budget failures share this path with real results).
	beforeOverride
)

type beforeResult struct {
	decision beforeDecision
	result   tool.Result
	nudges   []string
}

type afterResult struct {
	nudges         []string
	manualCompress bool
}

// toolCallEnv is the per-call view interceptors may read/mutate.
type toolCallEnv struct {
	role string
	turn *turnState
	key  string // tc.Name + "\x00" + tc.Arguments
}

// toolCallInterceptor is a single Before/After hook in the tool batch pipeline.
type toolCallInterceptor interface {
	Before(env *toolCallEnv, tc llm.ToolCall) beforeResult
	After(env *toolCallEnv, tc llm.ToolCall, result tool.Result) afterResult
}

// defaultToolInterceptors is the ordered lead/explore batch chain.
func defaultToolInterceptors(loader *prompt.Loader) []toolCallInterceptor {
	postExplore := prompt.Render(loader.MustLoad("post_explore"), map[string]string{})
	return []toolCallInterceptor{
		readConvergenceInterceptor{},
		postExploreInterceptor{},
		repeatCallInterceptor{},
		exploreBudgetInterceptor{},
		failureTrackerInterceptor{},
		turnFlagsInterceptor{postExploreNudge: postExplore},
	}
}

// --- read / list convergence ---

type readConvergenceInterceptor struct{}

func (readConvergenceInterceptor) Before(env *toolCallEnv, tc llm.ToolCall) beforeResult {
	if tc.Name != "read_file" && tc.Name != "list_dir" {
		return beforeResult{}
	}
	filePath := extractFilePath(tc.Arguments)
	if filePath == "" {
		return beforeResult{}
	}
	n := env.turn.explore.notePathRead(filePath)
	var nudges []string
	if env.role == "explore" {
		if n == 2 {
			nudges = append(nudges,
				"<convergence-nudge>You already read/list-dir '"+filePath+
					"'. Do NOT re-read it — use the prior result, or "+
					"search_content for a specific fact.</convergence-nudge>")
		}
		if n >= 3 {
			return beforeResult{
				decision: beforeDenyEarly,
				result: tool.Failed(fmt.Sprintf(
					"repeated %s of %q blocked; use the earlier result or search_content", tc.Name, filePath)),
				nudges: nudges,
			}
		}
		return beforeResult{nudges: nudges}
	}
	if n == 3 {
		nudges = append(nudges,
			"<convergence-nudge>You have read/list-dir '"+filePath+
				"' 3 times. STOP re-reading it. "+
				"Either you have enough information already, or "+
				"you need a different approach (grep/search_content for specifics, "+
				"or delegate to explore).</convergence-nudge>")
	}
	return beforeResult{nudges: nudges}
}

func (readConvergenceInterceptor) After(*toolCallEnv, llm.ToolCall, tool.Result) afterResult {
	return afterResult{}
}

// --- post-explore budget ---

type postExploreInterceptor struct{}

func (postExploreInterceptor) Before(env *toolCallEnv, tc llm.ToolCall) beforeResult {
	blocked, why := postExploreBlockTurn(env.turn, env.role, tc)
	if !blocked {
		return beforeResult{}
	}
	return beforeResult{
		decision: beforeDenyEarly,
		result:   tool.Failed(why),
	}
}

func (postExploreInterceptor) After(*toolCallEnv, llm.ToolCall, tool.Result) afterResult {
	return afterResult{}
}

// postExploreBlockTurn is the turn-level form of Runner.postExploreBlock.
func postExploreBlockTurn(turn *turnState, role string, tc llm.ToolCall) (bool, string) {
	if !turn.explore.Succeeded || role == "explore" {
		return false, ""
	}
	switch tc.Name {
	case "read_file", "list_dir":
		if turn.explore.ReadsAfter >= config.MaxReadsAfterExplore {
			return true, fmt.Sprintf(
				"post-explore read budget exhausted (%d/%d); synthesize from the explore summary. "+
					"If one fact is missing, use search_content/search_file instead of more read_file/list_dir.",
				config.MaxReadsAfterExplore, config.MaxReadsAfterExplore)
		}
	case "bash":
		if isRepoWalkBash(extractBashCommand(tc.Arguments)) {
			return true, "post-explore repo walk via bash blocked (find/ls -R/tree); " +
				"synthesize from the explore summary or use search_content/search_file for a specific fact"
		}
	}
	return false, ""
}

// --- identical-call repeat guard ---

type repeatCallInterceptor struct{}

func (repeatCallInterceptor) Before(env *toolCallEnv, tc llm.ToolCall) beforeResult {
	if env.turn.tools.count(env.key) <= config.MaxRepeatedToolCalls {
		return beforeResult{}
	}
	return beforeResult{
		decision: beforeOverride,
		result: tool.Failed(fmt.Sprintf(
			"repeated tool call blocked: %s. Use a different path, offset, limit, or query.", tc.Name)),
	}
}

func (repeatCallInterceptor) After(*toolCallEnv, llm.ToolCall, tool.Result) afterResult {
	return afterResult{}
}

// --- explore delegation budget ---

type exploreBudgetInterceptor struct{}

func (exploreBudgetInterceptor) Before(env *toolCallEnv, tc llm.ToolCall) beforeResult {
	if tc.Name != "explore" {
		return beforeResult{}
	}
	if env.turn.explore.noteDelegation() <= config.MaxExploreDelegations {
		return beforeResult{}
	}
	return beforeResult{
		decision: beforeOverride,
		result:   tool.Failed("explore delegation budget exhausted for this turn; synthesize the findings already collected"),
	}
}

func (exploreBudgetInterceptor) After(*toolCallEnv, llm.ToolCall, tool.Result) afterResult {
	return afterResult{}
}

// --- failure streak ---

type failureTrackerInterceptor struct{}

func (failureTrackerInterceptor) Before(*toolCallEnv, llm.ToolCall) beforeResult {
	return beforeResult{}
}

func (failureTrackerInterceptor) After(env *toolCallEnv, tc llm.ToolCall, result tool.Result) afterResult {
	env.turn.failure.noteResult(tc.Name, result.Succeeded())
	return afterResult{}
}

// --- planning / explore / compress flags ---

type turnFlagsInterceptor struct {
	postExploreNudge string
}

func (i turnFlagsInterceptor) Before(*toolCallEnv, llm.ToolCall) beforeResult {
	return beforeResult{}
}

func (i turnFlagsInterceptor) After(env *toolCallEnv, tc llm.ToolCall, result tool.Result) afterResult {
	var out afterResult
	switch tc.Name {
	case "explore":
		if result.Succeeded() {
			env.turn.explore.noteSuccess()
			out.nudges = append(out.nudges, i.postExploreNudge)
		} else {
			env.turn.explore.Used = true
		}
	case "TodoWrite":
		if result.Succeeded() {
			env.turn.planning.noteTodoWrite(tc.Arguments)
		}
	case "task_create":
		if result.Succeeded() {
			env.turn.planning.establishPlan()
		}
	case "compress":
		out.manualCompress = true
	}

	if tc.Name == "read_file" || tc.Name == "list_dir" {
		env.turn.explore.noteLeadRead(env.role)
	}
	if tc.Name != "TodoWrite" {
		env.turn.planning.noteNonTodoTool()
	}
	return out
}
